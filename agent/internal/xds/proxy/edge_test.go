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
	l := BuildEdgeListener(8080, nil)

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
	l := BuildEdgeListener(8443, []string{"kubernetes/api-tls", "kubernetes/foo-tls"})

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
	l := BuildEdgeListener(8080, nil)
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
