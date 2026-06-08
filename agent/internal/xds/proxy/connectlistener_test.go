package proxy

import (
	"strings"
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNodeID = "spiffe://example.org/ns/aether-system/sa/aether-agent"

func TestGenerateNodeConnectResources_NoNodeIdentity(t *testing.T) {
	assert.Nil(t, GenerateNodeConnectResources(nil, "", "spiffe://example.org"), "nothing without a node identity")
}

func TestGenerateNodeConnectResources(t *testing.T) {
	pods := []*cniv1.CNIPod{
		{Name: "pod-a", Ips: []string{"10.0.0.1"}},
		{Name: "pod-b", Ips: []string{"10.0.0.2"}},
		{Name: "no-ip"}, // skipped: no IP
	}

	res := GenerateNodeConnectResources(pods, testNodeID, "spiffe://example.org")
	require.NotNil(t, res)

	// node_connect listener + one inner listener per pod-with-IP (2).
	require.Len(t, res.Listeners, 3)
	require.Len(t, res.Clusters, 2)

	nc := findListener(res.Listeners, NodeConnectListenerName)
	require.NotNil(t, nc)
	assert.NotNil(t, nc.GetFilterChains()[0].GetTransportSocket(), "node CONNECT listener needs downstream mTLS")

	hcm := decodeHCM(t, nc)
	assert.True(t, hcm.GetHttp2ProtocolOptions().GetAllowConnect(), "must allow CONNECT")
	// Captures the verified peer identity into filter state for the inner HCM.
	assert.Equal(t, "envoy.filters.http.set_filter_state", hcm.GetHttpFilters()[0].GetName())
	// The reachability probe is answered by the health_check filter, not a route.
	assert.Equal(t, "envoy.filters.http.health_check", hcm.GetHttpFilters()[1].GetName())

	routes := hcm.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	require.Len(t, routes, 2) // one CONNECT route per pod; node-live is on the filter

	// Each pod's CONNECT route (keyed on its IP) targets its inner-demux cluster.
	byCluster := map[string]string{}
	for _, r := range routes {
		require.NotNil(t, r.GetMatch().GetConnectMatcher())
		authority := r.GetMatch().GetHeaders()[0].GetStringMatch().GetExact()
		require.Equal(t, "CONNECT", r.GetRoute().GetUpgradeConfigs()[0].GetUpgradeType())
		byCluster[r.GetRoute().GetCluster()] = authority
	}
	assert.Equal(t, "10.0.0.1", byCluster[innerDemuxClusterName(pods[0])])
	assert.Equal(t, "10.0.0.2", byCluster[innerDemuxClusterName(pods[1])])

	// The inner listener rebuilds XFCC from the captured peer identity and routes
	// to the pod's app cluster.
	inner := findListener(res.Listeners, innerListenerName(pods[0]))
	require.NotNil(t, inner)
	require.NotNil(t, inner.GetInternalListener(), "inner listener must be internal")
	innerHCM := decodeHCM(t, inner)
	vh := innerHCM.GetRouteConfig().GetVirtualHosts()[0]
	assert.Equal(t, AppClusterName(pods[0]), vh.GetRoutes()[0].GetRoute().GetCluster())
	require.Len(t, vh.GetRequestHeadersToAdd(), 1)
	xfcc := vh.GetRequestHeadersToAdd()[0].GetHeader()
	assert.Equal(t, xfccHeader, xfcc.GetKey())
	assert.True(t, strings.Contains(xfcc.GetValue(), "%FILTER_STATE("+peerURIFilterStateKey), "XFCC built from peer filter state")

	// The inner-demux cluster carries the filter state across via internal_upstream.
	require.Equal(t, internalUpstreamTransportSocketName, res.Clusters[0].GetTransportSocket().GetName())
}

func TestBuildNodeConnectRouteConfiguration_Empty(t *testing.T) {
	rc := buildNodeConnectRouteConfiguration(nil)
	assert.False(t, rc.GetValidateClusters().GetValue(), "validation off so churn doesn't wedge node_connect")
	routes := rc.GetVirtualHosts()[0].GetRoutes()
	require.Empty(t, routes, "no routes when there are no pods; node-live is on the health_check filter")
}

func findListener(ls []*listenerv3.Listener, name string) *listenerv3.Listener {
	for _, l := range ls {
		if l.GetName() == name {
			return l
		}
	}
	return nil
}

func decodeHCM(t *testing.T, l *listenerv3.Listener) *http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	return hcm
}
