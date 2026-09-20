package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateSnapshot_ZeroVhostsStillPublishesOutboundRoute is the regression
// gate for issue #817.
//
// The egress listener references out_http over RDS. When the agent had no
// registry-derived virtual hosts to publish — a local-only start, the fallback
// loadInitialRegistryConfig takes when the registry cannot serve endpoints
// within its budget — the snapshot omitted out_http entirely.
//
// Under delta ADS that is not "an empty route table", it is NO RESPONSE AT ALL:
// go-control-plane writes a delta response only when the subscription yields
// resources or removals, and a first-time subscriber to a name the snapshot has
// never carried yields neither, so the watch stays open and silent. Envoy's
// listener warms for the full initial_fetch_timeout, then activates with an
// unresolved route table and serves 404 NR route_not_found on everything.
//
// So the invariant is not "the snapshot has some routes" but "the snapshot
// carries out_http BY NAME, and that table is usable" — the on-demand catch-all
// is what makes a zero-vhost table absorb mesh-shaped authorities via ODCDS
// instead of dead-404ing them.
func TestGenerateSnapshot_ZeroVhostsStillPublishesOutboundRoute(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	ctx := context.Background()

	// A local pod, so there IS an egress listener with an out_http RDS
	// reference — but no registry load at all, so there are zero vhosts. This
	// is the shape a local-only start publishes.
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{makeCNIPod("pod-a", "default", "/proc/100/ns/net")}, nil
	})
	require.NoError(t, c.LoadListenersFromStorage(ctx, store, "example.org"))

	_, _, vhosts := c.clustersEndpointsAndVhosts()
	require.Empty(t, vhosts, "precondition: this snapshot must have nothing to publish")

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	routes := snap.GetResources(resourcev3.RouteType)
	require.Contains(t, routes, proxy.OutboundHTTPRouteName,
		"out_http must be published even with zero vhosts, or the egress listener's RDS never resolves (#817)")

	rc, ok := routes[proxy.OutboundHTTPRouteName].(*routev3.RouteConfiguration)
	require.True(t, ok, "out_http resource must be a RouteConfiguration")

	require.Len(t, rc.GetVirtualHosts(), 1, "zero service vhosts leaves exactly the catch-all floor")
	catchAll := rc.GetVirtualHosts()[0]
	assert.Equal(t, []string{"*"}, catchAll.GetDomains())

	// Liveness 200 (proposal 013 prober), mesh-shaped authority → ODCDS, 404.
	require.Len(t, catchAll.GetRoutes(), 3)
	assert.Equal(t, uint32(200), catchAll.GetRoutes()[0].GetDirectResponse().GetStatus(),
		"the egress liveness probe target lives on the catch-all, so it must survive a zero-vhost start")
	assert.Equal(t, ":authority", catchAll.GetRoutes()[1].GetRoute().GetClusterHeader(),
		"a mesh-shaped authority must still reach its cluster via ODCDS")
	assert.Equal(t, uint32(404), catchAll.GetRoutes()[2].GetDirectResponse().GetStatus())
}

// TestGenerateSnapshot_OutboundRouteSurvivesEmptyingTheRegistry covers the
// other direction of #817: a cache that HAD service vhosts and then lost them
// (every declared upstream leaves the node dependency set) must keep publishing
// out_http rather than dropping the resource. Dropping it would leave Envoy's
// last-delivered table in place today, but any listener created afterwards — a
// new pod, or a new epoch after a hot restart — would subscribe to a name the
// snapshot no longer carries, which is the silent-watch case above.
func TestGenerateSnapshot_OutboundRouteSurvivesEmptyingTheRegistry(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	ctx := context.Background()

	declareDeps(c, "echo")

	// A registry that serves a service the node depends on: out_http gets a
	// real vhost.
	full := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"echo": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", full))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	require.Greater(t, len(snap.GetResources(resourcev3.RouteType)), 0)

	// ...and then the node stops depending on anything (the last pod declaring
	// that upstream went away), so demand-scoped distribution empties the
	// cluster map and with it every service vhost.
	c.depMu.Lock()
	delete(c.podDeps, "test-netns")
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", full))

	_, _, vhosts := c.clustersEndpointsAndVhosts()
	require.Empty(t, vhosts, "precondition: the registry no longer yields any vhost")

	snap, err = c.GetSnapshot("node-1")
	require.NoError(t, err)
	require.Contains(t, snap.GetResources(resourcev3.RouteType), proxy.OutboundHTTPRouteName)
}
