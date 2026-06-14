package proxy

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
)

// buildDefaultOutboundHTTPFilterChain creates a filter chain for outbound HTTP traffic.
// It uses RDS (Route Discovery Service) to dynamically fetch routes and includes
// network namespace filter state. The chain NAME stays per-pod; the HCM stat
// prefix is shared across pods (cardinality round 2) — per-pod egress
// attribution rides tags and cluster stats, not HCM stat names.
//
// The source-reported stats dynamic module (proposal 007) is attached just
// before the router so it observes the final route/cluster. The chart mounts the
// module .so on the proxy unconditionally; Envoy rejects the listener if the
// referenced dynamic module is absent.
func buildDefaultOutboundHTTPFilterChain(cniPod *cniv1.CNIPod, meshDomain string) *listenerv3.FilterChain {
	hcm := buildHTTPConnectionManager("outbound_http", nil)

	// strip_any_host_port stays OFF: the authority port is a first-class routing
	// selector (FQDN:port → that port's cluster, proposal 005). The default-port
	// cluster's vhost lists both the portless and :defaultPort domains so the
	// default is reachable either way.

	// Readiness probe target: answered on worker threads ahead of the router, so
	// the CNI plugin's in-netns probe never depends on routes or upstreams.
	// The on_demand filter (before the router) pauses requests routed to a
	// cluster the scoped snapshot does not carry while ODCDS fetches it from
	// the agent (proposal 004 cold path).
	// The subset-headers filter (ECDS-discovered, shared node-wide) turns
	// x-aether-ip/x-aether-pod/x-aether-subset-* request headers into
	// envoy.lb match criteria ahead of routing.
	// The stats filter runs last before the router so it observes the final
	// route/cluster and response disposition at the log phase.
	prefix := []*http_connection_managerv3.HttpFilter{
		readinessHttpFilter(),
		subsetHeadersHttpFilter(),
		onDemandHttpFilter(),
		outboundStatsFilter(cniPod, meshDomain),
	}
	hcm.HttpFilters = append(prefix, hcm.HttpFilters...)

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
		Name:    fmt.Sprintf("out_http_%s", cniPod.GetName()),
		Filters: networkFilters,
	}
}
