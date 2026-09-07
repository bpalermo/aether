package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
)

// startingAdder stands in for the controller-runtime manager: it records what
// was registered and starts it, exactly as the real manager does at m.Start.
// Starting it is what makes the timing assertion meaningful — the retry loop is
// genuinely running while startSpireIdentity's caller carries on.
type startingAdder struct {
	ctx context.Context

	mu    sync.Mutex
	added []ctrlmanager.Runnable
	done  chan error
}

func (a *startingAdder) Add(r ctrlmanager.Runnable) error {
	a.mu.Lock()
	a.added = append(a.added, r)
	a.mu.Unlock()

	a.done = make(chan error, 1)
	go func() { a.done <- r.Start(a.ctx) }()
	return nil
}

func (a *startingAdder) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.added)
}

// withSpireConfig sets the SPIRE-related globals for one test and restores them.
func withSpireConfig(t *testing.T, enabled bool, socket string, logger *slog.Logger) {
	t.Helper()

	prevEnabled, prevSocket, prevMesh, prevWarn, prevLog := cfg.SpireEnabled, cfg.SpireWorkloadSocketPath, cfg.MeshDomain, cfg.SpireWaitWarnAfter, l
	t.Cleanup(func() {
		cfg.SpireEnabled, cfg.SpireWorkloadSocketPath, cfg.MeshDomain, cfg.SpireWaitWarnAfter, l = prevEnabled, prevSocket, prevMesh, prevWarn, prevLog
	})

	cfg.SpireEnabled = enabled
	cfg.SpireWorkloadSocketPath = socket
	cfg.MeshDomain = "aether.internal"
	cfg.SpireWaitWarnAfter = time.Minute
	l = logger
}

// TestStartSpireIdentityReturnsImmediately is the regression test for #740 at
// the call site. This slot used to block for up to 25s against an unreachable
// Workload API and then EXIT THE PROCESS — before the manager was started, so
// /healthz and /readyz were still silent and the kubelet was already counting.
// It must now come back at once, with the retry loop running behind it.
func TestStartSpireIdentityReturnsImmediately(t *testing.T) {
	dir, err := os.MkdirTemp("", "spire-identity")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	withSpireConfig(t, true, filepath.Join(dir, "absent.sock"), slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	adder := &startingAdder{ctx: ctx}

	start := time.Now()
	src, trustDomain, err := startSpireIdentity(ctx, adder)
	elapsed := time.Since(start)

	require.NoError(t, err, "an unreachable Workload API must not fail startup")
	require.NotNil(t, src)
	assert.Less(t, elapsed, 100*time.Millisecond, "startup must not wait for SPIRE")
	assert.Equal(t, 1, adder.count(), "the identity source must be registered as a runnable")
	assert.Equal(t, "aether.internal", trustDomain.Get(), "the trust domain is seeded with the mesh domain")
	assert.False(t, src.HasSVID(), "no SVID can exist against a socket nobody serves")

	// And the runnable it registered keeps waiting rather than failing.
	cancel()
	select {
	case runErr := <-adder.done:
		require.NoError(t, runErr, "the identity runnable must never fail the manager")
	case <-time.After(30 * time.Second):
		t.Fatal("the identity runnable did not return after cancellation")
	}
}

// TestStartSpireIdentityDisabledIsUnchanged pins the #421 cleartext path: with
// --spire-enabled=false nothing is registered, nothing is logged, and the trust
// domain is the mesh domain — byte for byte the old behaviour.
func TestStartSpireIdentityDisabledIsUnchanged(t *testing.T) {
	logs := &bytes.Buffer{}
	withSpireConfig(t, false, "/nonexistent/workload.sock",
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	adder := &startingAdder{ctx: t.Context()}
	src, trustDomain, err := startSpireIdentity(t.Context(), adder)

	require.NoError(t, err)
	assert.Nil(t, src, "SPIRE off must create no source at all")
	assert.Equal(t, "aether.internal", trustDomain.Get())
	assert.Equal(t, 0, adder.count(), "SPIRE off must register no runnable")
	assert.Empty(t, logs.String(), "SPIRE off must log nothing")
}

// TestReconcileSpireIdentityDisabledIsANoOp keeps the same promise for the
// post-arrival half: with no source there is nothing to wait on, and no
// goroutine is left behind holding a nil cache.
func TestReconcileSpireIdentityDisabledIsANoOp(t *testing.T) {
	withSpireConfig(t, false, "/nonexistent/workload.sock", slog.New(slog.DiscardHandler))

	// nil cache and nil storage on purpose: reaching either would panic, which
	// is the assertion.
	reconcileSpireIdentity(t.Context(), nil, nil, nil, nil)
}
