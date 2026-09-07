package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/common/spire"
	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
)

// startingManager stands in for the controller-runtime manager: it records what
// was registered, starts the runnables as m.Start would, and keeps the readiness
// checks so the test can run them. Starting the runnable is what makes the timing
// assertion meaningful — the retry loop is genuinely running while the caller
// carries on building the gRPC server.
type startingManager struct {
	ctx context.Context

	mu     sync.Mutex
	added  []ctrlmanager.Runnable
	checks map[string]healthz.Checker
	done   chan error
}

func newStartingManager(ctx context.Context) *startingManager {
	return &startingManager{ctx: ctx, checks: map[string]healthz.Checker{}, done: make(chan error, 1)}
}

func (m *startingManager) Add(r ctrlmanager.Runnable) error {
	m.mu.Lock()
	m.added = append(m.added, r)
	m.mu.Unlock()

	go func() { m.done <- r.Start(m.ctx) }()
	return nil
}

func (m *startingManager) AddReadyzCheck(name string, check healthz.Checker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks[name] = check
	return nil
}

func (m *startingManager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.added)
}

func (m *startingManager) check(name string) (healthz.Checker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.checks[name]
	return c, ok
}

// withSpireConfig sets the SPIRE-related globals for one test and restores them.
func withSpireConfig(t *testing.T, enabled bool, socket string, warnAfter time.Duration, logger *slog.Logger) {
	t.Helper()

	prevEnabled, prevSocket, prevWarn, prevLog := cfg.SpireEnabled, cfg.SpireWorkloadSocketPath, cfg.SpireWaitWarnAfter, l
	t.Cleanup(func() {
		cfg.SpireEnabled, cfg.SpireWorkloadSocketPath, cfg.SpireWaitWarnAfter, l = prevEnabled, prevSocket, prevWarn, prevLog
	})

	cfg.SpireEnabled = enabled
	cfg.SpireWorkloadSocketPath = socket
	cfg.SpireWaitWarnAfter = warnAfter
	l = logger
}

// TestBuildSpireGRPCCredsReturnsImmediately is the regression test for #740 at
// the registrar's call site. It used to block for up to 25s against an
// unreachable Workload API and then EXIT THE PROCESS, which took every agent's
// endpoint watch down with it for the length of the crash loop.
//
// The negative control is the code this replaces: spire.NewSource on the same
// unreachable socket returns only after spire.SourceTimeout (25s), with an error
// the caller propagated straight out of runRegistrar.
func TestBuildSpireGRPCCredsReturnsImmediately(t *testing.T) {
	withSpireConfig(t, true, spiretest.UnservedSocket(t), time.Minute, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newStartingManager(ctx)

	start := time.Now()
	opts, src, err := buildSpireGRPCCreds(ctx, m)
	elapsed := time.Since(start)

	require.NoError(t, err, "an unreachable Workload API must not fail startup")
	require.NotNil(t, src)
	t.Cleanup(func() { _ = src.Close() })
	assert.Less(t, elapsed, 100*time.Millisecond, "startup must not wait for SPIRE")
	assert.Len(t, opts, 1, "the gRPC server must still be given mTLS credentials")
	assert.Equal(t, 1, m.count(), "the identity source must be registered as a runnable")
	assert.False(t, src.HasSVID(), "no SVID can exist against a socket nobody serves")

	// And the runnable it registered keeps waiting rather than failing.
	cancel()
	select {
	case runErr := <-m.done:
		require.NoError(t, runErr, "the identity runnable must never fail the manager")
	case <-time.After(30 * time.Second):
		t.Fatal("the identity runnable did not return after cancellation")
	}
}

// TestBuildSpireGRPCCredsSpireDisabled pins the #421 path: with
// --spire-enabled=false the gRPC server is insecure, nothing is registered, and
// no readiness gate appears.
func TestBuildSpireGRPCCredsSpireDisabled(t *testing.T) {
	logs := &bytes.Buffer{}
	withSpireConfig(t, false, "/nonexistent/workload.sock", time.Minute,
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	m := newStartingManager(t.Context())
	opts, src, err := buildSpireGRPCCreds(t.Context(), m)

	require.NoError(t, err)
	assert.Nil(t, opts, "SPIRE off means insecure transport, unchanged")
	assert.Nil(t, src, "SPIRE off must create no source at all")
	assert.Equal(t, 0, m.count(), "SPIRE off must register no runnable")
	_, gated := m.check(spire.ReadyCheckName)
	assert.False(t, gated, "with no identity to wait for the check must disappear entirely")
	assert.Contains(t, logs.String(), "SPIRE disabled, gRPC server will use insecure transport")
}

// TestBuildSpireGRPCCredsReadiness is the registrar's readiness table. The gate is
// load-bearing here: NotReady removes this replica from the registrar Service's
// endpoints, so agents dial one that can actually complete a handshake instead of
// retrying against a registrar with no certificate to present.
//
// It has NO dwell (#740 PR 4), and that is the whole point of this test. On the
// rev210 upgrade roll (2026-09-07 20:03:45Z) this replica carried the agent's 2m
// dwell, so it was Ready — and therefore in the Service's endpoints — with no
// SVID; the agent on main-worker-01 dialled it and got
// `transport: authentication handshake failed: x509svid: could not get X509
// bundle`. The dwell belongs to the DaemonSet, whose NotReady arms a node taint;
// a Deployment behind a Service has no such coupling, and NotReady there is
// exactly the removal a pod that cannot handshake deserves.
func TestBuildSpireGRPCCredsReadiness(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	require.NoError(t, err)

	// An hour of warnAfter: the WARN escalation is a separate question from
	// readiness, and however long it is set to, a replica with no identity must
	// not stay in the endpoint set.
	withSpireConfig(t, true, spiretest.UnservedSocket(t), time.Hour, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newStartingManager(ctx)

	_, src, err := buildSpireGRPCCreds(ctx, m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	check, ok := m.check(spire.ReadyCheckName)
	require.True(t, ok, "the gate must be registered as %q", spire.ReadyCheckName)

	checkErr := check(req)
	require.Error(t, checkErr, "a registrar with no identity must leave its Service's endpoints at once")
	assert.Contains(t, checkErr.Error(), "no SPIRE SVID after")
}

// TestRegistrarTrustDomainIsLoggedOnArrival pins the post-arrival callback that
// replaced the startup trust-domain resolution: nothing is resolved (and nothing
// is authorized — the handshake fails first) until SPIRE issues the SVID, and when
// it does, the trust domain peers are authorized against is announced.
func TestRegistrarTrustDomainIsLoggedOnArrival(t *testing.T) {
	wlapi, sock := spiretest.Start(t, "spiffe://"+spiretest.TrustDomain+"/ns/aether-system/sa/aether-registrar")

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	withSpireConfig(t, true, sock, time.Hour, log)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newStartingManager(ctx)

	_, src, err := buildSpireGRPCCreds(ctx, m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	// While the Workload API refuses to attest, the source holds no SVID, so the
	// server can present no certificate and authorizes nothing.
	require.False(t, src.HasSVID())
	_, svidErr := src.GetX509SVID()
	require.ErrorIs(t, svidErr, spire.ErrNoSVIDYet)
	assert.NotContains(t, logs.String(), "resolved workload trust domain from SPIRE")

	wlapi.StartServing()

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "resolved workload trust domain from SPIRE")
	}, 30*time.Second, 20*time.Millisecond, "logs:\n%s", logs.String())
	assert.Contains(t, logs.String(), spiretest.TrustDomain)
}

// lockedBuffer is a concurrency-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
