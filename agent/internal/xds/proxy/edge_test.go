package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEdgeUpstreamTransportSocket verifies the edge fetches its SVID and trust
// bundle over the static spire_agent SDS cluster (SPIRE directly), not ADS,
// while SAN pinning still applies (inline matchers over the SDS bundle).
func TestEdgeUpstreamTransportSocket(t *testing.T) {
	sans := []string{"spiffe://aether.internal/ns/aether-test/sa/svc-1"}
	ts := EdgeUpstreamTransportSocket("spiffe://aether.internal/ns/aether-edge/sa/edge", "spiffe://aether.internal", sans, "8080")

	utc := &transport_sockets_v3.UpstreamTlsContext{}
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(utc))

	common := utc.GetCommonTlsContext()
	require.Len(t, common.GetTlsCertificateSdsSecretConfigs(), 1)
	cert := common.GetTlsCertificateSdsSecretConfigs()[0]
	assert.Equal(t, "spiffe://aether.internal/ns/aether-edge/sa/edge", cert.GetName())
	// The cert SDS comes from the spire_agent gRPC cluster, not ADS.
	grpc := cert.GetSdsConfig().GetApiConfigSource().GetGrpcServices()
	require.Len(t, grpc, 1)
	assert.Equal(t, SpireAgentSDSClusterName, grpc[0].GetEnvoyGrpc().GetClusterName())

	// SAN pinning unchanged: combined validation context, bundle still over the
	// spire_agent SDS cluster.
	combined := common.GetCombinedValidationContext()
	require.NotNil(t, combined)
	bundleSDS := combined.GetValidationContextSdsSecretConfig()
	assert.Equal(t, "spiffe://aether.internal", bundleSDS.GetName())
	assert.Equal(t, SpireAgentSDSClusterName,
		bundleSDS.GetSdsConfig().GetApiConfigSource().GetGrpcServices()[0].GetEnvoyGrpc().GetClusterName())
	require.Len(t, combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames(), 1)
	assert.Equal(t, "8080", utc.GetSni())
}

// TestNewServiceCluster_EdgePoolingOff verifies the edge passes pooling off for
// full upstream multiplexing on its single identity.
func TestNewServiceCluster_EdgePoolingOff(t *testing.T) {
	c := NewServiceCluster("svc-a.aether.internal", "svc-a", "svc-a", nil, false)
	assert.False(t, c.GetConnectionPoolPerDownstreamConnection())
}

// TestBuildEdgeListener verifies the public-facing edge listener: bound on all
// interfaces at the configured port, RDS-driven, with the readiness filter
// ahead of the router.
func TestBuildEdgeListener(t *testing.T) {
	l := BuildEdgeListener(EdgeListenerName, 8080, nil)

	assert.Equal(t, EdgeListenerName, l.GetName())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, l.GetTrafficDirection())
	addr := l.GetAddress().GetSocketAddress()
	assert.Equal(t, defaultEdgeAddress, addr.GetAddress())
	assert.Equal(t, uint32(8080), addr.GetPortValue())

	require.Len(t, l.GetFilterChains(), 1)
	filters := l.GetFilterChains()[0].GetFilters()
	require.Len(t, filters, 1)

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, filters[0].GetTypedConfig().UnmarshalTo(hcm))
	assert.Equal(t, EdgeHTTPRouteName, hcm.GetRds().GetRouteConfigName())

	// Readiness health_check filter is first (ahead of the router).
	require.GreaterOrEqual(t, len(hcm.GetHttpFilters()), 2)
	assert.Equal(t, httpHealthCheckFilterName, hcm.GetHttpFilters()[0].GetName())
	assert.Equal(t, httpRouterFilterName, hcm.GetHttpFilters()[len(hcm.GetHttpFilters())-1].GetName())
}

// TestBuildEdgeListenerTLS verifies downstream TLS termination: the filter chain
// gets a TLS transport socket serving the named SDS certs (SNI-selected), does
// NOT require a client certificate, and sets the TLS floor + ALPN.
func TestBuildEdgeListenerTLS(t *testing.T) {
	l := BuildEdgeListener(EdgeHTTPSListenerName, 8443, []string{"kubernetes/api-tls", "kubernetes/foo-tls"})

	ts := l.GetFilterChains()[0].GetTransportSocket()
	require.NotNil(t, ts, "TLS filter chain has a transport socket")
	assert.Equal(t, tlsTransportSocketName, ts.GetName())

	dtc := &transport_sockets_v3.DownstreamTlsContext{}
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(dtc))
	assert.False(t, dtc.GetRequireClientCertificate().GetValue(), "external clients present no cert")

	common := dtc.GetCommonTlsContext()
	sds := common.GetTlsCertificateSdsSecretConfigs()
	require.Len(t, sds, 2)
	assert.Equal(t, "kubernetes/api-tls", sds[0].GetName())
	assert.Equal(t, "kubernetes/foo-tls", sds[1].GetName())
	// Certs come over ADS (the agent-served cache secrets), not a file/SPIRE source.
	_, isAds := sds[0].GetSdsConfig().GetConfigSourceSpecifier().(*corev3.ConfigSource_Ads)
	assert.True(t, isAds, "downstream certs served over ADS SDS")
	assert.Equal(t, transport_sockets_v3.TlsParameters_TLSv1_2, common.GetTlsParams().GetTlsMinimumProtocolVersion())
	assert.Equal(t, []string{"h2", "http/1.1"}, common.GetAlpnProtocols())
}

// TestNewDownstreamTLSSecret verifies the SDS secret carries inline cert/key.
func TestNewDownstreamTLSSecret(t *testing.T) {
	s := NewDownstreamTLSSecret("kubernetes/api-tls", []byte("CERT"), []byte("KEY"))
	assert.Equal(t, "kubernetes/api-tls", s.GetName())
	assert.Equal(t, []byte("CERT"), s.GetTlsCertificate().GetCertificateChain().GetInlineBytes())
	assert.Equal(t, []byte("KEY"), s.GetTlsCertificate().GetPrivateKey().GetInlineBytes())
}

// TestBuildEdgeRedirectListener verifies the :80 listener 301-redirects to https.
func TestBuildEdgeRedirectListener(t *testing.T) {
	l := BuildEdgeRedirectListener(8080)
	assert.Equal(t, EdgeRedirectListenerName, l.GetName())
	assert.Equal(t, uint32(8080), l.GetAddress().GetSocketAddress().GetPortValue())

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	rc := hcm.GetRouteConfig()
	require.NotNil(t, rc, "redirect uses an inline route config")
	red := rc.GetVirtualHosts()[0].GetRoutes()[0].GetRedirect()
	require.NotNil(t, red)
	assert.True(t, red.GetHttpsRedirect())
}

// TestBuildEdgeListenerNoTLS verifies the plain-HTTP edge listener has no
// transport socket.
func TestBuildEdgeListenerNoTLS(t *testing.T) {
	l := BuildEdgeListener(EdgeListenerName, 8080, nil)
	assert.Nil(t, l.GetFilterChains()[0].GetTransportSocket())
}

// TestBuildEdgeRouteConfiguration verifies the edge route table: the exposed
// vhosts plus a catch-all 404 (no ODCDS catch-all — the edge serves only its
// explicit exposed set).
func TestBuildEdgeRouteConfiguration(t *testing.T) {
	vh := BuildOutboundClusterVirtualHost("svc-1.aether.internal", []string{"svc-1.aether.internal"})
	rc := BuildEdgeRouteConfiguration([]*routev3.VirtualHost{vh})

	assert.Equal(t, EdgeHTTPRouteName, rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 2)
	assert.Equal(t, "svc-1.aether.internal", rc.GetVirtualHosts()[0].GetName())

	last := rc.GetVirtualHosts()[1]
	assert.Equal(t, []string{"*"}, last.GetDomains())
	require.Len(t, last.GetRoutes(), 1)
	assert.Equal(t, uint32(404), last.GetRoutes()[0].GetDirectResponse().GetStatus())
}

// TestEdgeL4ListenerNames verifies the naming helpers are stable.
func TestEdgeL4ListenerNames(t *testing.T) {
	assert.Equal(t, "edge_tcp_5432", EdgeTCPListenerName(5432))
	assert.Equal(t, "edge_tls_8443", EdgeTLSListenerName(8443))
}

// TestEdgeGatewayListenerNaming verifies per-Gateway listener and route names are
// unique across (Gateway, port) pairs — regression guard for #332 where all edge
// listeners shared "edge_http" causing the :443 listener to be silently dropped.
func TestEdgeGatewayListenerNaming(t *testing.T) {
	cases := []struct {
		ns, gw     string
		port       uint32
		wantSuffix string
	}{
		{"aether-ingress", "edge", 18100, "edge_gw_aether-ingress_edge_18100"},
		{"aether-ingress", "edge", 18101, "edge_gw_aether-ingress_edge_18101"},
		{"conformance", "same-ns", 18200, "edge_gw_conformance_same-ns_18200"},
	}
	names := map[string]bool{}
	for _, tc := range cases {
		n := EdgeGatewayListenerName(tc.ns, tc.gw, tc.port)
		assert.Equal(t, tc.wantSuffix, n)
		assert.False(t, names[n], "listener name %q must be unique across cases", n)
		names[n] = true
	}
}

// TestEdgeGatewayListenerNamesAllDistinct verifies that for a typical set of
// Gateways and listeners (as seen in conformance), ALL listener names are distinct
// — this is the core guarantee of proposal 021 Phase 2 (no LDS collision).
func TestEdgeGatewayListenerNamesAllDistinct(t *testing.T) {
	type gwListener struct {
		ns, gw, listenerSection string
	}
	pairs := []gwListener{
		{"aether-ingress", "edge", "http"},
		{"aether-ingress", "edge", "https"},
		{"gateway-conformance-infra", "same-namespace", "http"},
		{"gateway-conformance-infra", "backend-namespaces", "http"},
		{"gateway-conformance-app-backend", "my-gateway", "http"},
	}
	// Assign unique internal ports (simulating the allocator).
	names := map[string]bool{}
	for i, p := range pairs {
		internalPort := uint32(18100 + i)
		n := EdgeGatewayListenerName(p.ns, p.gw, internalPort)
		assert.False(t, names[n], "collision: listener name %q already used", n)
		names[n] = true
	}
	assert.Len(t, names, len(pairs))
}

// TestEdgeGatewayRouteNameUnique verifies per-Gateway route config names are unique.
func TestEdgeGatewayRouteNameUnique(t *testing.T) {
	n1 := EdgeGatewayRouteName("ns-a", "gw1")
	n2 := EdgeGatewayRouteName("ns-b", "gw1")
	n3 := EdgeGatewayRouteName("ns-a", "gw2")
	assert.NotEqual(t, n1, n2)
	assert.NotEqual(t, n1, n3)
	assert.NotEqual(t, n2, n3)
	assert.Equal(t, "edge_rt_ns-a_gw1", n1)
}

// TestBuildEdgeGatewayHTTPListener verifies the per-Gateway plain-HTTP listener:
// unique name, bound on the internal port, RDS to the per-Gateway route config.
func TestBuildEdgeGatewayHTTPListener(t *testing.T) {
	l := BuildEdgeGatewayHTTPListener("ns-a", "my-gw", 18150, false)
	assert.Equal(t, "edge_gw_ns-a_my-gw_18150", l.GetName())
	assert.Equal(t, uint32(18150), l.GetAddress().GetSocketAddress().GetPortValue())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, l.GetTrafficDirection())
	require.Len(t, l.GetFilterChains(), 1)

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	// Must reference the per-Gateway route config (not the shared "edge_http").
	assert.Equal(t, "edge_rt_ns-a_my-gw", hcm.GetRds().GetRouteConfigName())
	// Plain HTTP: no transport socket.
	assert.Nil(t, l.GetFilterChains()[0].GetTransportSocket())
}

// TestBuildEdgeGatewayHTTPListener_Redirect verifies the per-Gateway HTTP→HTTPS
// redirect listener uses an inline route config (no RDS) with the per-Gateway name.
func TestBuildEdgeGatewayHTTPListener_Redirect(t *testing.T) {
	l := BuildEdgeGatewayHTTPListener("ns-a", "my-gw", 18151, true)
	assert.Equal(t, "edge_gw_ns-a_my-gw_18151", l.GetName())
	assert.Equal(t, uint32(18151), l.GetAddress().GetSocketAddress().GetPortValue())
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	// Redirect: uses inline route config (no RDS reference).
	assert.Nil(t, hcm.GetRds(), "redirect must use inline route config, not RDS")
	rc := hcm.GetRouteConfig()
	require.NotNil(t, rc)
	red := rc.GetVirtualHosts()[0].GetRoutes()[0].GetRedirect()
	require.NotNil(t, red)
	assert.True(t, red.GetHttpsRedirect())
}

// TestBuildEdgeGatewayHTTPSListener verifies the per-Gateway HTTPS listener
// terminates TLS, uses the per-Gateway route config, and has the unique name.
func TestBuildEdgeGatewayHTTPSListener(t *testing.T) {
	l := BuildEdgeGatewayHTTPSListener("ns-b", "secure-gw", 18160, []string{"kubernetes/my-cert"})
	assert.Equal(t, "edge_gw_ns-b_secure-gw_18160", l.GetName())
	assert.Equal(t, uint32(18160), l.GetAddress().GetSocketAddress().GetPortValue())

	require.Len(t, l.GetFilterChains(), 1)
	fc := l.GetFilterChains()[0]
	// TLS transport socket must be set.
	require.NotNil(t, fc.GetTransportSocket())

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	// Routes via per-Gateway RDS route config.
	assert.Equal(t, "edge_rt_ns-b_secure-gw", hcm.GetRds().GetRouteConfigName())
}

// TestBuildEdgeGatewayRouteConfiguration verifies per-Gateway route config:
// correct name, provided vhosts, catch-all 404.
func TestBuildEdgeGatewayRouteConfiguration(t *testing.T) {
	vh := BuildEdgeVirtualHost("api.example.com", []string{"api.example.com"}, []*routev3.Route{})
	rc := BuildEdgeGatewayRouteConfiguration("ns-a", "gw1", []*routev3.VirtualHost{vh})

	assert.Equal(t, "edge_rt_ns-a_gw1", rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 2)
	assert.Equal(t, "api.example.com", rc.GetVirtualHosts()[0].GetName())
	last := rc.GetVirtualHosts()[len(rc.GetVirtualHosts())-1]
	assert.Equal(t, []string{"*"}, last.GetDomains())
	assert.Equal(t, uint32(404), last.GetRoutes()[0].GetDirectResponse().GetStatus())
}

// TestBuildEdgeRouteConfigurationSingleWildcard is the regression guard for the
// NACK "Only a single wildcard domain is permitted": when the input already has a
// "*" vhost (the hostname-less-route catch-all), the route config must NOT add a
// SECOND "*" — it appends the 404 to the existing one as the last route.
func TestBuildEdgeRouteConfigurationSingleWildcard(t *testing.T) {
	starRoute := BuildEdgeRoute("/echo", "", "echo.aether.internal", nil, nil, nil)
	star := BuildEdgeVirtualHost("*", []string{"*"}, []*routev3.Route{starRoute})
	hosted := BuildEdgeVirtualHost("api.example.com", []string{"api.example.com"}, []*routev3.Route{starRoute})

	for _, rc := range []*routev3.RouteConfiguration{
		BuildEdgeGatewayRouteConfiguration("ns-a", "gw1", []*routev3.VirtualHost{hosted, star}),
		BuildEdgeRouteConfiguration([]*routev3.VirtualHost{hosted, star}),
	} {
		wildcards := 0
		var starVH *routev3.VirtualHost
		for _, vh := range rc.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				if d == "*" {
					wildcards++
					starVH = vh
				}
			}
		}
		require.Equal(t, 1, wildcards, "exactly one * vhost (Envoy NACKs duplicates) in %s", rc.GetName())
		// The 404 fallback is appended to the existing "*" vhost as the last route.
		require.NotNil(t, starVH)
		require.Len(t, starVH.GetRoutes(), 2)
		assert.Equal(t, uint32(404), starVH.GetRoutes()[1].GetDirectResponse().GetStatus())
	}
}

// TestBuildEdgeTCPListener_SingleBackend verifies the TCP listener: INBOUND on 0.0.0.0,
// one filter chain with a tcp_proxy (single-cluster form), no TLS transport socket.
func TestBuildEdgeTCPListener_SingleBackend(t *testing.T) {
	backends := []L4Backend{
		{Service: "pg", Cluster: "tcp:pg.aether.internal", Weight: 1},
	}
	ln := BuildEdgeTCPListener(5432, backends)
	require.NotNil(t, ln)

	assert.Equal(t, "edge_tcp_5432", ln.GetName())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, ln.GetTrafficDirection())
	addr := ln.GetAddress().GetSocketAddress()
	assert.Equal(t, defaultEdgeAddress, addr.GetAddress())
	assert.Equal(t, uint32(5432), addr.GetPortValue())

	require.Len(t, ln.GetFilterChains(), 1)
	fc := ln.GetFilterChains()[0]
	assert.Nil(t, fc.GetTransportSocket(), "TCP listener: no downstream TLS (plain passthrough)")
	assert.Nil(t, fc.GetFilterChainMatch(), "single-backend TCP listener: no filter chain match")

	// The tcp_proxy must reference the backend cluster.
	require.Len(t, fc.GetFilters(), 1)
	tcpProxy := &tcp_proxyv3.TcpProxy{}
	require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(tcpProxy))
	assert.Equal(t, "tcp:pg.aether.internal", tcpProxy.GetCluster())
}

// TestBuildEdgeTCPListener_WeightedBackends verifies weighted_clusters form when
// multiple backends are present.
func TestBuildEdgeTCPListener_WeightedBackends(t *testing.T) {
	backends := []L4Backend{
		{Service: "pg-v1", Cluster: "tcp:pg-v1.aether.internal", Weight: 90},
		{Service: "pg-v2", Cluster: "tcp:pg-v2.aether.internal", Weight: 10},
	}
	ln := BuildEdgeTCPListener(5432, backends)
	require.NotNil(t, ln)

	fc := ln.GetFilterChains()[0]
	tcpProxy := &tcp_proxyv3.TcpProxy{}
	require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(tcpProxy))
	wc := tcpProxy.GetWeightedClusters()
	require.NotNil(t, wc)
	require.Len(t, wc.GetClusters(), 2)
	assert.Equal(t, "tcp:pg-v1.aether.internal", wc.GetClusters()[0].GetName())
	assert.Equal(t, uint32(90), wc.GetClusters()[0].GetWeight())
	assert.Equal(t, "tcp:pg-v2.aether.internal", wc.GetClusters()[1].GetName())
	assert.Equal(t, uint32(10), wc.GetClusters()[1].GetWeight())
}

// TestBuildEdgeTCPListener_EmptyBackends returns nil (no listener without backends).
func TestBuildEdgeTCPListener_EmptyBackends(t *testing.T) {
	assert.Nil(t, BuildEdgeTCPListener(5432, nil))
	assert.Nil(t, BuildEdgeTCPListener(5432, []L4Backend{}))
}

// TestBuildEdgeTLSPassthroughListener_SNIRouting verifies the TLS passthrough listener:
// tls_inspector listener filter, per-SNI filter chains (transport_protocol "tls",
// server_names match), tcp_proxy filter.
func TestBuildEdgeTLSPassthroughListener_SNIRouting(t *testing.T) {
	rules := []L4ServiceRoute{
		{
			SNIHostnames: []string{"db.example.com"},
			Backends:     []L4Backend{{Service: "db", Cluster: "tcp:db.aether.internal", Weight: 1}},
		},
		{
			SNIHostnames: []string{"analytics.example.com", "bi.example.com"},
			Backends:     []L4Backend{{Service: "analytics", Cluster: "tcp:analytics.aether.internal", Weight: 1}},
		},
	}
	ln := BuildEdgeTLSPassthroughListener(5433, rules)
	require.NotNil(t, ln)

	assert.Equal(t, "edge_tls_5433", ln.GetName())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, ln.GetTrafficDirection())
	assert.Equal(t, uint32(5433), ln.GetAddress().GetSocketAddress().GetPortValue())

	// tls_inspector listener filter must be present.
	require.Len(t, ln.GetListenerFilters(), 1)
	assert.Equal(t, listenerFilterTLSInspectorName, ln.GetListenerFilters()[0].GetName())

	// Two filter chains, one per rule.
	require.Len(t, ln.GetFilterChains(), 2)

	fc0 := ln.GetFilterChains()[0]
	assert.Equal(t, []string{"db.example.com"}, fc0.GetFilterChainMatch().GetServerNames())
	assert.Equal(t, "tls", fc0.GetFilterChainMatch().GetTransportProtocol())
	tcpProxy0 := &tcp_proxyv3.TcpProxy{}
	require.NoError(t, fc0.GetFilters()[0].GetTypedConfig().UnmarshalTo(tcpProxy0))
	assert.Equal(t, "tcp:db.aether.internal", tcpProxy0.GetCluster())

	fc1 := ln.GetFilterChains()[1]
	assert.Equal(t, []string{"analytics.example.com", "bi.example.com"}, fc1.GetFilterChainMatch().GetServerNames())
}

// TestBuildEdgeTLSPassthroughListener_EmptyRules returns nil.
func TestBuildEdgeTLSPassthroughListener_EmptyRules(t *testing.T) {
	assert.Nil(t, BuildEdgeTLSPassthroughListener(5433, nil))
	assert.Nil(t, BuildEdgeTLSPassthroughListener(5433, []L4ServiceRoute{
		{SNIHostnames: []string{"h"}, Backends: nil},
	}))
}

// TestBuildEdgeRoute_Redirect: BuildEdgeRoute with a non-nil GammaRedirect emits a
// Route_Redirect action (no cluster) with the correct fields.
func TestBuildEdgeRoute_Redirect(t *testing.T) {
	rd := &GammaRedirect{
		Scheme:     "https",
		Hostname:   "new.example.com",
		Port:       8443,
		StatusCode: 301,
		PathType:   "ReplaceFullPath",
		PathValue:  "/new-path",
	}
	r := BuildEdgeRoute("/old", "", "", nil, rd, nil)

	require.NotNil(t, r, "BuildEdgeRoute must return a route")
	assert.Nil(t, r.GetRoute(), "redirect route must not have a RouteAction")
	rdr := r.GetRedirect()
	require.NotNil(t, rdr, "redirect route must have a RedirectAction")
	assert.Equal(t, routev3.RedirectAction_MOVED_PERMANENTLY, rdr.GetResponseCode())
	assert.Equal(t, "https", rdr.GetSchemeRedirect())
	assert.Equal(t, "new.example.com", rdr.GetHostRedirect())
	assert.Equal(t, uint32(8443), rdr.GetPortRedirect())
	assert.Equal(t, "/new-path", rdr.GetPathRedirect())
}

// TestBuildEdgeRoute_URLRewrite: BuildEdgeRoute with a non-nil GammaURLRewrite sets
// the rewrite fields on the RouteAction (cluster is still used).
func TestBuildEdgeRoute_URLRewrite(t *testing.T) {
	rw := &GammaURLRewrite{
		Hostname:  "backend.internal",
		PathType:  "ReplacePrefixMatch",
		PathValue: "/v2",
	}
	r := BuildEdgeRoute("/api", "", "svc-1.aether.internal", nil, nil, rw)

	require.NotNil(t, r)
	ra := r.GetRoute()
	require.NotNil(t, ra, "URLRewrite route must have a RouteAction")
	assert.Nil(t, r.GetRedirect(), "URLRewrite route must not be a redirect")
	assert.Equal(t, "svc-1.aether.internal", ra.GetCluster())
	assert.Equal(t, "backend.internal", ra.GetHostRewriteLiteral())
	assert.Equal(t, "/v2", ra.GetPrefixRewrite())
}

// TestBuildEdgeRoute_URLRewrite_FullPath: ReplaceFullPath URLRewrite uses RegexRewrite.
func TestBuildEdgeRoute_URLRewrite_FullPath(t *testing.T) {
	rw := &GammaURLRewrite{PathType: "ReplaceFullPath", PathValue: "/fixed"}
	r := BuildEdgeRoute("/", "", "svc-1.aether.internal", nil, nil, rw)

	ra := r.GetRoute()
	require.NotNil(t, ra)
	rr := ra.GetRegexRewrite()
	require.NotNil(t, rr, "ReplaceFullPath must produce a RegexRewrite")
	assert.Equal(t, ".*", rr.GetPattern().GetRegex())
	assert.Equal(t, "/fixed", rr.GetSubstitution())
}

// TestEdgeUpstreamTCPTransportSocket verifies the edge TCP transport socket uses
// NO ALPN (not "h2" or "aether-tcp") and NO SNI so the destination inbound falls
// to the TCP floor DEFAULT chain, while SDS is fetched from spire_agent (not ADS).
func TestEdgeUpstreamTCPTransportSocket(t *testing.T) {
	ts := EdgeUpstreamTCPTransportSocket(
		"spiffe://aether.internal/ns/aether-ingress/sa/edge",
		"spiffe://aether.internal",
		nil,
	)
	utc := &transport_sockets_v3.UpstreamTlsContext{}
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(utc))

	common := utc.GetCommonTlsContext()
	assert.Empty(t, common.GetAlpnProtocols(), "must advertise no ALPN (TCP floor default chain)")
	assert.Empty(t, utc.GetSni(), "must send no SNI (TCP floor default chain)")

	grpc := common.GetTlsCertificateSdsSecretConfigs()[0].GetSdsConfig().GetApiConfigSource().GetGrpcServices()
	require.Len(t, grpc, 1)
	assert.Equal(t, SpireAgentSDSClusterName, grpc[0].GetEnvoyGrpc().GetClusterName(),
		"SDS must be fetched from spire_agent, not ADS")
}
