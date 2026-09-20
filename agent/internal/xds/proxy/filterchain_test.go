package proxy

import (
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	xdstypev3 "github.com/cncf/xds/go/xds/type/v3"
	health_checkv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
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
			fc := buildDefaultOutboundHTTPFilterChain(&cniv1.CNIPod{Name: tt.podName}, "spiffe://aether.internal/ns/default/sa/test", "aether.internal", false, nil)

			require.NotNil(t, fc)
			assert.Equal(t, tt.expectedChainName, fc.GetName())
			// Outbound has 3 filters: the two source set_filter_state entries
			// (netns, then SPIFFE ID — issue #815) + http_connection_manager.
			assert.Len(t, fc.GetFilters(), 3)
			assert.Nil(t, fc.GetTransportSocket(), "outbound filter chain should not have TLS transport socket")
		})
	}
}

// TestOutboundChainReadinessFilter verifies the outbound HCM carries the
// non-pass-through health_check readiness filter ahead of the router, matched
// on the shared readiness path probed by the CNI plugin from inside the netns.
func TestOutboundChainReadinessFilter(t *testing.T) {
	fc := buildDefaultOutboundHTTPFilterChain(&cniv1.CNIPod{Name: "my-pod"}, "spiffe://aether.internal/ns/default/sa/test", "aether.internal", false, nil)
	require.Len(t, fc.GetFilters(), 3)

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, fc.GetFilters()[2].GetTypedConfig().UnmarshalTo(hcm))

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
	assert.Equal(t, meshconst.ProxyReadinessPath, hc.GetHeaders()[0].GetStringMatch().GetExact())
}

// TestOutboundChainStatsFilter verifies the stats filter (proposals 007/012)
// sits immediately before the router on the outbound HCM, carrying the pod's
// source identity in its per-instance filter_config.
func TestOutboundChainStatsFilter(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "my-pod", ServiceAccount: "checkout"}

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	fc := buildDefaultOutboundHTTPFilterChain(pod, "spiffe://aether.internal/ns/default/sa/test", "aether.internal", false, nil)
	require.NoError(t, fc.GetFilters()[2].GetTypedConfig().UnmarshalTo(hcm))

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
	// source_pod always travels in the config; the C++ filter drops it unless
	// emit_pod is set (off by default here).
	assert.Equal(t, "my-pod", fields["source_pod"].GetStringValue())
	assert.False(t, fields["emit_pod"].GetBoolValue())
}

// TestOutboundChainRDSInitialFetchTimeout pins the egress RDS warming budget
// (issue #817).
//
// initial_fetch_timeout is how long the listener stays WARMING for the first
// out_http delivery; when it expires Envoy activates the listener anyway, with
// an unresolved route table, and 404 NR route_not_found is what the mesh sees.
// Leaving the field unset inherits Envoy's default rather than stating one, and
// an unstated value is one no test can hold still.
//
// The value must not drop below the agent's own registryReadyTimeout, or a
// fresh Envoy epoch can activate the egress listener while the agent feeding it
// is still inside its initial-snapshot budget.
func TestOutboundChainRDSInitialFetchTimeout(t *testing.T) {
	fc := buildDefaultOutboundHTTPFilterChain(&cniv1.CNIPod{Name: "my-pod"}, "spiffe://aether.internal/ns/default/sa/test", "aether.internal", false, nil)
	require.Len(t, fc.GetFilters(), 3)

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, fc.GetFilters()[2].GetTypedConfig().UnmarshalTo(hcm))

	rds := hcm.GetRds()
	require.NotNil(t, rds, "the egress listener routes over RDS")
	assert.Equal(t, OutboundHTTPRouteName, rds.GetRouteConfigName())

	cs := rds.GetConfigSource()
	require.NotNil(t, cs)
	assert.NotNil(t, cs.GetAds(), "RDS rides the ADS stream")
	require.NotNil(t, cs.GetInitialFetchTimeout(), "the egress RDS warming budget must be explicit, not inherited")
	assert.Equal(t, OutboundRouteInitialFetchTimeout, cs.GetInitialFetchTimeout().AsDuration())
	assert.Equal(t, 15*time.Second, OutboundRouteInitialFetchTimeout,
		"kept equal to the agent's registryReadyTimeout; changing one without the other reopens the #817 window")
}

// TestOutboundChainRDSIdenticalAcrossPods is the emitted-resource half of the
// #852 guard; the constructor half lives in
// //agent/internal/xds/config:determinism_test.
//
// Every meshed pod on the node gets its own egress listener, and each one emits
// its own copy of the out_http Rds. Envoy keys RDS provider reuse on a hash of
// the WHOLE Rds message and matches on that hash alone, so those N copies
// collapse onto one provider, one route table and one subscription only while
// they agree. Anything per-pod leaking into that message splits them into N
// independent providers — a valid config, a healthy proxy, no NACK, and because
// the egress HCM stat_prefix is the constant "outbound_http", not even a new
// stat series: http.outbound_http.rds.out_http.config_reload would simply start
// counting N per push.
//
// One caveat the assertion deliberately does not encode: config_source's
// initial_fetch_timeout is released before that hash and restored after, so it
// alone cannot split a provider. Comparing the whole marshalled Rds is stricter
// than Envoy's own key on purpose — an out_http Rds that varies per pod is a bug
// regardless of which field Envoy happens to normalise away this release.
//
// The pods below differ in every field the chain builder reads, so agreement
// here means the Rds genuinely does not depend on the pod.
func TestOutboundChainRDSIdenticalAcrossPods(t *testing.T) {
	pods := []*cniv1.CNIPod{
		{Name: "checkout-abc", Namespace: "shop", ServiceAccount: "checkout"},
		{Name: "payments-xyz", Namespace: "billing", ServiceAccount: "payments"},
	}

	var want []byte
	for _, pod := range pods {
		fc := buildDefaultOutboundHTTPFilterChain(pod, "spiffe://aether.internal/ns/"+pod.GetNamespace()+"/sa/"+pod.GetServiceAccount(), "aether.internal", true, nil)
		require.Len(t, fc.GetFilters(), 3)

		hcm := &http_connection_managerv3.HttpConnectionManager{}
		require.NoError(t, fc.GetFilters()[2].GetTypedConfig().UnmarshalTo(hcm))

		rds := hcm.GetRds()
		require.NotNil(t, rds)
		got, err := proto.Marshal(rds)
		require.NoError(t, err)

		if want == nil {
			want = got
			continue
		}
		require.Equal(t, want, got,
			"the out_http Rds differs between pods: Envoy would create one RDS subscription, "+
				"route table and stats scope PER POD instead of sharing one (issue #852)")
	}
}
