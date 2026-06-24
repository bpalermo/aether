package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCaptureListener(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, nil)
	require.NoError(t, err)

	assert.Equal(t, "capture_p1", l.GetName())
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, "0.0.0.0", sa.GetAddress())
	assert.Equal(t, uint32(15001), sa.GetPortValue())
	assert.Equal(t, "/var/run/netns/p1", sa.GetNetworkNamespaceFilepath())
	assert.True(t, l.GetUseOriginalDst().GetValue(), "use_original_dst set")

	// listener filters: original_dst, http_inspector, tls_inspector.
	require.Len(t, l.GetListenerFilters(), 3)
	assert.Equal(t, listenerFilterOriginalDstName, l.GetListenerFilters()[0].GetName())
	assert.Equal(t, listenerFilterHTTPInspectorName, l.GetListenerFilters()[1].GetName())
	assert.Equal(t, listenerFilterTLSInspectorName, l.GetListenerFilters()[2].GetName())

	// no TCP services → single HCM filter chain.
	require.Len(t, l.GetFilterChains(), 1)
	filters := l.GetFilterChains()[0].GetFilters()
	hcmFilter := filters[len(filters)-1]
	var hcm http_connection_managerv3.HttpConnectionManager
	require.NoError(t, hcmFilter.GetTypedConfig().UnmarshalTo(&hcm))
	assert.Equal(t, CaptureHTTPRouteName, hcm.GetRds().GetRouteConfigName())
}

func TestGenerateCaptureListener_RequiresNetns(t *testing.T) {
	_, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1"}, 15001, "aether.internal", false, nil)
	assert.Error(t, err)
}

// TestGenerateCaptureListener_WithTCPServices verifies that non-HTTP services produce
// per-ClusterIP TCP floor chains in addition to the global HCM catch-all.
func TestGenerateCaptureListener_WithTCPServices(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	tcpSvcs := []CaptureTCPService{
		{ClusterName: "tcp:svc-a.aether.internal", ClusterIP: "10.96.1.10"},
		{ClusterName: "tcp:svc-b.aether.internal", ClusterIP: "10.96.1.20"},
	}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, tcpSvcs)
	require.NoError(t, err)

	// 2 TCP floor chains + 1 HCM catch-all.
	require.Len(t, l.GetFilterChains(), 3, "one chain per TCP service plus the HCM catch-all")

	// Collect chains by match type.
	var tcpChains []*listenerv3.FilterChain
	var hcmChain *listenerv3.FilterChain
	for _, fc := range l.GetFilterChains() {
		if len(fc.GetFilterChainMatch().GetPrefixRanges()) > 0 {
			tcpChains = append(tcpChains, fc)
		} else {
			hcmChain = fc
		}
	}
	require.Len(t, tcpChains, 2)
	require.NotNil(t, hcmChain)

	// TCP chains: keyed by /32 prefix_range, single tcp_proxy filter.
	ips := map[string]bool{}
	for _, fc := range tcpChains {
		require.Len(t, fc.GetFilterChainMatch().GetPrefixRanges(), 1)
		ip := fc.GetFilterChainMatch().GetPrefixRanges()[0].GetAddressPrefix()
		ips[ip] = true
		assert.Equal(t, uint32(32), fc.GetFilterChainMatch().GetPrefixRanges()[0].GetPrefixLen().GetValue())
		require.Len(t, fc.GetFilters(), 2, "namespace filter + tcp_proxy")
		assert.Equal(t, "envoy.filters.network.tcp_proxy", fc.GetFilters()[1].GetName())
	}
	assert.True(t, ips["10.96.1.10"], "svc-a ClusterIP chain present")
	assert.True(t, ips["10.96.1.20"], "svc-b ClusterIP chain present")

	// HCM catch-all: no prefix_ranges, not matched on application_protocols.
	assert.Empty(t, hcmChain.GetFilterChainMatch().GetPrefixRanges())
}

// TestGenerateCaptureListener_InvalidTCPService verifies that TCP services with
// empty or unparseable ClusterIPs are silently skipped (no nil chains emitted).
func TestGenerateCaptureListener_InvalidTCPService(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	tcpSvcs := []CaptureTCPService{
		{ClusterName: "tcp:svc-a.aether.internal", ClusterIP: ""},           // missing IP
		{ClusterName: "", ClusterIP: "10.96.1.10"},                          // missing name
		{ClusterName: "tcp:svc-b.aether.internal", ClusterIP: "not-an-ip"},  // bad IP
		{ClusterName: "tcp:svc-c.aether.internal", ClusterIP: "10.96.1.30"}, // valid
	}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, tcpSvcs)
	require.NoError(t, err)

	// Only the valid service should produce a chain; 1 TCP + 1 HCM.
	require.Len(t, l.GetFilterChains(), 2)
}

func TestBuildCaptureRouteConfiguration(t *testing.T) {
	// cluster.local authority -> service cluster, reusing the outbound vhost builder.
	vh := BuildOutboundClusterVirtualHost(
		ServiceClusterName("svc-1", "aether.internal"),
		[]string{"svc-1.aether-test.svc.cluster.local", "svc-1.aether-test.svc.cluster.local:18081"},
	)
	rc := BuildCaptureRouteConfiguration([]*routev3.VirtualHost{vh}, "aether.internal")
	assert.Equal(t, CaptureHTTPRouteName, rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 2, "the service vhost + the on-demand catch-all")

	got := rc.GetVirtualHosts()[0]
	assert.Contains(t, got.GetDomains(), "svc-1.aether-test.svc.cluster.local:18081")
	assert.Equal(t, "svc-1.aether.internal", got.GetRoutes()[0].GetRoute().GetCluster())

	// The catch-all recovers cold/off-node mesh services via ODCDS (ClusterHeader
	// on :authority) and 404s only non-mesh authorities — symmetric with outbound.
	catchAll := rc.GetVirtualHosts()[1]
	assert.Equal(t, []string{"*"}, catchAll.GetDomains())
	var hasOnDemand, has404 bool
	for _, r := range catchAll.GetRoutes() {
		if r.GetRoute().GetClusterHeader() == ":authority" {
			hasOnDemand = true
		}
		if r.GetDirectResponse().GetStatus() == 404 {
			has404 = true
		}
	}
	assert.True(t, hasOnDemand, "catch-all routes mesh authorities to ODCDS via :authority")
	assert.True(t, has404, "catch-all 404s non-mesh authorities")
}
