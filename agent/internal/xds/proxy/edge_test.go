package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
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
	l := BuildEdgeListener(8080)

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
