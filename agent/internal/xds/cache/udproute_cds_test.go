package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// udpProxyClustersNamed returns every cluster a udp_proxy listener filter names
// in the snapshot, keyed by the listener that named it.
//
// The #895 gate (TestCaptureChainsResolveAgainstCDS) walks filter CHAINS looking
// for tcp_proxy. A connection-less UDP listener has no filter chains at all —
// udp_proxy is a LISTENER filter — so that gate never saw this path. This is the
// UDP arm of the same check.
func udpProxyClustersNamed(t *testing.T, listeners map[string]any) map[string]string {
	t.Helper()
	named := map[string]string{}
	for _, res := range listeners {
		l, ok := res.(*listenerv3.Listener)
		if !ok {
			continue
		}
		for _, lf := range l.GetListenerFilters() {
			cfg := &udp_proxyv3.UdpProxyConfig{}
			if lf.GetTypedConfig() == nil || lf.GetTypedConfig().UnmarshalTo(cfg) != nil {
				continue
			}
			// 038: the route specifier is a matcher keyed on the dialled VIP,
			// one arm per UDPRoute parent. Walk every arm; the deprecated bare
			// `cluster` form must not come back.
			require.Empty(t, cfg.GetCluster(), "listener %q uses the deprecated single-cluster specifier", l.GetName())
			for vip, om := range cfg.GetMatcher().GetMatcherTree().GetExactMatchMap().GetMap() {
				r := &udp_proxyv3.Route{}
				require.NoError(t, om.GetAction().GetTypedConfig().UnmarshalTo(r), "listener %q arm %s", l.GetName(), vip)
				if c := r.GetCluster(); c != "" {
					named[c] = l.GetName()
				}
			}
		}
	}
	return named
}

// TestUDPCaptureListenerResolvesAgainstCDS is proposal 037 Risk 1 applied to the
// UDP path (#873, #877).
//
// udp_proxy has no ODCDS cold path any more than tcp_proxy does: a listener that
// names a cluster the snapshot does not carry accepts datagrams and drops them,
// with no NACK, no warning and no stat that distinguishes it from a backend that
// is down. captureUDPClusters SKIPS a backend service that is not in the cluster
// cache or whose registered port cannot be read, so the generator must not be
// able to bind a backend whose cluster that skip removed.
func TestUDPCaptureListenerResolvesAgainstCDS(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "client-0",
		Namespace:        "aether-test",
		ServiceAccount:   "client",
		NetworkNamespace: "/var/run/netns/cni-client",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	// A UDPRoute whose backends are, in this order:
	//   - "aether-test/ghost": accepted by the projector, but NOT in the
	//     registry, so captureUDPClusters produces no udp: cluster for it. This
	//     is an ordinary race — a route written before its backend Service is
	//     registered, or a backend whose endpoints this node does not hold.
	//   - "aether-test/udp-a": real, and therefore buildable.
	// The ghost carries the HEAVIER weight, so nothing about the weight ordering
	// rescues this: only checking against the clusters actually built does.
	ghostCluster := proxy.UDPClusterName("aether-test/ghost", "aether.internal")
	liveCluster := proxy.UDPClusterName("aether-test/udp-a", "aether.internal")
	c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{
		"aether-test/udp-a": {
			{Service: "aether-test/ghost", Cluster: ghostCluster, Weight: 90},
			{Service: "aether-test/udp-a", Cluster: liveCluster, Weight: 10},
		},
	})

	// The matcher keys on the parent's ClusterIP, which reaches the cache on
	// the capture reconciler's Service watch (the same source the TCP floor
	// chains match on). Without it there is no arm at all.
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "aether-test/udp-a", ClusterIP: "10.96.0.44"}})

	declareDeps(c, "aether-test/udp-a")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/udp-a": {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	cds := map[string]struct{}{}
	for name := range snap.GetResources(resourcev3.ClusterType) {
		cds[name] = struct{}{}
	}
	require.Contains(t, cds, liveCluster, "the buildable UDP cluster must exist, or this test proves nothing")
	require.NotContains(t, cds, ghostCluster, "the ghost backend must NOT have produced a cluster, or this test proves nothing")

	listeners := map[string]any{}
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		listeners[name] = res
	}
	named := udpProxyClustersNamed(t, listeners)
	require.NotEmpty(t, named, "no UDP capture listener was generated, so nothing is under test")

	// The listener is built at AddPod/SetUDPServiceRoutes time, BEFORE the
	// registry fills the cluster cache, so at build time nothing was routable.
	// It exists here only because reconcileUDPCaptureListeners noticed the
	// bindable cluster appear on the next snapshot push — without that, a
	// UDPRoute landing ahead of its backend would stay dark until the next
	// UDPRoute event.
	assert.Contains(t, named, liveCluster,
		"the unroutable heavier backend must fall through to the one that resolves, not silence the listener")

	for cluster, listener := range named {
		assert.Contains(t, cds, cluster,
			"UDP capture listener %q routes to cluster %q, which is not in the snapshot — udp_proxy has no ODCDS cold path, so those datagrams are accepted and dropped in silence",
			listener, cluster)
	}
}
