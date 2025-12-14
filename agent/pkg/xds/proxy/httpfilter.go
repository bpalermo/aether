package proxy

import (
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func routerHttpFilter() *http_connection_managerv3.HttpFilter {
	router := &routerv3.Router{}

	return httpFilter("envoy.filters.http.router", router)
}

func httpFilter(name string, config proto.Message) *http_connection_managerv3.HttpFilter {
	typedConfig, _ := anypb.New(config)

	return &http_connection_managerv3.HttpFilter{
		Name: name,
		ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
			TypedConfig: typedConfig,
		},
	}
}
