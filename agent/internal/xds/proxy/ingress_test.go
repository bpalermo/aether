package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNodeID = "spiffe://example.org/ns/aether-system/sa/aether-agent"

func TestNewNodeInboundListener_NoNodeIdentity(t *testing.T) {
	assert.Nil(t, NewNodeInboundListener(nil, "", "spiffe://example.org"), "nothing without a node identity")
}

func TestNewNodeInboundListener(t *testing.T) {
	pods := []*cniv1.CNIPod{
		{Name: "pod-a", Ips: []string{"10.0.0.1"}},
		{Name: "pod-b", Ips: []string{"10.0.0.2"}},
		{Name: "no-ip"}, // skipped: no IP
	}

	l := NewNodeInboundListener(pods, testNodeID, "spiffe://example.org")
	require.NotNil(t, l)
	assert.Equal(t, NodeInboundListenerName, l.GetName())
	assert.Equal(t, uint32(defaultNodeInboundPort), l.GetAddress().GetSocketAddress().GetPortValue())
	assert.NotNil(t, l.GetFilterChains()[0].GetTransportSocket(), "node inbound listener needs downstream mTLS")

	hcm := decodeHCM(t, l)
	assert.Equal(t, http_connection_managerv3.HttpConnectionManager_HTTP2, hcm.GetCodecType())
	// XFCC set natively from the verified peer (no rebuild).
	assert.Equal(t, http_connection_managerv3.HttpConnectionManager_SANITIZE_SET, hcm.GetForwardClientCertDetails())
	assert.True(t, hcm.GetSetCurrentClientCertDetails().GetUri())
	// node-live answered by the health_check filter, before the router.
	assert.Equal(t, "envoy.filters.http.health_check", hcm.GetHttpFilters()[0].GetName())
	assert.Equal(t, "envoy.filters.http.router", hcm.GetHttpFilters()[1].GetName())

	// One virtual host per pod-with-IP, matched by the pod IP authority -> app_<pod>.
	rc := hcm.GetRouteConfig()
	assert.False(t, rc.GetValidateClusters().GetValue(), "validation off so app_<pod> churn doesn't wedge the listener")
	require.Len(t, rc.GetVirtualHosts(), 2)
	byDomain := map[string]string{}
	for _, vh := range rc.GetVirtualHosts() {
		require.Len(t, vh.GetDomains(), 1)
		byDomain[vh.GetDomains()[0]] = vh.GetRoutes()[0].GetRoute().GetCluster()
	}
	assert.Equal(t, AppClusterName(pods[0]), byDomain["10.0.0.1"])
	assert.Equal(t, AppClusterName(pods[1]), byDomain["10.0.0.2"])
}

func TestBuildNodeInboundRouteConfiguration_Empty(t *testing.T) {
	rc := buildNodeInboundRouteConfiguration(nil)
	assert.False(t, rc.GetValidateClusters().GetValue(), "validation off so churn doesn't wedge node_inbound")
	assert.Empty(t, rc.GetVirtualHosts(), "no vhosts when there are no pods; node-live is on the health_check filter")
}

func decodeHCM(t *testing.T, l *listenerv3.Listener) *http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	return hcm
}
