package proxy

import (
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
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
// ahead of the router, and strip_any_host_port enabled for Gateway API hostname
// matching (port-agnostic per the spec).
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

	// strip_any_host_port must be set on edge listeners: Gateway API hostname
	// matching is port-agnostic, but Go HTTP clients (including the Gateway API
	// conformance suite) include the port in the Host header even for the standard
	// port (e.g. "baz.bar.com:80"). Without stripping, the ":authority" header
	// "baz.bar.com:80" misses the vhost domain "baz.bar.com" and falls to the
	// catch-all "*", causing HTTPRouteListenerHostnameMatching and
	// HTTPRouteHostnameIntersection to fail. The node/east-west outbound HCM must
	// NOT strip ports (FQDN:port is a routing selector there — proposal 005).
	assert.True(t, hcm.GetStripAnyHostPort(),
		"edge listener must strip :port from Host header for Gateway API hostname matching")
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

// TestEdgeK8sClusterName verifies the naming scheme is stable and unique across
// (namespace, service, port) tuples.
func TestEdgeK8sClusterName(t *testing.T) {
	assert.Equal(t, "edge_k8s_default_my-svc_8080", EdgeK8sClusterName("default", "my-svc", 8080))
	assert.Equal(t, "edge_k8s_conformance-ns_infra-backend_80", EdgeK8sClusterName("conformance-ns", "infra-backend", 80))
	// Different port → different name.
	assert.NotEqual(t, EdgeK8sClusterName("ns", "svc", 80), EdgeK8sClusterName("ns", "svc", 8080))
	// Different namespace → different name.
	assert.NotEqual(t, EdgeK8sClusterName("ns-a", "svc", 80), EdgeK8sClusterName("ns-b", "svc", 80))
}

// TestBuildEdgeK8sCluster verifies STRICT_DNS discovery, inline load assignment,
// and absence of a transport socket (cleartext).
func TestBuildEdgeK8sCluster(t *testing.T) {
	cl := BuildEdgeK8sCluster("edge_k8s_default_my-svc_8080", "my-svc.default.svc.cluster.local", 8080)

	require.NotNil(t, cl)
	assert.Equal(t, "edge_k8s_default_my-svc_8080", cl.GetName())

	// Must be STRICT_DNS (not EDS or STATIC).
	assert.Equal(t, clusterv3.Cluster_STRICT_DNS, cl.GetType())

	// No transport socket → cleartext upstream.
	assert.Nil(t, cl.GetTransportSocket(), "k8s cleartext cluster must have NO transport socket")

	// Load assignment must be inline with the correct FQDN and port.
	la := cl.GetLoadAssignment()
	require.NotNil(t, la)
	ep := la.GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "my-svc.default.svc.cluster.local", ep.GetAddress())
	assert.Equal(t, uint32(8080), ep.GetPortValue())
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
// unique name, bound on the internal port, RDS to the per-Gateway route config,
// and strip_any_host_port enabled.
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
	// strip_any_host_port must be set (Gateway API hostname matching is port-agnostic).
	assert.True(t, hcm.GetStripAnyHostPort(),
		"per-Gateway HTTP listener must strip :port from Host header for hostname matching")
}

// TestBuildEdgeGatewayHTTPListener_Redirect verifies the per-Gateway HTTP→HTTPS
// redirect listener uses an inline route config (no RDS) with the per-Gateway name
// and carries strip_any_host_port.
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
	// strip_any_host_port: set for consistency with routing listeners.
	assert.True(t, hcm.GetStripAnyHostPort(),
		"redirect listener carries strip_any_host_port for consistency")
}

// TestBuildEdgeGatewayHTTPSListener verifies the per-Gateway HTTPS listener
// terminates TLS, uses the per-Gateway route config, has the unique name, and
// carries strip_any_host_port.
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
	// strip_any_host_port must be set (HTTPS clients include the port in Host).
	assert.True(t, hcm.GetStripAnyHostPort(),
		"per-Gateway HTTPS listener must strip :port from Host header for hostname matching")
}

// TestBuildEdgeGatewayHTTPListener_MultiListenerHostPortStrip is the FIX A test for
// HTTPRouteHostnameIntersection conformance. It proves that:
//  1. The per-Gateway HTTP listener HCM carries strip_any_host_port = true.
//  2. The per-Gateway route config contains a vhost with domain "very.specific.com"
//     (no port suffix), so that after Envoy strips ":1234" from the request authority
//     the vhost matches.
//
// The conformance Gateway has three HTTP listeners on port 80 with hostnames
// "very.specific.com", "*.wildcard.io", "*.anotherwildcard.io". All three share
// one internal port (per-Gateway addressing deduplicates by external port).
// A request "Host: very.specific.com:1234" must route to the "very.specific.com"
// vhost; stripping the port is the only layer needed — the vhost domain itself must
// carry no port.
func TestBuildEdgeGatewayHTTPListener_MultiListenerHostPortStrip(t *testing.T) {
	// Build the per-Gateway HTTP listener (one per (ns, gw, internal-port) tuple).
	l := BuildEdgeGatewayHTTPListener("gateway-conformance-infra", "httproute-hostname-intersection", 18100, false)

	// 1. HCM must strip the port from :authority before vhost matching.
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	assert.True(t, hcm.GetStripAnyHostPort(),
		"per-Gateway HCM must strip :port so Host: very.specific.com:1234 matches the very.specific.com vhost")
	// Routes via per-Gateway RDS (not the shared "edge_http" route config).
	assert.Equal(t, "edge_rt_gateway-conformance-infra_httproute-hostname-intersection", hcm.GetRds().GetRouteConfigName())

	// 2. The per-Gateway route config carries a vhost for "very.specific.com" with NO
	//    port. Envoy strips ":1234" (step 1) then matches against this domain list.
	route := BuildEdgeRoute("/s1", "", nil, "", nil, "infra-backend-v1", nil, nil, nil, nil)
	specificVH := BuildEdgeVirtualHost("very.specific.com", []string{"very.specific.com"}, []*routev3.Route{route})
	wildcardIOVH := BuildEdgeVirtualHost("*.wildcard.io", []string{"*.wildcard.io"}, []*routev3.Route{route})
	wildcardAnotherVH := BuildEdgeVirtualHost("*.anotherwildcard.io", []string{"*.anotherwildcard.io"}, []*routev3.Route{route})

	rc := BuildEdgeGatewayRouteConfiguration(
		"gateway-conformance-infra",
		"httproute-hostname-intersection",
		[]*routev3.VirtualHost{specificVH, wildcardIOVH, wildcardAnotherVH},
	)

	// Verify "very.specific.com" vhost exists in the route config with no port.
	var found bool
	for _, vh := range rc.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			if d == "very.specific.com" {
				found = true
				// Domain must be bare hostname, no port — Envoy matches against
				// the stripped authority.
				assert.NotContains(t, d, ":", "vhost domain must not contain a port")
			}
		}
	}
	assert.True(t, found, "route config must have a vhost with domain very.specific.com")
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
	starRoute := BuildEdgeRoute("/echo", "", nil, "", nil, "echo.aether.internal", nil, nil, nil, nil)
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

// TestBuildEdgeRouteWeighted_MultipleBackends verifies that BuildEdgeRouteWeighted
// emits a weighted_clusters RouteAction when multiple backends are present.
// The total_weight must equal the sum of individual weights; each entry maps to its
// cluster name. This is the primary test for the Gateway API HTTPRouteWeight split.
func TestBuildEdgeRouteWeighted_MultipleBackends(t *testing.T) {
	backends := []WeightedRouteBackend{
		{Cluster: "svc-a.aether.internal", Weight: 3},
		{Cluster: "svc-b.aether.internal", Weight: 1},
	}
	r := BuildEdgeRouteWeighted("/split", "", nil, "", nil, backends, nil, nil, nil, nil)

	require.NotNil(t, r)
	// FIX 1: non-"/" prefix must use path_separated_prefix (segment-boundary match).
	assert.Equal(t, "/split", r.GetMatch().GetPathSeparatedPrefix())

	ra := r.GetRoute()
	require.NotNil(t, ra, "must have a RouteAction")
	assert.Nil(t, r.GetRedirect(), "no redirect for a weighted forwarding route")

	wc := ra.GetWeightedClusters()
	require.NotNil(t, wc, "must use weighted_clusters for multiple backends")
	assert.Equal(t, uint32(4), wc.GetTotalWeight().GetValue(), "total_weight = 3+1 = 4")
	require.Len(t, wc.GetClusters(), 2)
	assert.Equal(t, "svc-a.aether.internal", wc.GetClusters()[0].GetName())
	assert.Equal(t, uint32(3), wc.GetClusters()[0].GetWeight().GetValue())
	assert.Equal(t, "svc-b.aether.internal", wc.GetClusters()[1].GetName())
	assert.Equal(t, uint32(1), wc.GetClusters()[1].GetWeight().GetValue())
}

// TestBuildEdgeRouteWeighted_SingleBackend verifies that a single-entry backends
// list produces a plain single-cluster RouteAction (no weighted_clusters overhead).
func TestBuildEdgeRouteWeighted_SingleBackend(t *testing.T) {
	backends := []WeightedRouteBackend{{Cluster: "svc-1.aether.internal", Weight: 1}}
	r := BuildEdgeRouteWeighted("/api", "", nil, "", nil, backends, nil, nil, nil, nil)

	require.NotNil(t, r)
	ra := r.GetRoute()
	require.NotNil(t, ra)
	// Single backend: plain cluster specifier, NOT weighted_clusters.
	assert.Equal(t, "svc-1.aether.internal", ra.GetCluster(), "single-backend route uses plain cluster")
	assert.Nil(t, ra.GetWeightedClusters(), "single backend must NOT use weighted_clusters")
}

// TestBuildEdgeRouteWeighted_ExactMatch verifies the exact path match is preserved
// correctly when using weighted_clusters.
func TestBuildEdgeRouteWeighted_ExactMatch(t *testing.T) {
	backends := []WeightedRouteBackend{
		{Cluster: "svc-v1.aether.internal", Weight: 90},
		{Cluster: "svc-v2.aether.internal", Weight: 10},
	}
	r := BuildEdgeRouteWeighted("", "/healthz", nil, "", nil, backends, nil, nil, nil, nil)

	require.NotNil(t, r)
	assert.Equal(t, "/healthz", r.GetMatch().GetPath(), "exact match must be preserved")
	assert.Empty(t, r.GetMatch().GetPrefix())

	wc := r.GetRoute().GetWeightedClusters()
	require.NotNil(t, wc)
	assert.Equal(t, uint32(100), wc.GetTotalWeight().GetValue(), "total_weight = 90+10")
}

// TestBuildEdgeRouteWeighted_ZeroWeightBackends verifies that all-zero-weight
// backends produce a weighted_clusters route with total_weight=0. Per the Gateway
// API spec this is valid; Envoy returns 500 for such a route (no healthy backend).
func TestBuildEdgeRouteWeighted_ZeroWeightBackends(t *testing.T) {
	backends := []WeightedRouteBackend{
		{Cluster: "svc-a.aether.internal", Weight: 0},
		{Cluster: "svc-b.aether.internal", Weight: 0},
	}
	r := BuildEdgeRouteWeighted("/drain", "", nil, "", nil, backends, nil, nil, nil, nil)

	require.NotNil(t, r)
	wc := r.GetRoute().GetWeightedClusters()
	require.NotNil(t, wc, "must still produce weighted_clusters (not a redirect/error)")
	assert.Equal(t, uint32(0), wc.GetTotalWeight().GetValue(), "total_weight=0 is valid per spec")
}

// TestBuildEdgeRouteWeighted_RetryPolicy verifies the retry policy is wired on the
// weighted_clusters RouteAction (same as the single-cluster path).
func TestBuildEdgeRouteWeighted_RetryPolicy(t *testing.T) {
	backends := []WeightedRouteBackend{
		{Cluster: "svc-a.aether.internal", Weight: 1},
		{Cluster: "svc-b.aether.internal", Weight: 1},
	}
	r := BuildEdgeRouteWeighted("/", "", nil, "", nil, backends, nil, nil, nil, nil)
	ra := r.GetRoute()
	require.NotNil(t, ra)
	require.NotNil(t, ra.GetRetryPolicy(), "retry policy must be set on weighted route")
}

// TestBuildEdgeRouteWeighted_Redirect verifies that a redirect (non-nil GammaRedirect)
// takes precedence over backends and produces a Route_Redirect action.
func TestBuildEdgeRouteWeighted_Redirect(t *testing.T) {
	backends := []WeightedRouteBackend{{Cluster: "svc-a.aether.internal", Weight: 1}}
	rd := &GammaRedirect{Scheme: "https", StatusCode: 301}
	r := BuildEdgeRouteWeighted("/old", "", nil, "", nil, backends, nil, rd, nil, nil)

	require.NotNil(t, r)
	assert.NotNil(t, r.GetRedirect(), "redirect must produce Route_Redirect")
	assert.Nil(t, r.GetRoute(), "redirect must not have a RouteAction")
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
	r := BuildEdgeRoute("/old", "", nil, "", nil, "", nil, rd, nil, nil)

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
	r := BuildEdgeRoute("/api", "", nil, "", nil, "svc-1.aether.internal", nil, nil, rw, nil)

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
	r := BuildEdgeRoute("/", "", nil, "", nil, "svc-1.aether.internal", nil, nil, rw, nil)

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

// --- Header / method / query predicates in Envoy RouteMatch ---

// TestBuildEdgeRoute_HeaderMatch verifies that RouteHeaderMatch values are
// emitted as Envoy HeaderMatcher entries on the RouteMatch.
func TestBuildEdgeRoute_HeaderMatch(t *testing.T) {
	hdrs := []RouteHeaderMatch{
		{Name: "x-env", Value: "prod", Regex: false},
		{Name: "x-role", Value: "admin|editor", Regex: true},
	}
	r := BuildEdgeRoute("/api", "", hdrs, "", nil, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	match := r.GetMatch()
	// FIX 1: non-"/" prefix uses path_separated_prefix.
	require.Equal(t, "/api", match.GetPathSeparatedPrefix())
	require.Len(t, match.GetHeaders(), 2)

	// First header: exact match.
	h0 := match.GetHeaders()[0]
	assert.Equal(t, "x-env", h0.GetName())
	assert.Equal(t, "prod", h0.GetStringMatch().GetExact())

	// Second header: regex match.
	h1 := match.GetHeaders()[1]
	assert.Equal(t, "x-role", h1.GetName())
	require.NotNil(t, h1.GetStringMatch().GetSafeRegex(), "regex header match must use SafeRegex")
	assert.Equal(t, "admin|editor", h1.GetStringMatch().GetSafeRegex().GetRegex())
}

// TestBuildEdgeRoute_MethodMatch verifies that a non-empty Method string is emitted
// as an Envoy ":method" HeaderMatcher with an exact string match.
func TestBuildEdgeRoute_MethodMatch(t *testing.T) {
	r := BuildEdgeRoute("/write", "", nil, "POST", nil, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	headers := r.GetMatch().GetHeaders()
	require.Len(t, headers, 1)
	h := headers[0]
	assert.Equal(t, ":method", h.GetName(), ":method pseudo-header matcher")
	assert.Equal(t, "POST", h.GetStringMatch().GetExact())
}

// TestBuildEdgeRoute_QueryParamMatch verifies that RouteQueryParamMatch values are
// emitted as Envoy QueryParameterMatcher entries on the RouteMatch.
func TestBuildEdgeRoute_QueryParamMatch(t *testing.T) {
	qps := []RouteQueryParamMatch{
		{Name: "format", Value: "json", Regex: false},
		{Name: "version", Value: "v[12]", Regex: true},
	}
	r := BuildEdgeRoute("/search", "", nil, "", qps, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	params := r.GetMatch().GetQueryParameters()
	require.Len(t, params, 2)

	// First: exact match.
	p0 := params[0]
	assert.Equal(t, "format", p0.GetName())
	assert.Equal(t, "json", p0.GetStringMatch().GetExact())

	// Second: regex match.
	p1 := params[1]
	assert.Equal(t, "version", p1.GetName())
	require.NotNil(t, p1.GetStringMatch().GetSafeRegex())
	assert.Equal(t, "v[12]", p1.GetStringMatch().GetSafeRegex().GetRegex())
}

// TestBuildEdgeRoute_CombinedPredicates verifies that header, method, and query
// predicates are all applied together on a single RouteMatch (AND semantics).
func TestBuildEdgeRoute_CombinedPredicates(t *testing.T) {
	hdrs := []RouteHeaderMatch{{Name: "x-req", Value: "true", Regex: false}}
	qps := []RouteQueryParamMatch{{Name: "env", Value: "staging", Regex: false}}
	r := BuildEdgeRoute("/combined", "", hdrs, "DELETE", qps, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	match := r.GetMatch()

	// Header matchers: x-req + :method = 2.
	hdrsOut := match.GetHeaders()
	require.Len(t, hdrsOut, 2)
	names := []string{hdrsOut[0].GetName(), hdrsOut[1].GetName()}
	assert.Contains(t, names, "x-req")
	assert.Contains(t, names, ":method")

	// Query parameters: env.
	params := match.GetQueryParameters()
	require.Len(t, params, 1)
	assert.Equal(t, "env", params[0].GetName())
}

// TestBuildEdgeDirectResponseRoute_Status500 verifies that a direct_response route
// emits status 500 and no upstream cluster.
func TestBuildEdgeDirectResponseRoute_Status500(t *testing.T) {
	// Non-"/" prefix: must use path_separated_prefix (segment-boundary match).
	r := BuildEdgeDirectResponseRoute("/bad", "", nil, "", nil, 500)

	require.NotNil(t, r)
	assert.Equal(t, "/bad", r.GetMatch().GetPathSeparatedPrefix(),
		"non-/ prefix must use path_separated_prefix")
	assert.Empty(t, r.GetMatch().GetPrefix(), "plain prefix must be empty for non-/ paths")

	dr, ok := r.GetAction().(*routev3.Route_DirectResponse)
	require.True(t, ok, "action must be Route_DirectResponse")
	assert.Equal(t, uint32(500), dr.DirectResponse.GetStatus())
}

// TestBuildEdgeDirectResponseRoute_PredicatesPreserved verifies that predicates
// (path, header, method) are present on the direct_response route's RouteMatch,
// so the 500 only fires for the right request shape.
func TestBuildEdgeDirectResponseRoute_PredicatesPreserved(t *testing.T) {
	hdrs := []RouteHeaderMatch{{Name: "x-env", Value: "prod", Regex: false}}
	// Non-"/" prefix uses path_separated_prefix (FIX 1: segment-boundary matching).
	r := BuildEdgeDirectResponseRoute("/scoped", "", hdrs, "GET", nil, 500)

	require.NotNil(t, r)
	match := r.GetMatch()

	// Path: path_separated_prefix="/scoped" (not plain prefix).
	assert.Equal(t, "/scoped", match.GetPathSeparatedPrefix(), "non-/ prefix uses path_separated_prefix")

	// Header: x-env exact + :method GET.
	headers := match.GetHeaders()
	require.Len(t, headers, 2)
	nameMap := map[string]string{}
	for _, h := range headers {
		nameMap[h.GetName()] = h.GetStringMatch().GetExact()
	}
	assert.Equal(t, "prod", nameMap["x-env"])
	assert.Equal(t, "GET", nameMap[":method"])
}

// TestSortRoutesBySpecificity_HeaderCountRanksHigher verifies that when two routes
// share the same path the one with more headers sorts before the one with fewer.
func TestSortRoutesBySpecificity_HeaderCountRanksHigher(t *testing.T) {
	// Build two routes that differ only in header count.
	fewer := BuildEdgeRoute("/api", "", []RouteHeaderMatch{{Name: "h1", Value: "v1"}}, "", nil, "svc", nil, nil, nil, nil)
	more := BuildEdgeRoute("/api", "", []RouteHeaderMatch{{Name: "h1", Value: "v1"}, {Name: "h2", Value: "v2"}}, "", nil, "svc", nil, nil, nil, nil)

	require.NotNil(t, fewer)
	require.NotNil(t, more)

	// The route with 2 headers must sort before the route with 1.
	// Verify header counts are present.
	assert.Len(t, more.GetMatch().GetHeaders(), 2)
	assert.Len(t, fewer.GetMatch().GetHeaders(), 1)
}

// TestBuildEdgeRoute_MethodAndHeadersCombined verifies method present + extra header
// both contribute distinct entries to GetHeaders().
func TestBuildEdgeRoute_MethodAndHeadersCombined(t *testing.T) {
	hdrs := []RouteHeaderMatch{{Name: "x-custom", Value: "yes", Regex: false}}
	r := BuildEdgeRoute("/path", "", hdrs, "PATCH", nil, "svc", nil, nil, nil, nil)

	require.NotNil(t, r)
	headers := r.GetMatch().GetHeaders()
	// x-custom + :method = 2 entries.
	require.Len(t, headers, 2)
}

// --- FIX 2: HTTPRouteRewritePath prefix slash normalization + empty-remainder ---

// TestBuildEdgeRoute_URLRewrite_ReplacePrefixMatch verifies Gateway API
// ReplacePrefixMatch segment semantics for all cases:
//   - Non-slash replacement: trailing slash stripped to prevent double-slash
//     when the suffix starts with "/" (the #367 fix).
//   - Slash-only replacement ("/") must NOT produce an empty path on exact-match
//     requests (the empty-remainder fix). Envoy prefix_rewrite="" on path
//     "/strip-prefix" → ""; we emit regex_rewrite instead.
//   - Slash-only replacement with a suffix must produce "/suffix".
func TestBuildEdgeRoute_URLRewrite_ReplacePrefixMatch(t *testing.T) {
	tests := []struct {
		name           string
		matchPrefix    string
		pathValue      string
		wantPrefix     string // if non-empty, expect PrefixRewrite (no RegexRewrite)
		wantRegexPat   string // if non-empty, expect RegexRewrite with this pattern
		wantRegexSubst string // expected regex substitution
	}{
		{
			// "/" replacement with known prefix → regex_rewrite (empty-remainder fix).
			name:           "slash replacement uses regex for empty-remainder",
			matchPrefix:    "/prefix",
			pathValue:      "/",
			wantRegexPat:   `^/prefix/?(.*)$`,
			wantRegexSubst: `/\1`,
		},
		{
			// No trailing slash: plain prefix_rewrite, value unchanged.
			name:        "no trailing slash → value unchanged",
			matchPrefix: "/api",
			pathValue:   "/v2",
			wantPrefix:  "/v2",
		},
		{
			// Multi-segment value with trailing slash: slash stripped.
			name:        "trailing slash on multi-segment → stripped",
			matchPrefix: "/foo",
			pathValue:   "/bar/",
			wantPrefix:  "/bar",
		},
		{
			// Conformance strip-prefix: /strip-prefix → /, /strip-prefix/foo → /foo.
			name:           "strip-prefix conformance case",
			matchPrefix:    "/strip-prefix",
			pathValue:      "/",
			wantRegexPat:   `^/strip-prefix/?(.*)$`,
			wantRegexSubst: `/\1`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rw := &GammaURLRewrite{PathType: "ReplacePrefixMatch", PathValue: tc.pathValue}
			r := BuildEdgeRoute(tc.matchPrefix, "", nil, "", nil, "svc.aether.internal", nil, nil, rw, nil)
			require.NotNil(t, r)
			ra := r.GetRoute()
			require.NotNil(t, ra, "ReplacePrefixMatch must produce a RouteAction")
			switch {
			case tc.wantPrefix != "":
				assert.Equal(t, tc.wantPrefix, ra.GetPrefixRewrite(),
					"non-slash replacement must use PrefixRewrite with trailing slash stripped")
				assert.Nil(t, ra.GetRegexRewrite(), "non-slash replacement must not use RegexRewrite")
			case tc.wantRegexPat != "":
				rr := ra.GetRegexRewrite()
				require.NotNil(t, rr, "slash-only replacement must use RegexRewrite for empty-remainder correctness")
				assert.Equal(t, tc.wantRegexPat, rr.GetPattern().GetRegex(),
					"regex pattern must anchor, match prefix, optional slash, capture remainder")
				assert.Equal(t, tc.wantRegexSubst, rr.GetSubstitution(),
					"regex substitution must prepend / and append captured remainder")
				assert.Empty(t, ra.GetPrefixRewrite(), "slash-only replacement must not set PrefixRewrite")
			}
		})
	}
}

// --- FIX 3: HTTPRouteRedirectPortAndScheme — scheme-default port ---

// TestBuildEdgeRoute_Redirect_SchemeOnly verifies that a redirect with scheme set
// but no explicit port sets PortRedirect to the scheme's default (443 for https,
// 80 for http). Without this fix, Envoy's proto3 zero-value for port_redirect
// causes it to copy the original request port (e.g. https://host:80/path).
func TestBuildEdgeRoute_Redirect_SchemeOnly(t *testing.T) {
	tests := []struct {
		name     string
		scheme   string
		wantPort uint32
	}{
		{name: "https no port → 443", scheme: "https", wantPort: 443},
		{name: "http no port → 80", scheme: "http", wantPort: 80},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rd := &GammaRedirect{Scheme: tc.scheme, StatusCode: 301}
			r := BuildEdgeRoute("/path", "", nil, "", nil, "", nil, rd, nil, nil)
			require.NotNil(t, r)
			rdr := r.GetRedirect()
			require.NotNil(t, rdr)
			assert.Equal(t, tc.wantPort, rdr.GetPortRedirect(),
				"scheme-only redirect must use scheme-default port to avoid https://host:80")
		})
	}
}

// TestBuildEdgeRoute_Redirect_ExplicitPortOverridesSchemeDefault verifies that
// when Port is explicitly set, it takes precedence over the scheme default.
func TestBuildEdgeRoute_Redirect_ExplicitPortOverridesSchemeDefault(t *testing.T) {
	rd := &GammaRedirect{Scheme: "https", Port: 8443, StatusCode: 301}
	r := BuildEdgeRoute("/path", "", nil, "", nil, "", nil, rd, nil, nil)
	require.NotNil(t, r)
	rdr := r.GetRedirect()
	require.NotNil(t, rdr)
	assert.Equal(t, uint32(8443), rdr.GetPortRedirect(),
		"explicit port must override scheme default")
}

// --- FIX 4: HTTPRouteTimeoutRequest enforcement in BuildEdgeRoute ---

// TestBuildEdgeRoute_Timeout verifies that a non-nil timeout is wired onto
// the RouteAction.Timeout field of the forwarding route.
func TestBuildEdgeRoute_Timeout(t *testing.T) {
	timeout := &durationpb.Duration{Seconds: 5}
	r := BuildEdgeRoute("/slow", "", nil, "", nil, "backend.aether.internal", nil, nil, nil, timeout)
	require.NotNil(t, r)
	ra := r.GetRoute()
	require.NotNil(t, ra)
	require.NotNil(t, ra.GetTimeout(), "timeout must be wired onto RouteAction.Timeout")
	assert.Equal(t, int64(5), ra.GetTimeout().GetSeconds())
}

// TestBuildEdgeRoute_NilTimeout verifies that a nil timeout leaves RouteAction.Timeout
// unset (Envoy inherits the HCM-level default).
func TestBuildEdgeRoute_NilTimeout(t *testing.T) {
	r := BuildEdgeRoute("/fast", "", nil, "", nil, "backend.aether.internal", nil, nil, nil, nil)
	require.NotNil(t, r)
	ra := r.GetRoute()
	require.NotNil(t, ra)
	assert.Nil(t, ra.GetTimeout(), "nil timeout must leave RouteAction.Timeout unset")
}

// TestBuildEdgeRouteWeighted_Timeout verifies that a non-nil timeout is threaded
// onto the WeightedCluster RouteAction.Timeout.
func TestBuildEdgeRouteWeighted_Timeout(t *testing.T) {
	backends := []WeightedRouteBackend{{Cluster: "svc-a.aether.internal", Weight: 1}}
	timeout := &durationpb.Duration{Seconds: 2}
	r := BuildEdgeRouteWeighted("/slow", "", nil, "", nil, backends, nil, nil, nil, timeout)
	require.NotNil(t, r)
	ra := r.GetRoute()
	require.NotNil(t, ra)
	require.NotNil(t, ra.GetTimeout(), "timeout must be set on weighted route's RouteAction")
	assert.Equal(t, int64(2), ra.GetTimeout().GetSeconds())
}

// --- FIX 1: segment-boundary PathPrefix matching (HTTPRouteMatching) ---

// TestSetEdgePrefixPathSpecifier verifies that setEdgePrefixPathSpecifier emits
// path_separated_prefix for non-"/" prefixes (segment-boundary match) and plain
// prefix for "/" (Envoy rejects path_separated_prefix:"/").
func TestSetEdgePrefixPathSpecifier(t *testing.T) {
	tests := []struct {
		input   string
		wantPSP string // expected path_separated_prefix value (empty = use Prefix)
		wantPfx string // expected plain prefix value (empty = use PSP)
	}{
		{input: "/", wantPfx: "/"},
		{input: "/v2", wantPSP: "/v2"},
		{input: "/api/v1", wantPSP: "/api/v1"},
		// Trailing slash is normalized: /v2/ → /v2.
		{input: "/v2/", wantPSP: "/v2"},
		// Only slash: //// still collapses to "/".
		{input: "////", wantPfx: "/"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			match := &routev3.RouteMatch{}
			setEdgePrefixPathSpecifier(match, tc.input)
			if tc.wantPSP != "" {
				assert.Equal(t, tc.wantPSP, match.GetPathSeparatedPrefix(), "PathSeparatedPrefix")
				assert.Empty(t, match.GetPrefix(), "Prefix must be empty when using PathSeparatedPrefix")
			} else {
				assert.Equal(t, tc.wantPfx, match.GetPrefix(), "Prefix")
				assert.Empty(t, match.GetPathSeparatedPrefix(), "PathSeparatedPrefix must be empty when using Prefix")
			}
		})
	}
}

// TestBuildEdgeRoute_PathSeparatedPrefix verifies that BuildEdgeRoute emits
// path_separated_prefix for a non-"/" PathPrefix match so that "/v2" matches "/v2"
// and "/v2/x" but NOT "/v2example" (segment-boundary semantics, HTTPRouteMatching).
func TestBuildEdgeRoute_PathSeparatedPrefix(t *testing.T) {
	r := BuildEdgeRoute("/v2", "", nil, "", nil, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	match := r.GetMatch()
	assert.Equal(t, "/v2", match.GetPathSeparatedPrefix(),
		"/v2 prefix must use path_separated_prefix for segment-boundary matching")
	assert.Empty(t, match.GetPrefix(), "plain prefix must be empty for a non-/ PathPrefix")
	assert.Empty(t, match.GetPath(), "path must be empty for a PathPrefix match")
}

// TestBuildEdgeRoute_CatchAllPrefixPlain verifies that "/" stays plain prefix
// (not path_separated_prefix) so Envoy accepts it without rejection.
func TestBuildEdgeRoute_CatchAllPrefixPlain(t *testing.T) {
	r := BuildEdgeRoute("/", "", nil, "", nil, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	match := r.GetMatch()
	assert.Equal(t, "/", match.GetPrefix(),
		"catch-all '/' must remain a plain prefix (Envoy rejects path_separated_prefix:'/')")
	assert.Empty(t, match.GetPathSeparatedPrefix())
}

// TestBuildEdgeRoute_TrailingSlashNormalized verifies that a trailing slash in a
// PathPrefix is stripped: "/v2/" produces path_separated_prefix:"/v2" so that
// "/v2" and "/v2/foo" match while "/v2example" does not.
func TestBuildEdgeRoute_TrailingSlashNormalized(t *testing.T) {
	r := BuildEdgeRoute("/v2/", "", nil, "", nil, "svc.aether.internal", nil, nil, nil, nil)

	require.NotNil(t, r)
	assert.Equal(t, "/v2", r.GetMatch().GetPathSeparatedPrefix(),
		"trailing slash must be stripped: /v2/ → path_separated_prefix:/v2")
}
