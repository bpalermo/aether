package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestPreListen_SignalsTheStandbyMilestones: a surge standby (proposal 041)
// diffs its takeover against the local load, and reports Ready once the whole
// first serve is prepared. Both are signalled by PreListen, in that order, and
// the second only after every gate — here, the identity hold.
func TestPreListen_SignalsTheStandbyMilestones(t *testing.T) {
	reg := &mockRegistry{
		listAllEndpointsFunc: func(context.Context, registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	store := storage.NewMockStorageWithGetAll(func(context.Context) ([]*cniv1.CNIPod, error) { return nil, nil })
	sc := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", "example.org", reg, store, sc, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	gate := newFakeIdentity()
	srv.SetIdentityGate(gate)

	done := make(chan error, 1)
	go func() { done <- srv.PreListen(t.Context()) }()

	require.Eventually(t, func() bool { return isClosed(srv.LocalListenersLoaded()) }, 5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.False(t, isClosed(srv.FirstServeReady()), "not ready while a first-serve gate (identity) holds")

	gate.arrive()
	require.NoError(t, <-done)
	assert.True(t, isClosed(srv.FirstServeReady()))
}

// fakeOwnership is an ownership claim a test opens by hand.
type fakeOwnership struct {
	owned     chan struct{}
	contended bool
}

func (f *fakeOwnership) WaitOwned(ctx context.Context) error {
	select {
	case <-f.owned:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeOwnership) Contended() bool { return f.contended }

// TestSetOwnership_BindsOnlyOnceOwned: PreListen completes (the standby is
// Ready) but xds.sock is not bound until ownership, so the proxy keeps its
// stream to the agent that owns the node.
func TestSetOwnership_BindsOnlyOnceOwned(t *testing.T) {
	reg := &mockRegistry{
		listAllEndpointsFunc: func(context.Context, registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	store := storage.NewMockStorageWithGetAll(func(context.Context) ([]*cniv1.CNIPod, error) { return nil, nil })
	sc := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", "example.org", reg, store, sc, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	own := &fakeOwnership{owned: make(chan struct{}), contended: true}
	srv.SetOwnership(own)

	// The bind gate is the embedded server's; exercise it directly (Start would
	// bind the production socket path).
	gateDone := make(chan error, 1)
	require.NotNil(t, srv.ownedGate)
	go func() { gateDone <- srv.ownedGate(t.Context()) }()
	select {
	case err := <-gateDone:
		t.Fatalf("the bind gate opened before ownership (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(own.owned)
	select {
	case err := <-gateDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the bind gate did not open once owned")
	}
}
