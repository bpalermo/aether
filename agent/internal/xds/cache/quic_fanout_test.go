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
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQUICFanoutPublishesPerSourceTwins is proposal 038 Phase 4b end to end
// in the cache: with "demo/echo" allow-listed and two local pods of two
// ServiceAccounts that have both dialled it (aether#1020 builds twins only for
// observed pairs), the snapshot carries one `quic:` twin per SA (QUIC
// transport, HTTP/3), BOTH route tables' echo vhost selects between them by
// source identity with the h2 cluster as on_no_match, an unlisted service
// gets nothing, and clearing the allow-list removes it all.
func TestQUICFanoutPublishesPerSourceTwins(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	const td = "aether.internal"

	for _, p := range []struct{ name, sa string }{{"a-0", "source-a"}, {"a-1", "source-a"}, {"b-0", "source-b"}} {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: p.name, Namespace: "demo", ServiceAccount: p.sa,
			NetworkNamespace: "/var/run/netns/cni-" + p.name,
		}, td))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	c.SetCaptureAuthorities(map[string]string{"demo/echo": "echo.demo.svc.cluster.local", "demo/other": "other.demo.svc.cluster.local"})

	// The allow-list alone must pull the destination into the dependency set.
	c.SetEastWestQUICServices([]string{"demo/echo"})
	declareDeps(c, "demo/other")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/echo":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
				"demo/other": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	// Twins are demand-scoped (aether#1020): both sources have dialled echo.
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	echo := proxy.ServiceClusterName("demo/echo", c.meshDomain)
	twinA := proxy.QUICClusterName("demo/echo", c.meshDomain, "demo/source-a")
	twinB := proxy.QUICClusterName("demo/echo", c.meshDomain, "demo/source-b")
	clusters := snap.GetResources(resourcev3.ClusterType)
	require.Contains(t, clusters, echo, "the h2 cluster must exist (forced into the dependency set by the allow-list)")
	for _, name := range []string{twinA, twinB} {
		_, ok := clusters[name]
		require.True(t, ok, "missing twin %s in %v", name, keysOf(clusters))
	}
	var quicTwins int
	for name, res := range clusters {
		if !strings.HasPrefix(name, "quic:") {
			continue
		}
		quicTwins++
		ts := res.(*clusterv3.Cluster).GetTransportSocket()
		require.True(t, ts.GetTypedConfig().MessageIs(&quicv3.QuicUpstreamTransport{}), "%s must carry a QUIC upstream transport", name)
		assert.Equal(t, "demo/echo@"+strings.TrimPrefix(name, "quic:"+echo+"@"), res.(*clusterv3.Cluster).GetAltStatName(), "%s: per-source stats key (aether#960)", name)
		qt := &quicv3.QuicUpstreamTransport{}
		require.NoError(t, ts.GetTypedConfig().UnmarshalTo(qt))
		assert.Equal(t, "8080."+echo, qt.GetUpstreamTlsContext().GetSni(), "%s: SNI is <port>.<authority>, never the bare port (aether#957)", name)
	}
	assert.Equal(t, 2, quicTwins, "one twin per local ServiceAccount (two pods of source-a share one), none for the unlisted service: %v", keysOf(clusters))

	// Both route tables select by source identity on the echo vhost.
	wantArms := map[string]string{
		"spiffe://" + td + "/ns/demo/sa/source-a": twinA,
		"spiffe://" + td + "/ns/demo/sa/source-b": twinB,
	}
	var selecting int
	for name, res := range snap.GetResources(resourcev3.RouteType) {
		rc := res.(*routev3.RouteConfiguration)
		for _, vh := range rc.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				arms, noMatch, ok := proxy.QUICSelectionArms(r)
				if !ok {
					continue
				}
				selecting++
				assert.Equal(t, wantArms, arms, "route table %s vhost %s", name, vh.GetName())
				assert.Equal(t, echo, noMatch, "on_no_match must be the h2 cluster")
				assert.NotContains(t, vh.GetName(), "other", "the unlisted service must not select")
			}
		}
	}
	assert.GreaterOrEqual(t, selecting, 2, "the echo vhost on BOTH out_http and cap_http must carry the selection plugin")

	// Off again: no twins, no selection, byte-identical to before. The setter
	// signals an asynchronous regeneration; force it here so the read is not
	// the previous snapshot.
	c.SetEastWestQUICServices(nil)
	require.NoError(t, c.generateSnapshot(ctx))
	snap, err = c.GetSnapshot("node-1")
	require.NoError(t, err)
	for name := range snap.GetResources(resourcev3.ClusterType) {
		assert.False(t, strings.HasPrefix(name, "quic:"), "twin %s survived clearing the allow-list", name)
	}
	for _, res := range snap.GetResources(resourcev3.RouteType) {
		for _, vh := range res.(*routev3.RouteConfiguration).GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				_, _, ok := proxy.QUICSelectionArms(r)
				assert.False(t, ok, "vhost %s still selects after the allow-list was cleared", vh.GetName())
			}
		}
	}
	assert.Empty(t, c.QUICPairs(), "clearing the allow-list must prune the observed pairs too (aether#1020)")
}

// observeQUIC records that each source ("<ns>/<sa>") has dialled service over
// QUIC -- what an admitted on-demand request does -- and regenerates the
// snapshot synchronously.
func observeQUIC(t *testing.T, c *SnapshotCache, service string, sources ...string) {
	t.Helper()
	for _, src := range sources {
		d, reason := c.recordQUICPair(proxy.QUICClusterName(service, c.meshDomain, src))
		require.NotEqual(t, QUICTwinRefused, d, "pair %s <- %s refused: %s", service, src, reason)
	}
	require.NoError(t, c.generateSnapshot(context.Background()))
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
