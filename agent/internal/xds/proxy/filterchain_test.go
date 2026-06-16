package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	xdstypev3 "github.com/cncf/xds/go/xds/type/v3"
	health_checkv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDefaultOutboundHTTPFilterChain(t *testing.T) {
	tests := []struct {
		name              string
		podName           string
		expectedChainName string
	}{
		{
			name:              "standard outbound chain",
			podName:           "my-pod",
			expectedChainName: "out_http_my-pod",
		},
		{
			name:              "empty name",
			podName:           "",
			expectedChainName: "out_http_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := buildDefaultOutboundHTTPFilterChain(&cniv1.CNIPod{Name: tt.podName}, "aether.internal")

			require.NotNil(t, fc)
			assert.Equal(t, tt.expectedChainName, fc.GetName())
			// Outbound has 2 filters: set_filter_state + http_connection_manager
			assert.Len(t, fc.GetFilters(), 2)
			assert.Nil(t, fc.GetTransportSocket(), "outbound filter chain should not have TLS transport socket")
		})
	}
}

// TestOutboundChainReadinessFilter verifies the outbound HCM carries the
// non-pass-through health_check readiness filter ahead of the router, matched
// on the shared readiness path probed by the CNI plugin from inside the netns.
func TestOutboundChainReadinessFilter(t *testing.T) {
	fc := buildDefaultOutboundHTTPFilterChain(&cniv1.CNIPod{Name: "my-pod"}, "aether.internal")
	require.Len(t, fc.GetFilters(), 2)

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, fc.GetFilters()[1].GetTypedConfig().UnmarshalTo(hcm))

	assert.False(t, hcm.GetStripAnyHostPort(),
		"authority :port is a routing selector (FQDN:port → that port's cluster); must NOT be stripped")

	httpFilters := hcm.GetHttpFilters()
	require.Len(t, httpFilters, 5, "expected health_check + subset-headers + on_demand + stats + router")
	assert.Equal(t, httpHealthCheckFilterName, httpFilters[0].GetName())
	assert.Equal(t, SubsetHeadersFilterName, httpFilters[1].GetName())
	assert.Equal(t, httpOnDemandFilterName, httpFilters[2].GetName())
	assert.Equal(t, statsFilterName, httpFilters[3].GetName())
	assert.Equal(t, httpRouterFilterName, httpFilters[4].GetName())

	hc := &health_checkv3.HealthCheck{}
	require.NoError(t, httpFilters[0].GetTypedConfig().UnmarshalTo(hc))
	assert.False(t, hc.GetPassThroughMode().GetValue(), "readiness filter must answer directly")
	assert.Empty(t, hc.GetClusterMinHealthyPercentages(), "pure server-state check, no cluster gating")
	require.Len(t, hc.GetHeaders(), 1)
	assert.Equal(t, ":path", hc.GetHeaders()[0].GetName())
	assert.Equal(t, constants.ProxyReadinessPath, hc.GetHeaders()[0].GetStringMatch().GetExact())
}

// TestOutboundChainStatsFilter verifies the stats filter (proposals 007/012)
// sits immediately before the router on the outbound HCM, carrying the pod's
// source identity in its per-instance filter_config.
func TestOutboundChainStatsFilter(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "my-pod", ServiceAccount: "checkout"}

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	fc := buildDefaultOutboundHTTPFilterChain(pod, "aether.internal")
	require.NoError(t, fc.GetFilters()[1].GetTypedConfig().UnmarshalTo(hcm))

	filters := hcm.GetHttpFilters()
	require.Len(t, filters, 5, "expected health_check + subset + on_demand + stats + router")
	assert.Equal(t, statsFilterName, filters[3].GetName())
	assert.Equal(t, httpRouterFilterName, filters[4].GetName())

	// The source identity travels in the filter's TypedStruct config; Envoy
	// resolves the native C++ factory by the inner proto type.
	ts := &xdstypev3.TypedStruct{}
	require.NoError(t, filters[3].GetTypedConfig().UnmarshalTo(ts))
	assert.Equal(t, statsConfigTypeURL, ts.GetTypeUrl())
	fields := ts.GetValue().GetFields()
	assert.Equal(t, "source", fields["reporter"].GetStringValue())
	assert.Equal(t, "checkout", fields["source_service"].GetStringValue())
}
