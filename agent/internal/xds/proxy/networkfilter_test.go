package proxy

import (
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
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
			hcm := buildHTTPConnectionManager(tt.statPrefix, tt.routeConfig)

			require.NotNil(t, hcm)
			assert.Equal(t, tt.statPrefix, hcm.GetStatPrefix())
			assert.Equal(t, http_connection_managerv3.HttpConnectionManager_AUTO, hcm.GetCodecType())
			require.Len(t, hcm.GetHttpFilters(), 1)
			assert.Equal(t, httpRouterFilterName, hcm.GetHttpFilters()[0].GetName())
		})
	}
}

func TestBuildHTTPConnectionManagerFilter(t *testing.T) {
	hcm := buildHTTPConnectionManager("test", nil)
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
