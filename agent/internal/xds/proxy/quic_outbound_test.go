package proxy

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/config"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
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

// TestQUICAltStatName pins the per-source stats key (aether#960).
func TestQUICAltStatName(t *testing.T) {
	assert.Equal(t, "demo/echo@demo/source-a", QUICAltStatName("demo/echo", "demo/source-a"))
	assert.Equal(t, "", QUICAltStatName("", "demo/source-a"), "no h2 key: Envoy keys by the twin's own name")
	assert.Equal(t, "", QUICAltStatName("demo/echo", ""), "no source key: never fall back to the SHARED h2 key")
}

// TestQUICServerName pins the SNI/server_names contract both ends share: the
// port as the first label of the destination's mesh authority, so the name
// falls under the "*.<sa>.<ns>.<meshDomain>" DNS SAN SPIRE issues (aether#957).
func TestQUICServerName(t *testing.T) {
	assert.Equal(t, "8080.echo.demo.aether.internal", QUICServerName("8080", "echo.demo.aether.internal"))
	assert.Equal(t, "8081.echo.demo.aether.internal", QUICServerName("8081", ServiceClusterName("demo/echo", "aether.internal")))
}

// TestQUICClusterFrom pins the per-source HTTP/3 cluster's shape (038 Phase 4b):
// a clone of the h2 base (same endpoints via its own EDS name, subsets,
// outlier detection),
// renamed, HTTP/3 protocol options, and a QuicUpstreamTransport that names the
// SOURCE's SVID statically, pins the destination SAN, carries the SNI it is given,
// offers only h3 and sets MaxSessionKeys: 0 (R4 on the client side).
func TestQUICClusterFrom(t *testing.T) {
	base := NewServiceCluster("echo.demo.aether.internal", "demo/echo", "demo/echo", []string{"zone"})
	q := QUICClusterFrom(base, "quic:echo.demo.aether.internal@demo/source-a",
		"spiffe://aether.internal/ns/demo/sa/source-a", "spiffe://aether.internal",
		[]string{"spiffe://aether.internal/ns/demo/sa/echo"}, QUICServerName("8080", "echo.demo.aether.internal"))

	assert.Equal(t, "quic:echo.demo.aether.internal@demo/source-a", q.GetName())
	assert.Equal(t, "demo/echo@demo/source-a", q.GetAltStatName(), "a twin must NOT share the h2 cluster's stat tree (aether#960)")
	assert.Equal(t, "demo/echo", base.GetAltStatName(), "the base keeps its own")
	assert.Equal(t, "quic:echo.demo.aether.internal@demo/source-a", q.GetEdsClusterConfig().GetServiceName(),
		"the twin's OWN EDS resource: sharing the base's name is deduplicated by Envoy's delta WatchMap and a late twin warms for 15 s (aether#1008)")
	assert.Equal(t, "demo/echo", base.GetEdsClusterConfig().GetServiceName(), "the base keeps the bare-service EDS name")
	assert.True(t, proto.Equal(base.GetEdsClusterConfig().GetEdsConfig(), q.GetEdsClusterConfig().GetEdsConfig()), "same ads: {} source: the fix is the name, not a second stream")
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

	// A GAMMA split (weighted clusters) is not rewritten: the matcher action
	// names one cluster, so a per-source weighted split has no representation.
	wvh := &routev3.VirtualHost{Name: h2, Routes: []*routev3.Route{{
		Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_WeightedClusters{
			WeightedClusters: &routev3.WeightedCluster{Clusters: []*routev3.WeightedCluster_ClusterWeight{{Name: h2, Weight: wrapperspb.UInt32(100)}}},
		}}},
	}}}
	assert.Equal(t, 0, ApplyQUICClusterSelection(wvh, h2, arms))

	// aether#961: a GAMMA rule whose single backendRef is the parent renders as
	// `cluster: <h2>` and IS selected -- it rides QUIC like the default route;
	// a single-cluster rule to ANOTHER service's cluster is not this vhost's
	// h2 cluster and is left alone.
	other := "other.demo.aether.internal"
	gvh := &routev3.VirtualHost{Name: h2, Routes: []*routev3.Route{
		{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/api"}},
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: h2}}},
		},
		{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/elsewhere"}},
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: other}}},
		},
	}}
	assert.Equal(t, 1, ApplyQUICClusterSelection(gvh, h2, arms), "the single-backend GAMMA rule to the parent is selected, the rule to another service is not")
	_, _, sel := QUICSelectionArms(gvh.GetRoutes()[0])
	assert.True(t, sel, "/api (single backendRef = parent) must carry the selection")
	assert.Equal(t, other, gvh.GetRoutes()[1].GetRoute().GetCluster(), "/elsewhere keeps its own cluster")
}

// TestQUICLoadAssignmentFrom pins the twin's load assignment (aether#1008): the
// base's, byte for byte, under the twin's name. Every field of the base is
// populated so a field the copy forgets shows up as a difference, and the
// descriptor's field set is pinned so a NEW upstream field fails here instead
// of silently vanishing from every twin.
func TestQUICLoadAssignmentFrom(t *testing.T) {
	base := &endpointv3.ClusterLoadAssignment{
		ClusterName: "demo/echo",
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			Locality: &corev3.Locality{Region: "r1", Zone: "z1"},
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HealthStatus: corev3.HealthStatus_DRAINING,
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
					Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
						Address: "10.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 18008},
					}}},
				}},
			}},
		}},
		NamedEndpoints: map[string]*endpointv3.Endpoint{"n": {Hostname: "h"}},
		Policy:         &endpointv3.ClusterLoadAssignment_Policy{OverprovisioningFactor: wrapperspb.UInt32(140)},
	}
	const twin = "quic:echo.demo.aether.internal@demo/source-a"

	got := QUICLoadAssignmentFrom(base, twin)
	require.Equal(t, twin, got.GetClusterName())
	assert.Equal(t, "demo/echo", base.GetClusterName(), "the base must not be renamed")
	renamed, _ := proto.Clone(got).(*endpointv3.ClusterLoadAssignment)
	renamed.ClusterName = base.GetClusterName()
	assert.True(t, proto.Equal(base, renamed), "the twin's CLA is the base's in every field but the name")

	a, err := proto.MarshalOptions{Deterministic: true}.Marshal(QUICLoadAssignmentFrom(base, twin))
	require.NoError(t, err)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(QUICLoadAssignmentFrom(base, twin))
	require.NoError(t, err)
	assert.Equal(t, a, b, "identical inputs must marshal to identical bytes (delta-xDS hashing)")

	var fields []string
	fds := base.ProtoReflect().Descriptor().Fields()
	for i := range fds.Len() {
		fields = append(fields, string(fds.Get(i).Name()))
	}
	assert.ElementsMatch(t, []string{"cluster_name", "endpoints", "named_endpoints", "policy"}, fields,
		"ClusterLoadAssignment grew a field: QUICLoadAssignmentFrom must carry it")
	assert.Nil(t, QUICLoadAssignmentFrom(nil, twin))
}
