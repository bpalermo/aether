package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
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
// in the cache, now that east-west QUIC is unconditional (#979): with two
// destinations in the dependency set and two local ServiceAccounts, EVERY
// destination's vhost on BOTH route tables selects by source identity -- one
// arm per SA, the h2 cluster as on_no_match -- and a `quic:` twin (QUIC
// transport, HTTP/3) exists for exactly the (destination, source) pairs that
// have dialled (aether#1020 demand scoping): both SAs for echo, only source-a
// for other.
//
// "demo/other" is the anti-vacuity case for dropping the allow-list: under
// the old predicate (only allow-listed destinations, here "demo/echo") its
// vhost carried no arms and its on-demand request was refused
// (destination_not_quic_enabled), so this test is red on that predicate.
func TestQUICFanoutPublishesPerSourceTwins(t *testing.T) {
	c := quicFanoutCache(t, false, map[string][]*registryv1.ServiceEndpoint{
		"demo/echo":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
		"demo/other": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080)},
	})
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")
	observeQUIC(t, c, "demo/other", "demo/source-a")
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	observed := map[string][]string{
		"demo/echo":  {"source-a", "source-b"},
		"demo/other": {"source-a"},
	}
	wantArms := map[string]map[string]string{}
	for svc, dialled := range observed {
		h2 := proxy.ServiceClusterName(svc, c.meshDomain)
		require.Contains(t, clusters, h2, "the h2 cluster must exist")
		arms := map[string]string{}
		for _, sa := range []string{"source-a", "source-b"} {
			arms["spiffe://"+quicTestTD+"/ns/demo/sa/"+sa] = proxy.QUICClusterName(svc, c.meshDomain, "demo/"+sa)
		}
		wantArms[h2] = arms
		for _, sa := range dialled {
			twin := proxy.QUICClusterName(svc, c.meshDomain, "demo/"+sa)
			_, ok := clusters[twin]
			require.True(t, ok, "missing twin %s in %v", twin, keysOf(clusters))
		}
	}
	twins := quicTwinsBySvc(t, clusters, c.meshDomain)
	assert.Len(t, twins, 2, "both destinations get twins -- a service nobody listed included: %v", twins)
	// Exactly one per observed pair: the two pods of source-a share one, and
	// the per-port / alias entries (the :18081 mesh-port alias exists here)
	// never get their own.
	assert.Len(t, twins[proxy.ServiceClusterName("demo/echo", c.meshDomain)], 2, "echo: one twin per dialled SA: %v", twins)
	assert.Len(t, twins[proxy.ServiceClusterName("demo/other", c.meshDomain)], 1, "other: only source-a dialled: %v", twins)

	// Both route tables select by source identity on BOTH destinations' vhosts,
	// with an arm for every local SA whether or not its twin is built yet.
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

// testQUICStream is the xDS stream -- the proxy generation, issue #1052 -- the
// cache tests' on-demand requests and re-statements arrive on, unless a test
// models more than one.
const testQUICStream int64 = 1

// observeQUIC records that each source ("<ns>/<sa>") has dialled service over
// QUIC -- what an admitted on-demand request does -- and regenerates the
// snapshot synchronously.
func observeQUIC(t *testing.T, c *SnapshotCache, service string, sources ...string) {
	t.Helper()
	for _, src := range sources {
		d, reason := c.recordQUICPair(testQUICStream, proxy.QUICClusterName(service, c.meshDomain, src))
		require.NotEqual(t, QUICTwinRefused, d, "pair %s <- %s refused: %s", service, src, reason)
	}
	require.NoError(t, c.generateSnapshot(context.Background()))
}

// TestQUICFanoutSkipsWaypointedServices: with the east/west waypoint on, a
// service that has an endpoint in another cluster (dialed at that node's
// tunnel, waypoint-tagged) stays h2 -- no arm, no twin, even for a pair that
// has been observed -- because a twin carries ONE static QUIC socket and not
// the waypoint transport-socket matcher, and the tunnel has no QUIC leg. A
// local-only service on the same node still gets its twins, so the skip is
// per service, not a node-wide off switch.
func TestQUICFanoutSkipsWaypointedServices(t *testing.T) {
	remote := makeEndpoint("10.9.0.1", "cluster-2", "node-9", 8080)
	remote.KubernetesMetadata.NodeIp = "198.51.100.1"
	c := quicFanoutCache(t, true, map[string][]*registryv1.ServiceEndpoint{
		"demo/local":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
		"demo/spread": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080), remote},
	})
	observeQUIC(t, c, "demo/local", "demo/source-a", "demo/source-b")
	observeQUIC(t, c, "demo/spread", "demo/source-a", "demo/source-b")
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
// pinned h2 cluster and therefore no twin and no selection -- even for an
// observed pair -- the identity readiness gate the unconditional fan-out keeps.
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
	observeQUIC(t, c, "demo/echo", "demo/source-a")
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

// TestQUICTwinsCarryTheirOwnIdleTimeout (aether#1054): every `quic:` twin's
// pool carries the configured h3 idle timeout (default 8s), while the h2 base
// and every other cluster keep the 30s UpstreamIdleTimeout.
func TestQUICTwinsCarryTheirOwnIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"default", 0, config.DefaultQUICTwinIdleTimeout},
		{"configured", 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache("node-1")
			ctx := context.Background()
			const td = "aether.internal"
			require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
				Name: "a-0", Namespace: "demo", ServiceAccount: "source-a",
				NetworkNamespace: "/var/run/netns/cni-a-0",
			}, td))
			require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
			if tc.set != 0 {
				c.SetQUICIdleTimeout(tc.set)
			}
			declareDeps(c, "demo/echo")
			reg := &mockRegistry{
				listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
					return map[string][]*registryv1.ServiceEndpoint{
						"demo/echo": {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
					}, nil
				},
			}
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
			observeQUIC(t, c, "demo/echo", "demo/source-a")
			snap, err := c.GetSnapshot("node-1")
			require.NoError(t, err)

			var twins, others int
			for name, res := range snap.GetResources(resourcev3.ClusterType) {
				raw, ok := res.(*clusterv3.Cluster).GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey]
				if !ok {
					continue
				}
				po := &httpv3.HttpProtocolOptions{}
				require.NoError(t, raw.UnmarshalTo(po))
				idle := po.GetCommonHttpProtocolOptions().GetIdleTimeout().AsDuration()
				if strings.HasPrefix(name, "quic:") {
					twins++
					assert.Equal(t, tc.want, idle, "%s: h3 twin idle timeout", name)
					continue
				}
				others++
				assert.Equal(t, config.UpstreamIdleTimeout, idle, "%s: non-twin clusters keep the 30s idle timeout", name)
			}
			assert.Equal(t, 1, twins, "one twin for the one observed pair")
			assert.Positive(t, others, "the h2 base must be checked too")
		})
	}
}
