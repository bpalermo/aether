package server

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIdentity is an IdentityGate whose readiness the test controls.
type fakeIdentity struct {
	ready chan struct{}
	has   atomic.Bool
}

func newFakeIdentity() *fakeIdentity {
	return &fakeIdentity{ready: make(chan struct{})}
}

func (f *fakeIdentity) HasSVID() bool          { return f.has.Load() }
func (f *fakeIdentity) Ready() <-chan struct{} { return f.ready }
func (f *fakeIdentity) arrive()                { f.has.Store(true); close(f.ready) }

// newHoldServer builds an xDS server whose registry always fails, so a PreListen
// that runs to completion necessarily takes the local-only fallback — which is
// precisely what must not happen while identity is pending.
func newHoldServer(t *testing.T, gate IdentityGate) (*AgentXdsServer, *atomic.Int64) {
	t.Helper()

	var registryCalls atomic.Int64
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			registryCalls.Add(1)
			return nil, errors.New("registry unavailable: no SPIRE SVID yet")
		},
	}
	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{}, nil
	})
	snapshotCache := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))

	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", "example.org", reg, store, snapshotCache, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	srv.SetIdentityGate(gate)
	return srv, &registryCalls
}

// TestPreListen_HoldsUntilIdentity is issue #740's finding 1. With SPIRE down, a
// restarted agent cannot handshake to the registrar, so the local-only fallback
// fires and the snapshot it publishes has no cross-node endpoints — which
// REPLACES the complete config Envoy was still happily serving. On
// main-worker-03 on 2026-09-07 that failed ~95% of the node's mesh probes for a
// whole 6m41s outage. The crash loop this replaced never did that, because a
// dying agent published nothing.
//
// So: while identity is pending, PreListen must not reach the registry at all,
// must not return, and must not open the socket.
func TestPreListen_HoldsUntilIdentity(t *testing.T) {
	gate := newFakeIdentity() // no SVID yet
	srv, registryCalls := newHoldServer(t, gate)

	done := make(chan error, 1)
	go func() { done <- srv.PreListen(t.Context()) }()

	select {
	case err := <-done:
		t.Fatalf("PreListen returned while identity was pending (err=%v): Envoy would be served a local-only snapshot", err)
	case <-time.After(250 * time.Millisecond):
	}
	assert.Zero(t, registryCalls.Load(), "the registry must not be consulted before identity exists")

	// Identity lands: the hold releases and the normal path runs, including the
	// local-only fallback it is allowed to take from here on.
	gate.arrive()

	select {
	case err := <-done:
		require.NoError(t, err, "a registry that is still unavailable must not fail startup")
	case <-time.After(30 * time.Second):
		t.Fatal("PreListen did not proceed after identity arrived")
	}
	assert.Positive(t, registryCalls.Load(), "with identity in hand PreListen must build the snapshot as before")
}

// TestPreListen_IdentityAlreadyHeld is the steady-state control: an agent that
// already has its SVID (every restart outside a SPIRE outage) must not pay so
// much as a scheduling hop for the gate.
func TestPreListen_IdentityAlreadyHeld(t *testing.T) {
	gate := newFakeIdentity()
	gate.arrive()
	srv, registryCalls := newHoldServer(t, gate)

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Positive(t, registryCalls.Load())
}

// TestPreListen_NoGateIsUnchanged pins the --spire-enabled=false path (#421) and
// the edge, which builds this server without a gate: no gate means no hold, byte
// for byte the pre-#740 behaviour including the local-only fallback.
func TestPreListen_NoGateIsUnchanged(t *testing.T) {
	srv, registryCalls := newHoldServer(t, nil)

	require.NoError(t, srv.PreListen(t.Context()))
	assert.Positive(t, registryCalls.Load())
}

// TestPreListen_HoldEndsOnShutdown proves the hold is not a way to wedge a
// shutdown: cancelling the context ends it, and without an error — a SPIRE
// outage must never be the reason a shutdown is reported as a failure.
func TestPreListen_HoldEndsOnShutdown(t *testing.T) {
	gate := newFakeIdentity()
	srv, registryCalls := newHoldServer(t, gate)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.PreListen(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the identity hold did not end when the context was cancelled")
	}
	assert.Zero(t, registryCalls.Load(), "a cancelled hold must not fall through to the snapshot build")
}
