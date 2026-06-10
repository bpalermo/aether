package cache

import (
	"context"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoveEndpointDoesNotMutateAliasedLoadAssignment is the R2 regression test
// (docs/proposals/002): load assignments handed to previous snapshots are
// marshaled by xDS server goroutines without holding clusterMu, so RemoveEndpoint
// must build a new load assignment rather than mutating the aliased one in place.
func TestRemoveEndpointDoesNotMutateAliasedLoadAssignment(t *testing.T) {
	c := newTestCache("node-1")

	ep1 := &endpointv3.LocalityLbEndpoints{}
	ep2 := &endpointv3.LocalityLbEndpoints{}
	c.clusters["svc"] = clusterEntry{
		cluster: &clusterv3.Cluster{Name: "svc"},
		loadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: "svc",
			Endpoints:   []*endpointv3.LocalityLbEndpoints{ep1, ep2},
		},
		endpoints: map[string]*endpointv3.LocalityLbEndpoints{
			"10.0.0.1": ep1,
			"10.0.0.2": ep2,
		},
	}

	// Simulate a previously published snapshot holding the current proto.
	aliased, ok := c.Endpoints("svc")[0].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok)
	require.Len(t, aliased.Endpoints, 2)

	require.NoError(t, c.RemoveEndpoint(context.Background(), "svc", "10.0.0.1"))

	assert.Len(t, aliased.Endpoints, 2, "load assignment aliased into a prior snapshot must not be mutated")

	current, ok := c.Endpoints("svc")[0].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok)
	assert.Len(t, current.Endpoints, 1, "current load assignment must reflect the removal")
	assert.NotSame(t, aliased, current, "removal must produce a new load assignment")
}
