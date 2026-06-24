package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCaptureListener(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateCaptureListener(pod, 15001, "aether.internal", false)
	require.NoError(t, err)

	assert.Equal(t, "capture_p1", l.GetName())
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, "0.0.0.0", sa.GetAddress())
	assert.Equal(t, uint32(15001), sa.GetPortValue())
	assert.Equal(t, "/var/run/netns/p1", sa.GetNetworkNamespaceFilepath())
	assert.True(t, l.GetUseOriginalDst().GetValue(), "use_original_dst set")

	// listener filters: original_dst then http_inspector.
	require.Len(t, l.GetListenerFilters(), 2)
	assert.Equal(t, listenerFilterOriginalDstName, l.GetListenerFilters()[0].GetName())
	assert.Equal(t, listenerFilterHTTPInspectorName, l.GetListenerFilters()[1].GetName())

	// the HCM serves the capture RDS route table.
	require.Len(t, l.GetFilterChains(), 1)
	filters := l.GetFilterChains()[0].GetFilters()
	hcmFilter := filters[len(filters)-1]
	var hcm http_connection_managerv3.HttpConnectionManager
	require.NoError(t, hcmFilter.GetTypedConfig().UnmarshalTo(&hcm))
	assert.Equal(t, CaptureHTTPRouteName, hcm.GetRds().GetRouteConfigName())
}

func TestGenerateCaptureListener_RequiresNetns(t *testing.T) {
	_, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1"}, 15001, "aether.internal", false)
	assert.Error(t, err)
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
