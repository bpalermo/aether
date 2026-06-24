package proxy

import (
	"strings"
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	xdstypev3 "github.com/cncf/xds/go/xds/type/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	healthcheckv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInboundListener(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name:             "pod-a",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
		Ips:              []string{"10.0.0.1"},
	}

	l, err := NewInboundListener(pod, "example.org", false)
	require.NoError(t, err)
	assert.Equal(t, InboundListenerName(pod), l.GetName())

	// Bound into the pod's netns at the mesh inbound port (per-pod lifecycle + netpol).
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, uint32(defaultInboundPort), sa.GetPortValue())
	assert.Equal(t, "/var/run/netns/cni-a", sa.GetNetworkNamespaceFilepath())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, l.GetTrafficDirection())

	// First chain is the TCP floor — the listener's default chain (no match): a
	// no-ALPN mTLS connection (the source tcp_proxy egress) lands here.
	tcpFloor := l.GetFilterChains()[0]
	assert.Nil(t, tcpFloor.GetFilterChainMatch(), "TCP floor is the default chain (no match)")
	require.NotNil(t, tcpFloor.GetTransportSocket(), "TCP floor chain must terminate mTLS")
	require.Len(t, tcpFloor.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", tcpFloor.GetFilters()[0].GetName())

	// Second chain is the default HCM chain (no match criteria = catch-all for h2).
	fc := l.GetFilterChains()[1]
	require.NotNil(t, fc.GetTransportSocket(), "inbound listener must terminate mTLS")

	hcm := decodeHCM(t, l)
	// XFCC set natively from the verified peer (the caller's SVID).
	assert.Equal(t, http_connection_managerv3.HttpConnectionManager_SANITIZE_SET, hcm.GetForwardClientCertDetails())
	assert.True(t, hcm.GetSetCurrentClientCertDetails().GetUri())

	// Liveness + readiness health-check filters, then the stats filter (proposal
	// 007 Phase 2), then the router.
	require.Len(t, hcm.GetHttpFilters(), 4)
	assert.Equal(t, livenessHealthCheckFilterName, hcm.GetHttpFilters()[0].GetName())
	assert.Equal(t, readinessHealthCheckFilterName, hcm.GetHttpFilters()[1].GetName())
	assert.Equal(t, statsFilterName, hcm.GetHttpFilters()[2].GetName())
	assert.Equal(t, "envoy.filters.http.router", hcm.GetHttpFilters()[3].GetName())

	live := decodeHealthCheck(t, hcm.GetHttpFilters()[0])
	assert.False(t, live.GetPassThroughMode().GetValue())
	assert.Equal(t, MeshLivePath, live.GetHeaders()[0].GetStringMatch().GetExact())
	assert.Empty(t, live.GetClusterMinHealthyPercentages(), "liveness must not depend on the app")

	ready := decodeHealthCheck(t, hcm.GetHttpFilters()[1])
	assert.Equal(t, MeshReadyPath, ready.GetHeaders()[0].GetStringMatch().GetExact())
	// Readiness gates on the pod's app health-probe cluster being healthy.
	require.Contains(t, ready.GetClusterMinHealthyPercentages(), HealthProbeClusterName(pod))
	assert.Equal(t, float64(100), ready.GetClusterMinHealthyPercentages()[HealthProbeClusterName(pod)].GetValue())

	// All other requests route to the pod's app cluster.
	rc := hcm.GetRouteConfig()
	assert.False(t, rc.GetValidateClusters().GetValue(), "validation off so app_<pod> churn doesn't wedge the listener")
	vh := rc.GetVirtualHosts()[0]
	assert.Equal(t, []string{"*"}, vh.GetDomains())
	assert.Equal(t, AppClusterName(pod, AppPortFromPod(pod)), vh.GetRoutes()[0].GetRoute().GetCluster())
}

func TestNewInboundListener_Errors(t *testing.T) {
	_, err := NewInboundListener(nil, "example.org", false)
	require.Error(t, err)

	_, err = NewInboundListener(&cniv1.CNIPod{Name: "no-netns"}, "example.org", false)
	require.Error(t, err)
}

// decodeHCM finds and decodes the first HCM filter chain on the listener.
// With the TCP floor chain now first, the default HCM chain is at index 1.
func decodeHCM(t *testing.T, l *listenerv3.Listener) *http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	for _, fc := range l.GetFilterChains() {
		if fc.GetFilterChainMatch() != nil && len(fc.GetFilterChainMatch().GetApplicationProtocols()) > 0 {
			// Skip the TCP floor chain (matched on application_protocols).
			continue
		}
		for _, f := range fc.GetFilters() {
			hcm := &http_connection_managerv3.HttpConnectionManager{}
			if err := f.GetTypedConfig().UnmarshalTo(hcm); err == nil {
				return hcm
			}
		}
	}
	t.Fatal("no HCM filter found in listener filter chains")
	return nil
}

func decodeHealthCheck(t *testing.T, f *http_connection_managerv3.HttpFilter) *healthcheckv3.HealthCheck {
	t.Helper()
	hc := &healthcheckv3.HealthCheck{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(hc))
	return hc
}

// TestInboundFilterChains_MultiPort verifies per-port SNI demux: a default
// (no-server_names) chain plus one server_names=<port> chain per served port,
// each forwarding to that port's app cluster.
func TestInboundFilterChains_MultiPort(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name:             "mp",
		Namespace:        "default",
		ServiceAccount:   "mp",
		NetworkNamespace: "/var/run/netns/cni-a",
		Ips:              []string{"10.0.0.1"},
		Annotations:      map[string]string{constants.AnnotationEndpointPorts: "8080,9090"},
	}
	l, err := NewInboundListener(pod, "example.org", false)
	require.NoError(t, err)

	bySNI := map[string]*listenerv3.FilterChain{}
	var defaultChain *listenerv3.FilterChain
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		// The TCP floor is the only no-match (default) chain — skip it.
		if m == nil {
			continue
		}
		// Per-port HCM chains match a port SNI; the no-SNI HCM chain matches "h2".
		if len(m.GetServerNames()) > 0 {
			bySNI[m.GetServerNames()[0]] = fc
			continue
		}
		defaultChain = fc
	}
	require.NotNil(t, defaultChain, "a default (no-SNI, h2) HCM chain must exist for back-compat")
	require.Contains(t, bySNI, "8080")
	require.Contains(t, bySNI, "9090")

	clusterOf := func(fc *listenerv3.FilterChain) string {
		hcm := &http_connection_managerv3.HttpConnectionManager{}
		require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
		return hcm.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()[0].GetRoute().GetCluster()
	}
	assert.Equal(t, "app_mp_9090", clusterOf(bySNI["9090"]))
	assert.Equal(t, "app_mp_8080", clusterOf(bySNI["8080"]))
	assert.Equal(t, "app_mp_8080", clusterOf(defaultChain), "default chain → primary port")
}

// TestInboundChainStatsFilter verifies the stats filter (proposal 007
// Phase 2) sits after the two health-check filters (so locally-answered probes
// are not counted) and before the router on the inbound HCM, carrying the local
// pod's destination identity and reporter=destination in its filter_config.
func TestInboundChainStatsFilter(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name:             "checkout-abc",
		Namespace:        "default",
		ServiceAccount:   "checkout",
		NetworkNamespace: "/var/run/netns/cni-a",
		Ips:              []string{"10.0.0.1"},
	}
	l, err := NewInboundListener(pod, "aether.internal", false)
	require.NoError(t, err)
	require.NotEmpty(t, l.GetFilterChains())

	// FilterChains()[0] is the TCP floor chain (application_protocols=["aether-tcp"]).
	// The default HCM chain (catch-all for h2/mTLS traffic) is at index 1.
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[1].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))

	filters := hcm.GetHttpFilters()
	require.Len(t, filters, 4, "expected liveness + readiness + stats + router")
	assert.Equal(t, statsFilterName, filters[2].GetName())
	assert.Equal(t, httpRouterFilterName, filters[3].GetName())

	ts := &xdstypev3.TypedStruct{}
	require.NoError(t, filters[2].GetTypedConfig().UnmarshalTo(ts))
	assert.Equal(t, statsConfigTypeURL, ts.GetTypeUrl())
	fields := ts.GetValue().GetFields()
	assert.Equal(t, "destination", fields["reporter"].GetStringValue())
	assert.Equal(t, "checkout", fields["destination_service"].GetStringValue())
	// destination_pod (the local pod) always travels; the C++ filter drops it
	// unless emit_pod is set (off by default here).
	assert.Equal(t, "checkout-abc", fields["destination_pod"].GetStringValue())
	assert.False(t, fields["emit_pod"].GetBoolValue())
}

// TestInboundTCPFloorFilterChain verifies the TCP floor chain on the inbound :15008
// listener (proposal 018, Phase 3a) — the DEFAULT chain (no match): a no-ALPN mTLS
// connection lands here, with a single tcp_proxy network filter to the app cluster.
func TestInboundTCPFloorFilterChain(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name:             "svc-a",
		Namespace:        "default",
		ServiceAccount:   "svc-a",
		NetworkNamespace: "/var/run/netns/cni-tcp",
		Ips:              []string{"10.0.0.5"},
	}
	l, err := NewInboundListener(pod, "aether.internal", false)
	require.NoError(t, err)

	// Find the TCP floor chain: the default chain (no match), named in_tcp_<pod>.
	var tcpFloor *listenerv3.FilterChain
	for _, fc := range l.GetFilterChains() {
		if fc.GetFilterChainMatch() == nil && strings.HasPrefix(fc.GetName(), "in_tcp_") {
			tcpFloor = fc
			break
		}
	}
	require.NotNil(t, tcpFloor, "TCP floor chain (default, in_tcp_<pod>) must be present")

	// Chain name.
	assert.Equal(t, "in_tcp_svc-a", tcpFloor.GetName())

	// Must terminate mTLS (transport socket present).
	require.NotNil(t, tcpFloor.GetTransportSocket(), "TCP floor chain must terminate mTLS")
	assert.Equal(t, "envoy.transport_sockets.tls", tcpFloor.GetTransportSocket().GetName())

	// Single tcp_proxy network filter routing to the pod's app cluster.
	require.Len(t, tcpFloor.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", tcpFloor.GetFilters()[0].GetName())

	tcp := &tcp_proxyv3.TcpProxy{}
	require.NoError(t, tcpFloor.GetFilters()[0].GetTypedConfig().UnmarshalTo(tcp))
	assert.Equal(t, "app_svc-a_8080", tcp.GetCluster(), "tcp_proxy must forward to the pod's primary app cluster")
}
