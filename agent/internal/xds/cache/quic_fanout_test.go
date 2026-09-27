package cache

import (
	"context"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quicFanoutCache is the shared fixture: two local pods of source-a and one of
// source-b in "demo", the node identity served, capture on, and the given
// registry listing loaded with every listed service in the dependency set.
func quicFanoutCache(t *testing.T, waypoint bool, endpoints map[string][]*registryv1.ServiceEndpoint) *SnapshotCache {
	t.Helper()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetWaypointConfig(waypoint, proxy.DefaultEastWestTunnelPort)
	ctx := context.Background()
	for _, p := range []struct{ name, sa string }{{"a-0", "source-a"}, {"a-1", "source-a"}, {"b-0", "source-b"}} {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: p.name, Namespace: "demo", ServiceAccount: p.sa,
			NetworkNamespace: "/var/run/netns/cni-" + p.name,
		}, quicTestTD))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	authorities := map[string]string{}
	services := make([]string, 0, len(endpoints))
	for svc := range endpoints {
		ref := strings.SplitN(svc, "/", 2)
		authorities[svc] = ref[1] + "." + ref[0] + ".svc.cluster.local"
		services = append(services, svc)
	}
	c.SetCaptureAuthorities(authorities)
	declareDeps(c, services...)
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return endpoints, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	return c
}

const quicTestTD = "aether.internal"

// quicTwinsBySvc groups the snapshot's quic: twins by the h2 authority they
// clone, and asserts every twin's transport shape on the way.
func quicTwinsBySvc(t *testing.T, clusters map[string]types.Resource, meshDomain string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for name, res := range clusters {
		if !strings.HasPrefix(name, "quic:") {
			continue
		}
		authority, source, ok := strings.Cut(strings.TrimPrefix(name, "quic:"), "@")
		require.True(t, ok, "twin %s has no @<source> suffix", name)
		cl := res.(*clusterv3.Cluster)
		ts := cl.GetTransportSocket()
		require.True(t, ts.GetTypedConfig().MessageIs(&quicv3.QuicUpstreamTransport{}), "%s must carry a QUIC upstream transport", name)
		svc, ok := proxy.ServiceFromClusterName(authority, meshDomain)
		require.True(t, ok, "twin %s does not clone a mesh authority", name)
		assert.Equal(t, svc+"@"+source, cl.GetAltStatName(), "%s: per-source stats key (aether#960)", name)
		qt := &quicv3.QuicUpstreamTransport{}
		require.NoError(t, ts.GetTypedConfig().UnmarshalTo(qt))
		assert.Equal(t, "8080."+authority, qt.GetUpstreamTlsContext().GetSni(), "%s: SNI is <port>.<authority>, never the bare port (aether#957)", name)
		out[authority] = append(out[authority], name)
	}
	return out
}

// TestQUICFanoutPublishesPerSourceTwins is proposal 038 Phase 4b end to end
// in the cache, now that east-west QUIC is unconditional: with two
// destinations in the dependency set and two local ServiceAccounts, EVERY
// destination gets one `quic:` twin per SA (QUIC transport, HTTP/3) and BOTH
// route tables' vhost for each selects between them by source identity with
// the h2 cluster as on_no_match.
//
// "demo/other" is the anti-vacuity case for dropping the allow-list: under
// the old predicate (twins only for allow-listed destinations, here
// "demo/echo") it got nothing, so this test is red on that predicate.
func TestQUICFanoutPublishesPerSourceTwins(t *testing.T) {
	c := quicFanoutCache(t, false, map[string][]*registryv1.ServiceEndpoint{
		"demo/echo":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
		"demo/other": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080)},
	})
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	wantArms := map[string]map[string]string{}
	for _, svc := range []string{"demo/echo", "demo/other"} {
		h2 := proxy.ServiceClusterName(svc, c.meshDomain)
		require.Contains(t, clusters, h2, "the h2 cluster must exist")
		arms := map[string]string{}
		for _, sa := range []string{"source-a", "source-b"} {
			twin := proxy.QUICClusterName(svc, c.meshDomain, "demo/"+sa)
			_, ok := clusters[twin]
			require.True(t, ok, "missing twin %s in %v", twin, keysOf(clusters))
			arms["spiffe://"+quicTestTD+"/ns/demo/sa/"+sa] = twin
		}
		wantArms[h2] = arms
	}
	twins := quicTwinsBySvc(t, clusters, c.meshDomain)
	assert.Len(t, twins, 2, "both destinations get twins -- a service nobody listed included: %v", twins)
	for authority, names := range twins {
		// Exactly one per SA: the two pods of source-a share one, and the
		// per-port / alias entries (the :18081 mesh-port alias exists here)
		// never get their own.
		assert.Len(t, names, 2, "%s: one twin per local ServiceAccount, default entry only: %v", authority, names)
	}

	// Both route tables select by source identity on BOTH destinations' vhosts.
	selecting := map[string]int{}
	for name, res := range snap.GetResources(resourcev3.RouteType) {
		rc := res.(*routev3.RouteConfiguration)
		for _, vh := range rc.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				arms, noMatch, ok := proxy.QUICSelectionArms(r)
				if !ok {
					continue
				}
				want, known := wantArms[noMatch]
				require.True(t, known, "route table %s vhost %s: on_no_match %q is not an h2 service cluster", name, vh.GetName(), noMatch)
				assert.Equal(t, want, arms, "route table %s vhost %s", name, vh.GetName())
				selecting[noMatch]++
			}
		}
	}
	for h2 := range wantArms {
		assert.GreaterOrEqual(t, selecting[h2], 2, "%s: the vhost on BOTH out_http and cap_http must carry the selection plugin", h2)
	}
}

// TestQUICFanoutSkipsWaypointedServices: with the east/west waypoint on, a
// service that has an endpoint in another cluster (dialed at that node's
// tunnel, waypoint-tagged) stays h2 -- no twin, no selection -- because a twin
// shares the EDS resource but not the waypoint transport-socket matcher, and
// the tunnel has no QUIC leg. A local-only service on the same node still gets
// its twins, so the skip is per service, not a node-wide off switch.
func TestQUICFanoutSkipsWaypointedServices(t *testing.T) {
	remote := makeEndpoint("10.9.0.1", "cluster-2", "node-9", 8080)
	remote.KubernetesMetadata.NodeIp = "192.168.9.1"
	c := quicFanoutCache(t, true, map[string][]*registryv1.ServiceEndpoint{
		"demo/local":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
		"demo/spread": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080), remote},
	})
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	spread := proxy.ServiceClusterName("demo/spread", c.meshDomain)
	local := proxy.ServiceClusterName("demo/local", c.meshDomain)
	require.Contains(t, clusters, spread, "the waypointed service keeps its h2 cluster")
	twins := quicTwinsBySvc(t, clusters, c.meshDomain)
	assert.NotContains(t, twins, spread, "a waypointed service must never get a twin")
	assert.Len(t, twins[local], 2, "the local-only service still gets one twin per SA: %v", twins)

	for _, res := range snap.GetResources(resourcev3.RouteType) {
		for _, vh := range res.(*routev3.RouteConfiguration).GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				if _, noMatch, ok := proxy.QUICSelectionArms(r); ok {
					assert.NotEqual(t, spread, noMatch, "vhost %s selects for the waypointed service", vh.GetName())
				}
			}
		}
	}
}

// TestQUICFanoutWaitsForIdentity: before the node SVID is served there is no
// pinned h2 cluster and therefore no twin and no selection -- the identity
// readiness gate the unconditional fan-out keeps.
func TestQUICFanoutWaitsForIdentity(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "a-0", Namespace: "demo", ServiceAccount: "source-a",
		NetworkNamespace: "/var/run/netns/cni-a-0",
	}, quicTestTD))
	declareDeps(c, "demo/echo")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"demo/echo": {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	assert.Empty(t, quicTwinsBySvc(t, snap.GetResources(resourcev3.ClusterType), c.meshDomain), "no twin before the node identity is served")
	assert.Empty(t, c.quicArmsByService(), "no selection arms before the node identity is served")
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
