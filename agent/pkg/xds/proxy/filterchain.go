package proxy

import (
	"fmt"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
)

func buildDefaultOutboundHTTPFilterChain(name string) *listenerv3.FilterChain {
	routeConfig := buildOutboundRouteConfiguration()

	var networkFilters []*listenerv3.Filter
	networkFilters = append(networkFilters, buildNetworkNamespaceFilterState())
	networkFilters = append(networkFilters, buildHTTPConnectionManager(name, routeConfig))

	return &listenerv3.FilterChain{
		Name:    fmt.Sprintf("outbound_http_%s", name),
		Filters: networkFilters,
	}
}
