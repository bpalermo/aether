package cache

import (
	"context"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snapshotClusterNames returns the cluster resource names in the node's current
// published snapshot — what the proxy's delta CDS stream can be answered with.
func snapshotClusterNames(t *testing.T, c *SnapshotCache) map[string]types.Resource {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	return snap.GetResources(resourcev3.ClusterType)
}

// TestODCDS_DefaultPortAuthorityIsAnsweredInOnePush is the #682 regression test.
//
// The ODCDS catch-all resolves its cluster from the request :authority, so a
// client dialing "echo.aether-test.aether.internal:18081" makes the on_demand
// filter ask for a cluster of exactly that name. Before the fix the agent only
// ever published the PORTLESS "echo.aether-test.aether.internal" for the default
// port, so that on-demand name could never be satisfied: Envoy parked it in its
// delta subscription state as "waiting for server" and deduped every later
// subscribe for it, so once the demand set shrank and the service's vhost went
// away, no ODCDS request reached the agent again and the authority stayed dead
// until the ADS stream reset (~5 minutes in vivo).
//
// The assertion that matters: after the service is dropped from the dependency
// set, ONE on-demand observation plus ONE reload must put the requested name
// back in the published snapshot — no stream reset, no second round trip.
func TestODCDS_DefaultPortAuthorityIsAnsweredInOnePush(t *testing.T) {
	const (
		service   = "aether-test/echo"
		fqdn      = "echo.aether-test.aether.internal"
		authority = fqdn + ":18081"
	)

	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	// A short TTL so the idle expiry that shrinks the demand set is testable.
	c.observedTTL = 30 * time.Millisecond

	reg := &catalogListerRegistry{
		mockRegistry: &mockRegistry{
			listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return map[string][]*registryv1.ServiceEndpoint{
					service: {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 18081)},
				}, nil
			},
		},
		known: map[string]bool{service: true},
	}

	// Cold path: the proxy's on-demand subscribe for the authority is observed,
	// and the next reload publishes BOTH the portless cluster and the authority.
	c.ObserveDependency(ctx, service)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	warm := snapshotClusterNames(t, c)
	require.Contains(t, warm, fqdn, "portless cluster is published")
	require.Contains(t, warm, authority, "the ODCDS name for the default port must resolve to a cluster")

	// Demand-set shrink (the outage trigger): the observed dependency ages out,
	// the service leaves the dependency set and the reload drops its clusters.
	time.Sleep(40 * time.Millisecond)
	c.PruneObservedDependencies()
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	dropped := snapshotClusterNames(t, c)
	require.NotContains(t, dropped, fqdn, "the dropped service leaves the snapshot")
	require.NotContains(t, dropped, authority)

	// The very next request re-issues the on-demand subscribe for the authority
	// on the SAME stream. One observation + one reload must answer it.
	c.ObserveDependency(ctx, service)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	repaired := snapshotClusterNames(t, c)
	assert.Contains(t, repaired, authority,
		"the just-dropped cluster must be republished under the requested on-demand name in one push")
	assert.Contains(t, repaired, fqdn)
}
