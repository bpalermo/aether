package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	commonspire "aethermesh.dev/common/spire"
	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// gatingAdder is startingAdder plus the readiness registry, which is what the
// edge's identity wiring needs from the manager.
type gatingAdder struct {
	*startingAdder

	mu     sync.Mutex
	checks map[string]healthz.Checker
}

func newGatingAdder(ctx context.Context) *gatingAdder {
	return &gatingAdder{startingAdder: &startingAdder{ctx: ctx}, checks: map[string]healthz.Checker{}}
}

func (g *gatingAdder) AddReadyzCheck(name string, check healthz.Checker) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks[name] = check
	return nil
}

func (g *gatingAdder) check(name string) (healthz.Checker, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.checks[name]
	return c, ok
}

// gatingAdder must satisfy exactly what the production wiring asks for.
var _ edgeIdentityManager = (*gatingAdder)(nil)

// recordingSink records the identity the edge folds into its snapshot cache,
// standing in for the real SnapshotCache (whose UpdateEdgeIdentity recomputes the
// mTLS clusters and pushes a snapshot).
type recordingSink struct {
	mu       sync.Mutex
	calls    int
	spiffeID string
	td       string
}

func (s *recordingSink) UpdateEdgeIdentity(_ context.Context, spiffeID, trustDomain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.spiffeID, s.td = spiffeID, trustDomain
	return nil
}

func (s *recordingSink) get() (int, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.spiffeID, s.td
}

// TestResolveEdgeIdentityReturnsImmediately is the regression test for #740 at
// the edge's call site. It used to block for up to 25s against an unreachable
// Workload API and then EXIT THE PROCESS — on a north-south gateway, which means
// ingress stayed down for the whole crash loop.
//
// The negative control is the code this replaces: commonspire.NewSource on the
// same unreachable socket returns only after commonspire.SourceTimeout (25s).
func TestResolveEdgeIdentityReturnsImmediately(t *testing.T) {
	withSpireConfig(t, true, spiretest.UnservedSocket(t), slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newGatingAdder(ctx)

	start := time.Now()
	src, trustDomain, spiffeID, err := resolveEdgeIdentity(ctx, m)
	elapsed := time.Since(start)

	require.NoError(t, err, "an unreachable Workload API must not fail startup")
	require.NotNil(t, src)
	t.Cleanup(func() { _ = src.Close() })
	assert.Less(t, elapsed, 100*time.Millisecond, "startup must not wait for SPIRE")
	assert.Equal(t, "aether.internal", trustDomain, "the trust domain is seeded with the mesh domain")
	assert.Empty(t, spiffeID, "no identity is claimed before SPIRE issues one")
	assert.Equal(t, 1, m.count(), "the identity source must be registered as a runnable")

	// The readiness gate is registered and already failing: the edge takes NO
	// dwell (#740 PR 4). The agent's 2m dwell exists only because NotReady on the
	// agent DaemonSet arms a node taint; the edge is a Deployment behind the
	// MetalLB LoadBalancer, where NotReady means one replica leaves the ingress
	// endpoints and a second goes on serving — the correct treatment of a gateway
	// whose upstream clusters would carry no identity.
	req, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	require.NoError(t, err)
	check, ok := m.check(commonspire.ReadyCheckName)
	require.True(t, ok, "the gate must be registered as %q", commonspire.ReadyCheckName)
	gateErr := check(req)
	require.Error(t, gateErr, "an edge with no identity must leave the ingress endpoints at once")
	assert.Contains(t, gateErr.Error(), "no SPIRE SVID after")

	cancel()
	select {
	case runErr := <-m.done:
		require.NoError(t, runErr, "the identity runnable must never fail the manager")
	case <-time.After(30 * time.Second):
		t.Fatal("the identity runnable did not return after cancellation")
	}
}

// TestResolveEdgeIdentityGateIgnoresWarnAfter pins the separation PR 4 of #740
// introduced: the readiness dwell is no longer the wait's WARN threshold. A long
// --spire-wait-warn-after keeps the wait quiet in the log, but it must not keep
// an identity-less edge inside the LoadBalancer's endpoint set.
func TestResolveEdgeIdentityGateIgnoresWarnAfter(t *testing.T) {
	withSpireConfig(t, true, spiretest.UnservedSocket(t), slog.New(slog.DiscardHandler))
	cfg.SpireWaitWarnAfter = time.Hour // the log stays at INFO; readiness does not care

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newGatingAdder(ctx)

	src, _, _, err := resolveEdgeIdentity(ctx, m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	req, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	require.NoError(t, err)
	check, ok := m.check(commonspire.ReadyCheckName)
	require.True(t, ok)
	checkErr := check(req)
	require.Error(t, checkErr, "a long warn threshold must not keep an identity-less edge in the ingress endpoints")
	assert.Contains(t, checkErr.Error(), "no SPIRE SVID after")
}

// TestResolveEdgeIdentitySpireDisabled pins the #421 cleartext path: nothing is
// registered, nothing is logged, and the seeds are exactly what they always were.
func TestResolveEdgeIdentitySpireDisabled(t *testing.T) {
	logs := &bytes.Buffer{}
	withSpireConfig(t, false, "/nonexistent/workload.sock",
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	m := newGatingAdder(t.Context())
	src, trustDomain, spiffeID, err := resolveEdgeIdentity(t.Context(), m)

	require.NoError(t, err)
	assert.Nil(t, src, "SPIRE off must create no source at all")
	assert.Equal(t, "aether.internal", trustDomain)
	assert.Empty(t, spiffeID)
	assert.Equal(t, 0, m.count(), "SPIRE off must register no runnable")
	_, gated := m.check(commonspire.ReadyCheckName)
	assert.False(t, gated, "with no identity to wait for the check must disappear entirely")
	assert.Empty(t, logs.String(), "SPIRE off must log nothing")

	// And the post-arrival half is a no-op with no source: a nil sink would panic.
	wireEdgeIdentity(t.Context(), nil, nil)
}

// TestWireEdgeIdentityFoldsInTheSVID is the payoff: the edge starts with seeds,
// and the moment the Workload API issues its SVID the real SPIFFE ID and trust
// domain reach the snapshot cache — which recomputes the mTLS clusters and pushes
// a snapshot, so no restart is needed.
func TestWireEdgeIdentityFoldsInTheSVID(t *testing.T) {
	const edgeID = "spiffe://" + spiretest.TrustDomain + "/ns/aether-ingress/sa/aether-edge"

	wlapi, sock := spiretest.Start(t, edgeID)
	withSpireConfig(t, true, sock, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newGatingAdder(ctx)

	src, _, _, err := resolveEdgeIdentity(ctx, m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	sink := &recordingSink{}
	wireEdgeIdentity(ctx, src, sink)

	// Still refusing to attest: the cache keeps the seeds, so the edge serves
	// clusters with no mTLS injection rather than clusters naming an identity it
	// does not hold.
	require.Eventually(t, func() bool { return wlapi.Fetches() >= 1 }, 30*time.Second, 10*time.Millisecond,
		"the source must be attempting")
	calls, _, _ := sink.get()
	require.Equal(t, 0, calls, "no identity can be applied before SPIRE issues one")

	wlapi.StartServing()

	require.Eventually(t, func() bool {
		calls, _, _ := sink.get()
		return calls >= 1
	}, 30*time.Second, 20*time.Millisecond, "the edge identity must be applied once the SVID lands")

	calls, gotID, gotTD := sink.get()
	assert.Equal(t, 1, calls, "the identity is applied once, not polled")
	assert.Equal(t, edgeID, gotID)
	assert.Equal(t, spiretest.TrustDomain, gotTD)
	assert.Equal(t, int64(0), wlapi.BadHeaders(), "every call must carry the workload.spiffe.io header")
}
