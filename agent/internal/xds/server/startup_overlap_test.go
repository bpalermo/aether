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

// TestPreListen_LoadsLocalListenersWhileIdentityIsPending is issue #1123's
// startup-overlap term. Building the local pods' listeners from storage needs
// nothing from SPIRE (the trust domain is the seeded one, and a different one
// is folded in later by reconcileSpireIdentity), yet it ran strictly AFTER the
// identity hold: 0.25-0.98 s per restart on the 2026-10-02 reference-cluster agent rolls,
// serially behind an SVID wait of 0.15-2.0 s. Both sit inside the window in
// which the node's proxy has no ADS stream.
//
// The listeners are now built during the hold. What the hold protects is
// unchanged and still asserted: the registry is not consulted and PreListen
// does not return (so the socket does not open) until identity exists.
func TestPreListen_LoadsLocalListenersWhileIdentityIsPending(t *testing.T) {
	var storageReads atomic.Int64
	var registryCalls atomic.Int64
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			registryCalls.Add(1)
			return nil, errors.New("registry unavailable")
		},
	}
	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		storageReads.Add(1)
		return []*cniv1.CNIPod{}, nil
	})
	snapshotCache := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", "example.org", reg, store, snapshotCache, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	gate := newFakeIdentity()
	srv.SetIdentityGate(gate)

	done := make(chan error, 1)
	go func() { done <- srv.PreListen(t.Context()) }()

	require.Eventually(t, func() bool { return storageReads.Load() > 0 }, 5*time.Second, 5*time.Millisecond,
		"the local listeners must be built while the identity hold is still in force, not after it")
	select {
	case err := <-done:
		t.Fatalf("PreListen returned while identity was pending (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	assert.Zero(t, registryCalls.Load(), "the registry must still not be consulted before identity exists")

	gate.arrive()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("PreListen did not proceed after identity arrived")
	}
	assert.Equal(t, int64(1), storageReads.Load(), "the listeners are built once, not again after the hold")
	assert.Positive(t, registryCalls.Load())
}
