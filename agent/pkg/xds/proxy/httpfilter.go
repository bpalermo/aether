package proxy

import (
	"github.com/bpalermo/aether/agent/pkg/xds/config"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/proto"
)

func routerHttpFilter() *http_connection_managerv3.HttpFilter {
	return httpFilter("envoy.filters.http.router", &routerv3.Router{})
}

func httpFilter(name string, msg proto.Message) *http_connection_managerv3.HttpFilter {
	return &http_connection_managerv3.HttpFilter{
		Name: name,
		ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
