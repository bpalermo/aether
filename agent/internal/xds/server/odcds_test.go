package server

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnDemandObserver_RecordsNamedCDSSubscriptions verifies a named delta
// CDS subscription (the on_demand filter requesting an undistributed cluster)
// lands in the dependency set, while wildcard subscriptions, per-pod cluster
// names, and other type URLs are ignored.
func TestOnDemandObserver_RecordsNamedCDSSubscriptions(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", logr.Discard())
	o := newOnDemandObserver(c, logr.Discard())

	// Named CDS subscription: observed.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-on-demand"},
	}))
	assert.Contains(t, c.DependencySet(), "svc-on-demand")

	// Wildcard and per-pod names: ignored.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"*", "", "app_pod-1", "health_pod-1"},
	}))
	deps := c.DependencySet()
	assert.NotContains(t, deps, "*")
	assert.NotContains(t, deps, "app_pod-1")
	assert.NotContains(t, deps, "health_pod-1")

	// EDS subscriptions are named per cluster; they must not be observed.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.EndpointType,
		ResourceNamesSubscribe: []string{"svc-eds-sub"},
	}))
	assert.NotContains(t, c.DependencySet(), "svc-eds-sub")
}

// TestCombinedCallbacks_Dispatch verifies the combiner reaches every member.
func TestCombinedCallbacks_Dispatch(t *testing.T) {
	c1 := cache.NewSnapshotCache("node-1", logr.Discard())
	c2 := cache.NewSnapshotCache("node-1", logr.Discard())
	combined := combinedCallbacks{
		newOnDemandObserver(c1, logr.Discard()).Callbacks(),
		newOnDemandObserver(c2, logr.Discard()).Callbacks(),
	}

	require.NoError(t, combined.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-x"},
	}))
	assert.Contains(t, c1.DependencySet(), "svc-x")
	assert.Contains(t, c2.DependencySet(), "svc-x")

	// Unwired hooks are nil-safe.
	require.NoError(t, combined.OnStreamOpen(context.Background(), 1, resourcev3.ClusterType))
	combined.OnStreamClosed(1, nil)
}
