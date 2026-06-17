package proxy

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestBuildNetworkNamespaceFilterState(t *testing.T) {
	filter := buildNetworkNamespaceFilterState()

	require.NotNil(t, filter)
	assert.Equal(t, "envoy.filters.network.set_filter_state", filter.GetName())
	assert.NotNil(t, filter.GetTypedConfig())
}

func TestBuildHTTPConnectionManager(t *testing.T) {
	tests := []struct {
		name        string
		statPrefix  string
		routeConfig *routev3.RouteConfiguration
		expectRDS   bool
	}{
		{
			name:       "with inline route config",
			statPrefix: "test",
			routeConfig: &routev3.RouteConfiguration{
				Name: "test_routes",
			},
			expectRDS: false,
		},
		{
			name:        "with nil route config",
			statPrefix:  "test",
			routeConfig: nil,
			expectRDS:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hcm := buildHTTPConnectionManager(tt.statPrefix, ReporterSource, tt.routeConfig)

			require.NotNil(t, hcm)
			assert.Equal(t, tt.statPrefix, hcm.GetStatPrefix())
			assert.Equal(t, http_connection_managerv3.HttpConnectionManager_AUTO, hcm.GetCodecType())
			require.Len(t, hcm.GetHttpFilters(), 1)
			assert.Equal(t, httpRouterFilterName, hcm.GetHttpFilters()[0].GetName())
		})
	}
}

func TestBuildHTTPConnectionManagerFilter(t *testing.T) {
	hcm := buildHTTPConnectionManager("test", ReporterSource, nil)
	filter := buildHTTPConnectionManagerFilter(hcm)

	require.NotNil(t, filter)
	assert.Equal(t, "envoy.http_connection_manager", filter.GetName())
	require.NotNil(t, filter.GetTypedConfig())

	var decoded http_connection_managerv3.HttpConnectionManager
	err := proto.Unmarshal(filter.GetTypedConfig().GetValue(), &decoded)
	require.NoError(t, err)
	assert.Equal(t, "test", decoded.GetStatPrefix())
}

func TestBuildSetFilterState(t *testing.T) {
	filter := buildSetFilterState("my.key", "%SOME_FORMAT%")

	require.NotNil(t, filter)
	assert.Equal(t, "envoy.filters.network.set_filter_state", filter.GetName())
	assert.NotNil(t, filter.GetTypedConfig())
}

// TestHCMDownstreamIdleTimeout verifies the shared HCM builder sets the
// downstream idle timeout backstop (Envoy default is 1h, which let a peer's
// leaked upstream connections pin thousands of inbound connections).
func TestHCMDownstreamIdleTimeout(t *testing.T) {
	hcm := buildHTTPConnectionManager("test", ReporterSource, nil)
	idle := hcm.GetCommonHttpProtocolOptions().GetIdleTimeout()
	require.NotNil(t, idle, "downstream idle timeout must be set")
	assert.Equal(t, downstreamIdleTimeout, idle.AsDuration())
	assert.Greater(t, downstreamIdleTimeout, config.UpstreamIdleTimeout,
		"downstream timeout must exceed upstream so the client side disconnects first")
}

// TestNetnsFilterStateSharedOnce verifies the netns filter state is shared with
// the immediate upstream connection only (ONCE): that is the hop where the
// service cluster's transport-socket matcher selects the source pod's cert.
// TRANSITIVE was HBONE-era multi-hop plumbing; re-propagating the source netns
// into any future chained hop could silently drive cert selection there.
func TestNetnsFilterStateSharedOnce(t *testing.T) {
	f := buildNetworkNamespaceFilterState()
	var cfg set_filter_state_v3.Config
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(&cfg))
	require.Len(t, cfg.GetOnNewConnection(), 1)
	v := cfg.GetOnNewConnection()[0]
	assert.Equal(t, setFilterStatev3.FilterStateValue_ONCE, v.GetSharedWithUpstream())
}
