package proxy

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/config"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestSourceSAKeyFromSpiffeID(t *testing.T) {
	assert.Equal(t, "demo/source-a", SourceSAKeyFromSpiffeID("spiffe://aether.internal/ns/demo/sa/source-a"))
	assert.Equal(t, "", SourceSAKeyFromSpiffeID("spiffe://aether.internal/node/main-worker-01"), "a node identity is not a workload")
	assert.Equal(t, "", SourceSAKeyFromSpiffeID("spiffe://aether.internal/ns/demo/sa/"), "empty sa")
	assert.Equal(t, "", SourceSAKeyFromSpiffeID("spiffe://aether.internal/ns/demo/sa/a/b"), "nested path is not a workload id")
	assert.Equal(t, "", SourceSAKeyFromSpiffeID(""))
}

func TestQUICClusterName(t *testing.T) {
	assert.Equal(t, "quic:echo.demo.aether.internal@demo/source-a", QUICClusterName("demo/echo", "aether.internal", "demo/source-a"))
}

// TestQUICServerName pins the SNI/server_names contract both ends share: the
// port as the first label of the destination's mesh authority, so the name
// falls under the "*.<sa>.<ns>.<meshDomain>" DNS SAN SPIRE issues (aether#957).
func TestQUICServerName(t *testing.T) {
	assert.Equal(t, "8080.echo.demo.aether.internal", QUICServerName("8080", "echo.demo.aether.internal"))
	assert.Equal(t, "8081.echo.demo.aether.internal", QUICServerName("8081", ServiceClusterName("demo/echo", "aether.internal")))
}

// TestQUICClusterFrom pins the per-source HTTP/3 cluster's shape (038 Phase 4b):
// a clone of the h2 base (same EDS resource, subsets, outlier detection),
// renamed, HTTP/3 protocol options, and a QuicUpstreamTransport that names the
// SOURCE's SVID statically, pins the destination SAN, carries the SNI it is given,
// offers only h3 and sets MaxSessionKeys: 0 (R4 on the client side).
func TestQUICClusterFrom(t *testing.T) {
	base := NewServiceCluster("echo.demo.aether.internal", "demo/echo", "demo/echo", []string{"zone"})
	q := QUICClusterFrom(base, "quic:echo.demo.aether.internal@demo/source-a",
		"spiffe://aether.internal/ns/demo/sa/source-a", "spiffe://aether.internal",
		[]string{"spiffe://aether.internal/ns/demo/sa/echo"}, QUICServerName("8080", "echo.demo.aether.internal"))

	assert.Equal(t, "quic:echo.demo.aether.internal@demo/source-a", q.GetName())
	assert.Equal(t, "demo/echo", q.GetEdsClusterConfig().GetServiceName(), "the same EDS resource as the h2 twin: no second load assignment")
	assert.Equal(t, clusterv3.Cluster_EDS, q.GetType())
	assert.NotNil(t, q.GetLbSubsetConfig(), "subset config cloned")
	assert.NotNil(t, q.GetOutlierDetection(), "outlier detection cloned")
	assert.Equal(t, "echo.demo.aether.internal", base.GetName(), "the base must not be mutated")
	assert.Nil(t, base.GetTransportSocket())

	po := &httpv3.HttpProtocolOptions{}
	require.NoError(t, q.GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey].UnmarshalTo(po))
	assert.NotNil(t, po.GetExplicitHttpConfig().GetHttp3ProtocolOptions(), "explicit HTTP/3")

	require.Equal(t, "envoy.transport_sockets.quic", q.GetTransportSocket().GetName())
	assert.Nil(t, q.GetTransportSocketMatcher(), "no per-connection selection on QUIC: the identity is the cluster's")
	qt := &quicv3.QuicUpstreamTransport{}
	require.NoError(t, q.GetTransportSocket().GetTypedConfig().UnmarshalTo(qt))
	ctx := qt.GetUpstreamTlsContext()
	assert.Equal(t, "8080.echo.demo.aether.internal", ctx.GetSni(), "the hostname-form SNI the QUIC client can verify (aether#957)")
	require.NotNil(t, ctx.MaxSessionKeys, "R4 client side: MaxSessionKeys must be EXPLICIT")
	assert.Equal(t, uint32(0), ctx.GetMaxSessionKeys().GetValue())
	assert.Equal(t, []string{"h3"}, ctx.GetCommonTlsContext().GetAlpnProtocols())
	require.Len(t, ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs(), 1)
	assert.Equal(t, "spiffe://aether.internal/ns/demo/sa/source-a", ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()[0].GetName(), "the SOURCE SA's SVID, statically")
	assert.Nil(t, ctx.GetCommonTlsContext().GetCustomTlsCertificateSelector(), "QuicClientTransportSocketFactory rejects a certificate selector")
	sans := ctx.GetCommonTlsContext().GetCombinedValidationContext().GetDefaultValidationContext().GetMatchTypedSubjectAltNames()
	require.Len(t, sans, 1)
	assert.Equal(t, "spiffe://aether.internal/ns/demo/sa/echo", sans[0].GetMatcher().GetExact(), "the destination SAN pin, same as the h2 twin")
}

// TestApplyQUICClusterSelection pins the selection mechanism: the route to the
// h2 cluster becomes a matcher cluster specifier keyed on the source
// identity filter state, one arm per local identity, on_no_match = h2. A
// GAMMA weighted route is untouched; an empty arm set is a no-op.
func TestApplyQUICClusterSelection(t *testing.T) {
	const h2 = "echo.demo.aether.internal"
	vh := BuildOutboundClusterVirtualHost(h2, []string{h2})
	arms := map[string]string{
		"spiffe://aether.internal/ns/demo/sa/source-a": "quic:" + h2 + "@demo/source-a",
		"spiffe://aether.internal/ns/demo/sa/source-b": "quic:" + h2 + "@demo/source-b",
	}
	assert.Equal(t, 0, ApplyQUICClusterSelection(vh, h2, nil), "no arms, no rewrite")
	n := ApplyQUICClusterSelection(vh, h2, arms)
	require.Equal(t, 1, n)

	var found bool
	for _, r := range vh.GetRoutes() {
		got, noMatch, ok := QUICSelectionArms(r)
		if !ok {
			continue
		}
		found = true
		assert.Equal(t, arms, got)
		assert.Equal(t, h2, noMatch, "an identity with no arm -- or no stamp -- keeps the h2 cluster")
		assert.Nil(t, r.GetRoute().GetEarlyDataPolicy(), "no 0-RTT on a route that can reach QUIC (R4)")
	}
	require.True(t, found, "no route carries the selection plugin")

	// A GAMMA split (weighted clusters) is not rewritten: GAMMA-routed
	// destinations stay h2 in this cut.
	wvh := &routev3.VirtualHost{Name: h2, Routes: []*routev3.Route{{
		Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_WeightedClusters{
			WeightedClusters: &routev3.WeightedCluster{Clusters: []*routev3.WeightedCluster_ClusterWeight{{Name: h2, Weight: wrapperspb.UInt32(100)}}},
		}}},
	}}}
	assert.Equal(t, 0, ApplyQUICClusterSelection(wvh, h2, arms))
}
