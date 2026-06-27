package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnDemandObserver_RecordsNamedCDSSubscriptions verifies a named delta
// CDS subscription (the on_demand filter requesting an undistributed cluster)
// lands in the dependency set, while wildcard subscriptions, per-pod cluster
// names, and other type URLs are ignored.
func TestOnDemandObserver_RecordsNamedCDSSubscriptions(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))

	// Named CDS subscription for a mesh authority: observed under the bare
	// service name (the suffix is the deterministic bridge between the
	// data-plane cluster name and the control-plane keys).
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-on-demand.team-a.aether.internal"},
	}))
	assert.Contains(t, c.DependencySet(), "team-a/svc-on-demand")

	// Wildcard, per-pod names, names outside the mesh domain, and nested
	// labels under it: all ignored.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"*", "", "app_pod-1", "health_pod-1", "svc-bare", "a.b.c.aether.internal", ".aether.internal"},
	}))
	deps := c.DependencySet()
	assert.Len(t, deps, 1, "only the mesh-authority subscription is observed")

	// EDS subscriptions are named per cluster; they must not be observed.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.EndpointType,
		ResourceNamesSubscribe: []string{"svc-eds-sub.aether.internal"},
	}))
	assert.NotContains(t, c.DependencySet(), "svc-eds-sub")
}

// TestCombinedCallbacks_Dispatch verifies the combiner reaches every member.
func TestCombinedCallbacks_Dispatch(t *testing.T) {
	c1 := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	c2 := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	combined := combinedCallbacks{
		newOnDemandObserver(c1, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks(),
		newOnDemandObserver(c2, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks(),
	}

	require.NoError(t, combined.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-x.team-a.aether.internal"},
	}))
	assert.Contains(t, c1.DependencySet(), "team-a/svc-x")
	assert.Contains(t, c2.DependencySet(), "team-a/svc-x")

	// Unwired hooks are nil-safe.
	require.NoError(t, combined.OnStreamOpen(context.Background(), 1, resourcev3.ClusterType))
	combined.OnStreamClosed(1, nil)
}

// catalogRegistry wraps mockRegistry with a fixed service catalog.
type catalogRegistry struct {
	*mockRegistry
	known map[string]bool
}

func (c *catalogRegistry) HasService(name string) bool { return c.known[name] }

// TestOnDemandObserver_CatalogGate verifies nonexistent services are rejected
// before touching the dependency set, while known services are observed.
func TestOnDemandObserver_CatalogGate(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	reg := &catalogRegistry{mockRegistry: &mockRegistry{}, known: map[string]bool{"team-a/svc-real": true}}
	o := newOnDemandObserver(c, reg, slog.New(slog.DiscardHandler))

	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-real.team-a.aether.internal", "svc-ghost.team-a.aether.internal"},
	}))
	deps := c.DependencySet()
	assert.Contains(t, deps, "team-a/svc-real", "catalog hit is observed")
	assert.NotContains(t, deps, "team-a/svc-ghost", "catalog miss never pollutes the dependency set")
}
