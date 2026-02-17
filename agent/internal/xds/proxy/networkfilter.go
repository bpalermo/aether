package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	"google.golang.org/protobuf/proto"
)

const (
	networkNamespaceFilterStateKey = "aether.network.network_namespace"
)

func buildNetworkNamespaceFilterState() *listenerv3.Filter {
	return buildSetFilterState(networkNamespaceFilterStateKey, "%FILTER_STATE(envoy.network.network_namespace:PLAIN)%")
}

func buildSetFilterState(objectKey string, inlineStringFormatString string) *listenerv3.Filter {
	filter := &set_filter_state_v3.Config{
		OnNewConnection: []*setFilterStatev3.FilterStateValue{
			{
				Key: &setFilterStatev3.FilterStateValue_ObjectKey{
					ObjectKey: objectKey,
				},
				FactoryKey: "envoy.string",
				Value: &setFilterStatev3.FilterStateValue_FormatString{
					FormatString: &corev3.SubstitutionFormatString{
						Format: &corev3.SubstitutionFormatString_TextFormatSource{
							TextFormatSource: &corev3.DataSource{
								Specifier: &corev3.DataSource_InlineString{
									InlineString: inlineStringFormatString,
								},
							},
						},
					},
				},
				SharedWithUpstream: setFilterStatev3.FilterStateValue_ONCE,
			},
		},
	}
	return networkFilter("envoy.filters.network.set_filter_state", filter)
}

func buildHTTPConnectionManager(name string, routeConfig *routev3.RouteConfiguration) *http_connection_managerv3.HttpConnectionManager {
	return &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  http_connection_managerv3.HttpConnectionManager_AUTO,
		HttpFilters: []*http_connection_managerv3.HttpFilter{
			routerHttpFilter(),
		},
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: routeConfig,
		},
	}
}

func buildHTTPConnectionManagerFilter(config *http_connection_managerv3.HttpConnectionManager) *listenerv3.Filter {
	return networkFilter("envoy.http_connection_manager", config)
}

func networkFilter(name string, msg proto.Message) *listenerv3.Filter {
	return &listenerv3.Filter{
		Name: name,
		ConfigType: &listenerv3.Filter_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
