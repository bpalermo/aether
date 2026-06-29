package proxy

import (
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCaptureListener(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, nil, false, nil)
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

	// no TCP services → single HCM filter chain; no DefaultFilterChain (withPassthrough=false).
	require.Len(t, l.GetFilterChains(), 1)
	assert.Nil(t, l.GetDefaultFilterChain(), "no DefaultFilterChain when withPassthrough=false")
	filters := l.GetFilterChains()[0].GetFilters()
	hcmFilter := filters[len(filters)-1]
	var hcm http_connection_managerv3.HttpConnectionManager
	require.NoError(t, hcmFilter.GetTypedConfig().UnmarshalTo(&hcm))
	assert.Equal(t, CaptureHTTPRouteName, hcm.GetRds().GetRouteConfigName())
}

func TestGenerateCaptureListener_RequiresNetns(t *testing.T) {
	_, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1"}, 15001, "aether.internal", false, nil, false, nil)
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
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, tcpSvcs, false, nil)
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
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, tcpSvcs, false, nil)
	require.NoError(t, err)

	// Only the valid service should produce a chain; 1 TCP + 1 HCM.
	require.Len(t, l.GetFilterChains(), 2)
}

// TestGenerateCaptureListener_WithPassthrough verifies that withPassthrough=true adds
// a DefaultFilterChain routing to the passthrough_original_dst cluster, without adding
// an extra named chain (DefaultFilterChain is separate from filter_chains).
func TestGenerateCaptureListener_WithPassthrough(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, nil, true, nil)
	require.NoError(t, err)

	// Named filter_chains: only the HCM chain (no TCP services).
	require.Len(t, l.GetFilterChains(), 1, "one HCM chain, passthrough is in DefaultFilterChain")

	// With passthrough, the HCM chain is scoped to cleartext (transport_protocol=
	// raw_buffer) so TLS egress (transport_protocol=tls) misses it and falls to the
	// DefaultFilterChain passthrough instead of failing in the HTTP codec.
	assert.Equal(t, "raw_buffer", l.GetFilterChains()[0].GetFilterChainMatch().GetTransportProtocol(),
		"HCM chain must match raw_buffer so TLS egress falls through to the passthrough")

	// DefaultFilterChain must be set.
	require.NotNil(t, l.GetDefaultFilterChain(), "DefaultFilterChain must be set when withPassthrough=true")
	dfc := l.GetDefaultFilterChain()
	assert.Equal(t, "cap_passthrough", dfc.GetName())

	// DefaultFilterChain must have a single tcp_proxy filter routing to passthrough_original_dst.
	require.Len(t, dfc.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", dfc.GetFilters()[0].GetName())
	var tp tcp_proxyv3.TcpProxy
	require.NoError(t, dfc.GetFilters()[0].GetTypedConfig().UnmarshalTo(&tp))
	assert.Equal(t, PassthroughClusterName, tp.GetCluster())
	assert.Equal(t, "cap_passthrough", tp.GetStatPrefix())
}

// TestGenerateCaptureListener_WithPassthroughAndTCPServices verifies that named
// TCP floor chains coexist with the DefaultFilterChain passthrough when both are
// enabled. The named chains take precedence (Envoy evaluates them first).
func TestGenerateCaptureListener_WithPassthroughAndTCPServices(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	tcpSvcs := []CaptureTCPService{
		{ClusterName: "tcp:svc-a.aether.internal", ClusterIP: "10.96.1.10"},
	}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false, tcpSvcs, true, nil)
	require.NoError(t, err)

	// Named chains: 1 TCP floor + 1 HCM.
	require.Len(t, l.GetFilterChains(), 2)
	// DefaultFilterChain carries the passthrough.
	require.NotNil(t, l.GetDefaultFilterChain())
	assert.Equal(t, "cap_passthrough", l.GetDefaultFilterChain().GetName())
}

// TestNewPassthroughOriginalDstCluster verifies the passthrough cluster is an
// ORIGINAL_DST cluster with the correct name and no transport socket.
func TestNewPassthroughOriginalDstCluster(t *testing.T) {
	cl := NewPassthroughOriginalDstCluster()
	require.NotNil(t, cl)
	assert.Equal(t, PassthroughClusterName, cl.GetName())
	// Must be ORIGINAL_DST type (4).
	assert.Equal(t, int32(4), int32(cl.GetType()))
	// ORIGINAL_DST REQUIRES CLUSTER_PROVIDED LB — any other policy (the
	// ROUND_ROBIN default) makes Envoy reject the cluster and NACK CDS.
	assert.Equal(t, clusterv3.Cluster_CLUSTER_PROVIDED, cl.GetLbPolicy())
	// No transport socket — passthrough is plaintext.
	assert.Nil(t, cl.GetTransportSocket())
	// No upstream HTTP protocol options — raw TCP.
	assert.Empty(t, cl.GetTypedExtensionProtocolOptions())

	// SO_MARK on the upstream sockets (proposal 022 M2-default): the CNI
	// redirect-all rule RETURNs this mark so the proxy's own forwarded egress is
	// not re-captured. Only socket_options (no source_address) so the bind/netns
	// are unchanged.
	opts := cl.GetUpstreamBindConfig().GetSocketOptions()
	require.Len(t, opts, 1)
	assert.Equal(t, int64(1), opts[0].GetLevel()) // SOL_SOCKET
	assert.Equal(t, int64(36), opts[0].GetName()) // SO_MARK
	assert.Equal(t, int64(constants.CapturePassthroughFwMark), opts[0].GetIntValue())
	assert.Equal(t, corev3.SocketOption_STATE_PREBIND, opts[0].GetState())
	assert.Nil(t, cl.GetUpstreamBindConfig().GetSourceAddress(), "no source_address — bind unchanged")
}

// TestBuildCapturePassthroughFilterChain verifies the DefaultFilterChain structure.
func TestBuildCapturePassthroughFilterChain(t *testing.T) {
	fc := BuildCapturePassthroughFilterChain()
	require.NotNil(t, fc)
	assert.Equal(t, "cap_passthrough", fc.GetName())
	// No FilterChainMatch: this is used as DefaultFilterChain, not a named chain.
	assert.Nil(t, fc.GetFilterChainMatch())
	// Single tcp_proxy filter.
	require.Len(t, fc.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", fc.GetFilters()[0].GetName())
	var tp tcp_proxyv3.TcpProxy
	require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(&tp))
	assert.Equal(t, PassthroughClusterName, tp.GetCluster())
}

func TestBuildCaptureRouteConfiguration(t *testing.T) {
	// cluster.local authority -> service cluster, reusing the outbound vhost builder.
	// The service key is the namespace-qualified "<ns>/<svc>" (020 Part 1), rendered
	// as the FQDN authority svc-1.aether-test.aether.internal.
	vh := BuildOutboundClusterVirtualHost(
		ServiceClusterName("aether-test/svc-1", "aether.internal"),
		[]string{"svc-1.aether-test.svc.cluster.local", "svc-1.aether-test.svc.cluster.local:18081"},
	)
	rc := BuildCaptureRouteConfiguration([]*routev3.VirtualHost{vh}, "aether.internal", false)
	assert.Equal(t, CaptureHTTPRouteName, rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 2, "the service vhost + the on-demand catch-all")

	got := rc.GetVirtualHosts()[0]
	assert.Contains(t, got.GetDomains(), "svc-1.aether-test.svc.cluster.local:18081")
	assert.Equal(t, "svc-1.aether-test.aether.internal", got.GetRoutes()[0].GetRoute().GetCluster())

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
	assert.True(t, has404, "scoped-capture catch-all 404s non-mesh authorities")
}

// TestBuildCaptureRouteConfigurationRedirectAll verifies that with redirect-all
// capture the catch-all's final fallthrough routes to the ORIGINAL_DST passthrough
// (so a captured request to a real Kubernetes Service with no mesh authority and no
// dependency-set vhost still reaches its backend via kube-proxy) instead of 404'ing.
func TestBuildCaptureRouteConfigurationRedirectAll(t *testing.T) {
	rc := BuildCaptureRouteConfiguration(nil, "aether.internal", true)
	require.Len(t, rc.GetVirtualHosts(), 1, "just the on-demand catch-all")
	catchAll := rc.GetVirtualHosts()[0]

	var hasOnDemand, hasPassthrough, has404 bool
	for _, r := range catchAll.GetRoutes() {
		if r.GetRoute().GetClusterHeader() == ":authority" {
			hasOnDemand = true
		}
		if r.GetRoute().GetCluster() == PassthroughClusterName {
			hasPassthrough = true
		}
		if r.GetDirectResponse().GetStatus() == 404 {
			has404 = true
		}
	}
	assert.True(t, hasOnDemand, "mesh authorities still resolve via ODCDS")
	assert.True(t, hasPassthrough, "redirect-all fallthrough routes to the ORIGINAL_DST passthrough")
	assert.False(t, has404, "redirect-all catch-all does not hard-404 non-mesh authorities")
}
