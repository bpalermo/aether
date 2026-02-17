package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/proto"
)

const (
	httpRouterFilterName = "envoy.filters.http.router"
)

func routerHttpFilter() *http_connection_managerv3.HttpFilter {
	return httpFilter(httpRouterFilterName, &routerv3.Router{})
}

func httpFilter(name string, msg proto.Message) *http_connection_managerv3.HttpFilter {
	return &http_connection_managerv3.HttpFilter{
		Name: name,
		ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
