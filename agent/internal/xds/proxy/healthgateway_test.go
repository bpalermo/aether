package proxy

import (
	"testing"

	health_checkv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func gatewayHCM(t *testing.T, probeClusters []string) *http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	l := BuildHealthGatewayListener("/run/aether/health.sock", probeClusters)
	require.Len(t, l.GetFilterChains(), 1)
	require.Len(t, l.GetFilterChains()[0].GetFilters(), 1)
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	return hcm
}

func TestBuildHealthGatewayListener(t *testing.T) {
	l := BuildHealthGatewayListener("/run/aether/health.sock", []string{"health_b", "health_a"})

	assert.Equal(t, HealthGatewayListenerName, l.GetName())
	assert.Equal(t, "/run/aether/health.sock", l.GetAddress().GetPipe().GetPath(), "gateway must listen on the UDS, not a port")

	hcm := gatewayHCM(t, []string{"health_b", "health_a"})
	filters := hcm.GetHttpFilters()
	require.Len(t, filters, 3, "one health_check per cluster + router")

	// Sorted cluster order, router last.
	for i, wantCluster := range []string{"health_a", "health_b"} {
		assert.Equal(t, httpHealthCheckFilterName, filters[i].GetName())
		hc := &health_checkv3.HealthCheck{}
		require.NoError(t, filters[i].GetTypedConfig().UnmarshalTo(hc))
		assert.False(t, hc.GetPassThroughMode().GetValue())
		assert.Equal(t, HealthGatewayPath(wantCluster), hc.GetHeaders()[0].GetStringMatch().GetExact())
		require.Contains(t, hc.GetClusterMinHealthyPercentages(), wantCluster)
		assert.Equal(t, float64(100), hc.GetClusterMinHealthyPercentages()[wantCluster].GetValue())
	}
	assert.Equal(t, httpRouterFilterName, filters[2].GetName())

	// Catch-all 404 for unprogrammed pods.
	routes := hcm.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	require.Len(t, routes, 1)
	assert.Equal(t, uint32(404), routes[0].GetDirectResponse().GetStatus())
}

// TestBuildHealthGatewayListener_Deterministic: equal pod sets in different
// iteration order must produce byte-identical listeners, or every snapshot
// rebuild would push a spurious LDS update.
func TestBuildHealthGatewayListener_Deterministic(t *testing.T) {
	a := BuildHealthGatewayListener("/run/aether/health.sock", []string{"health_x", "health_y", "health_z"})
	b := BuildHealthGatewayListener("/run/aether/health.sock", []string{"health_z", "health_x", "health_y"})
	assert.True(t, proto.Equal(a, b), "gateway listener must be order-independent")
}

// TestBuildHealthGatewayListener_Empty: with no pods the gateway still exists
// (router + 404 only), so the agent's probe gets 404 (skip) rather than a
// confusing connection error from a missing listener.
func TestBuildHealthGatewayListener_Empty(t *testing.T) {
	hcm := gatewayHCM(t, nil)
	require.Len(t, hcm.GetHttpFilters(), 1)
	assert.Equal(t, httpRouterFilterName, hcm.GetHttpFilters()[0].GetName())
}

// TestHealthGatewayBypassesOverloadManager verifies the gateway is exempt from
// overload actions: liveness must report app truth, not proxy state, or an
// overloaded proxy would flap every local pod's registry health cluster-wide.
func TestHealthGatewayBypassesOverloadManager(t *testing.T) {
	l := BuildHealthGatewayListener("/run/aether/health.sock", []string{"health_a"})
	assert.True(t, l.GetBypassOverloadManager(), "health gateway must bypass overload manager actions")
}
