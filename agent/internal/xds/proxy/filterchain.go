package proxy

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
)

// buildDefaultOutboundHTTPFilterChain creates a filter chain for outbound HTTP traffic.
// It uses RDS (Route Discovery Service) to dynamically fetch routes and includes
// network namespace filter state. The chain NAME stays per-pod; the HCM stat
// prefix is shared across pods (cardinality round 2) — per-pod egress
// attribution rides tags and cluster stats, not HCM stat names.
func buildDefaultOutboundHTTPFilterChain(name string) *listenerv3.FilterChain {
	hcm := buildHTTPConnectionManager("outbound_http", nil)

	// Mesh authorities are FQDN-only and the catch-all routes on the raw
	// authority as a cluster name: strip any :port from the authority before
	// filters and routing so <svc>.<domain>:8080 matches the same vhost and
	// names the same on-demand cluster as the portless form.
	hcm.StripPortMode = &http_connection_managerv3.HttpConnectionManager_StripAnyHostPort{StripAnyHostPort: true}

	// Readiness probe target: answered on worker threads ahead of the router, so
	// the CNI plugin's in-netns probe never depends on routes or upstreams.
	// The on_demand filter (before the router) pauses requests routed to a
	// cluster the scoped snapshot does not carry while ODCDS fetches it from
	// the agent (proposal 004 cold path).
	hcm.HttpFilters = append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter(), onDemandHttpFilter()}, hcm.HttpFilters...)

	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: OutboundHTTPRouteName,
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
