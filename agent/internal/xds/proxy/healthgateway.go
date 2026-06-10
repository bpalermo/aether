package proxy

import (
	"slices"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	health_checkv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// HealthGatewayListenerName names the node-local health gateway listener.
	HealthGatewayListenerName = "health_gateway"
	// healthGatewayPathPrefix prefixes per-pod health paths on the gateway.
	healthGatewayPathPrefix = "/healthz/"
)

// HealthGatewayPath returns the gateway path that reflects the active health
// of the given per-pod health-probe cluster (health_<pod>).
func HealthGatewayPath(probeClusterName string) string {
	return healthGatewayPathPrefix + probeClusterName
}

// BuildHealthGatewayListener builds the agent-facing health gateway: a Unix
// domain socket listener whose HTTP filter chain holds one non-pass-through
// health_check filter per health-probe cluster, each matched on
// /healthz/<cluster> and gated on that cluster's active health check
// (cluster_min_healthy_percentages). The liveness loop probes these paths to
// reflect per-pod app health into the registry — answered on worker threads
// from the same host-health state the load balancer uses, replacing the admin
// /clusters dump that was serialized on Envoy's main thread.
//
// Response semantics per path: 200 = the pod's host passes its active HC;
// 503 = it fails (which includes HC warm-up — hosts start failed until their
// first passing check — the liveness loop applies the warm-up grace);
// 404 (router catch-all) = the pod's filter is not programmed yet.
//
// Filters are emitted in sorted cluster order so the listener config is
// deterministic and pod-set-equal snapshots do not produce spurious LDS updates.
func BuildHealthGatewayListener(socketPath string, probeClusters []string) *listenerv3.Listener {
	sorted := slices.Clone(probeClusters)
	slices.Sort(sorted)

	filters := make([]*http_connection_managerv3.HttpFilter, 0, len(sorted)+1)
	for _, cluster := range sorted {
		filters = append(filters, healthGatewayFilter(cluster))
	}
	filters = append(filters, routerHttpFilter())

	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix:  HealthGatewayListenerName,
		CodecType:   http_connection_managerv3.HttpConnectionManager_AUTO,
		HttpFilters: filters,
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: &routev3.RouteConfiguration{
				Name: HealthGatewayListenerName,
				VirtualHosts: []*routev3.VirtualHost{
					{
						Name:    "health_gateway_catch_all",
						Domains: []string{"*"},
						Routes: []*routev3.Route{
							{
								Match: &routev3.RouteMatch{
									PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
								},
								// Unknown path = pod not programmed (yet): the
								// liveness loop treats 404 as "skip, not known".
								Action: &routev3.Route_DirectResponse{
									DirectResponse: &routev3.DirectResponseAction{Status: 404},
								},
							},
						},
					},
				},
			},
		},
	}

	return &listenerv3.Listener{
		Name: HealthGatewayListenerName,
		Address: &corev3.Address{
			Address: &corev3.Address_Pipe{
				Pipe: &corev3.Pipe{Path: socketPath},
			},
		},
		StatPrefix: HealthGatewayListenerName,
		FilterChains: []*listenerv3.FilterChain{
			{
				Name:    HealthGatewayListenerName,
				Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
			},
		},
	}
}

// healthGatewayFilter builds the per-pod health_check filter: requests to
// /healthz/<cluster> answer 200/503 from the cluster's active-HC host health;
// everything else passes through to the next filter.
func healthGatewayFilter(probeCluster string) *http_connection_managerv3.HttpFilter {
	return httpFilter(httpHealthCheckFilterName, &health_checkv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{
							Exact: HealthGatewayPath(probeCluster),
						},
					},
				},
			},
		},
		ClusterMinHealthyPercentages: map[string]*typev3.Percent{
			probeCluster: {Value: 100},
		},
	})
}
