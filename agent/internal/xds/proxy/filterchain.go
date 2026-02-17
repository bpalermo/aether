package proxy

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func buildDefaultInboundHTTPFilterChain(name string) *listenerv3.FilterChain {
	routeConfig := buildInboundRouteConfiguration()

	hcm := buildHTTPConnectionManager(name, routeConfig)
	hcm.ForwardClientCertDetails = http_connection_managerv3.HttpConnectionManager_SANITIZE
	hcm.SetCurrentClientCertDetails = &http_connection_managerv3.HttpConnectionManager_SetCurrentClientCertDetails{
		Subject: wrapperspb.Bool(true),
	}

	var networkFilters []*listenerv3.Filter
	networkFilters = append(networkFilters, buildHTTPConnectionManagerFilter(hcm))

	return &listenerv3.FilterChain{
		Name:    fmt.Sprintf("in_http_%s", name),
		Filters: networkFilters,
	}
}

func buildDefaultOutboundHTTPFilterChain(name string) *listenerv3.FilterChain {
	hcm := buildHTTPConnectionManager(name, nil)

	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: "out_http",
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	var networkFilters []*listenerv3.Filter
	networkFilters = append(networkFilters, buildNetworkNamespaceFilterState())
	networkFilters = append(networkFilters, buildHTTPConnectionManagerFilter(hcm))

	return &listenerv3.FilterChain{
		Name:    fmt.Sprintf("out_http_%s", name),
		Filters: networkFilters,
	}
}
