package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateNodeConnectListener_NoNodeIdentity(t *testing.T) {
	l := GenerateNodeConnectListener(nil, "", "spiffe://example.org")
	assert.Nil(t, l, "no listener without a node identity")
}

func TestGenerateNodeConnectListener(t *testing.T) {
	pods := []*cniv1.CNIPod{
		{Name: "pod-a", Ips: []string{"10.0.0.1"}},
		{Name: "pod-b", Ips: []string{"10.0.0.2"}},
		{Name: "no-ip"}, // skipped: no IP
	}

	l := GenerateNodeConnectListener(pods, "spiffe://example.org/ns/aether-system/sa/aether-agent", "spiffe://example.org")
	require.NotNil(t, l)
	assert.Equal(t, NodeConnectListenerName, l.GetName())
	assert.Equal(t, uint32(defaultNodeConnectPort), l.GetAddress().GetSocketAddress().GetPortValue())

	fc := l.GetFilterChains()
	require.Len(t, fc, 1)
	assert.NotNil(t, fc[0].GetTransportSocket(), "node CONNECT listener must have downstream mTLS")

	// Decode the HCM and assert CONNECT + routing.
	require.Len(t, fc[0].GetFilters(), 1)
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, fc[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	assert.True(t, hcm.GetHttp2ProtocolOptions().GetAllowConnect(), "must allow CONNECT")
	assert.Equal(t, http_connection_managerv3.HttpConnectionManager_SANITIZE_SET, hcm.GetForwardClientCertDetails())
	assert.True(t, hcm.GetSetCurrentClientCertDetails().GetUri(), "must forward caller URI SAN")

	routes := hcm.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	// node-live + one route per pod with an IP (2 of 3 pods).
	require.Len(t, routes, 3)
	assert.Equal(t, NodeLivePath, routes[0].GetMatch().GetPath())
	assert.Equal(t, uint32(200), routes[0].GetDirectResponse().GetStatus())

	// Each pod route is a CONNECT match on the pod IP -> its app cluster.
	byCluster := map[string]string{} // cluster -> authority match
	for _, r := range routes[1:] {
		require.NotNil(t, r.GetMatch().GetConnectMatcher())
		require.Len(t, r.GetMatch().GetHeaders(), 1)
		authority := r.GetMatch().GetHeaders()[0].GetStringMatch().GetExact()
		ra := r.GetRoute()
		require.NotNil(t, ra)
		require.Len(t, ra.GetUpgradeConfigs(), 1)
		assert.Equal(t, "CONNECT", ra.GetUpgradeConfigs()[0].GetUpgradeType())
		assert.NotNil(t, ra.GetUpgradeConfigs()[0].GetConnectConfig())
		byCluster[ra.GetCluster()] = authority
	}
	assert.Equal(t, "10.0.0.1", byCluster[AppClusterName(pods[0])])
	assert.Equal(t, "10.0.0.2", byCluster[AppClusterName(pods[1])])
}

func TestBuildNodeConnectRouteConfiguration_Empty(t *testing.T) {
	rc := buildNodeConnectRouteConfiguration(nil)
	routes := rc.GetVirtualHosts()[0].GetRoutes()
	require.Len(t, routes, 1, "only the node-live route when there are no pods")
	assert.Equal(t, NodeLivePath, routes[0].GetMatch().GetPath())
	_, ok := routes[0].GetAction().(*routev3.Route_DirectResponse)
	assert.True(t, ok)
}
