package proxy

import (
	"slices"
	"strings"

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

// HealthGatewayProbe is one pod's entry on the health gateway.
//
// Cluster is the pod's app health-probe cluster (health_<pod>); it names the
// gateway PATH, so the agent's liveness loop keeps asking for
// /healthz/health_<pod> and needs no knowledge of what else gates it.
//
// Requires is every cluster that must be fully healthy for that path to answer
// 200. It always contains Cluster, and — on an mTLS pod whose inbound-readiness
// probe is programmed — also inboundready_<pod> (issue #815): an endpoint is
// only mesh-routable when its application is up AND its own mesh inbound
// listener is serving mTLS with its own certificate.
type HealthGatewayProbe struct {
	Cluster  string
	Requires []string
}

// NewHealthGatewayProbe builds a probe entry for one pod: the app probe cluster
// plus any additional clusters that must also be healthy. Empty names are
// dropped and the requirement set is sorted and de-duplicated, so the emitted
// config is a pure function of the input set.
func NewHealthGatewayProbe(appProbeCluster string, alsoRequired ...string) HealthGatewayProbe {
	required := make([]string, 0, 1+len(alsoRequired))
	required = append(required, appProbeCluster)
	for _, name := range alsoRequired {
		if name != "" {
			required = append(required, name)
		}
	}
	slices.Sort(required)
	return HealthGatewayProbe{Cluster: appProbeCluster, Requires: slices.Compact(required)}
}

// BuildHealthGatewayListener builds the agent-facing health gateway: a Unix
// domain socket listener whose HTTP filter chain holds one non-pass-through
// health_check filter per pod, each matched on /healthz/<app-probe-cluster> and
// gated on the active health check of every cluster in that pod's requirement
// set (cluster_min_healthy_percentages). The liveness loop probes these paths to
// reflect per-pod health into the registry — answered on worker threads
// from the same host-health state the load balancer uses, replacing the admin
// /clusters dump that was serialized on Envoy's main thread.
//
// Response semantics per path: 200 = every required cluster's host passes its
// active HC; 503 = at least one fails (which includes HC warm-up — hosts start
// failed until their first passing check — the liveness loop applies the warm-up
// grace); 404 (router catch-all) = the pod's filter is not programmed yet.
//
// Filters are emitted in sorted cluster order so the listener config is
// deterministic and pod-set-equal snapshots do not produce spurious LDS updates.
func BuildHealthGatewayListener(socketPath string, probes []HealthGatewayProbe) *listenerv3.Listener {
	sorted := slices.Clone(probes)
	slices.SortFunc(sorted, func(a, b HealthGatewayProbe) int { return strings.Compare(a.Cluster, b.Cluster) })

	filters := make([]*http_connection_managerv3.HttpFilter, 0, len(sorted)+1)
	for _, probe := range sorted {
		filters = append(filters, healthGatewayFilter(probe))
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
		Name:                          HealthGatewayListenerName,
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		// Exempt from overload-manager actions (stop_accepting_requests etc.):
		// the gateway answers the agent's delegated-liveness probes, whose job is
		// to report APP truth, not proxy state. Without the bypass, an overloaded
		// proxy would 503 liveness probes, the agent would mark every local pod
		// unhealthy, and the registry would flap cluster-wide on transient
		// overload. Shedding away from an overloaded node happens through the
		// data-plane listeners instead: 503'd new streams are retried by client
		// routes on a different endpoint, and active-mode client health checks
		// fail against the inbound listener. The gateway is a node-local UDS
		// with a handful of agent connections — bypassing it frees no
		// meaningful memory.
		BypassOverloadManager: true,
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
// /healthz/<app-probe-cluster> answer 200 only when every cluster in the pod's
// requirement set passes its active HC, 503 otherwise; everything else passes
// through to the next filter.
//
// cluster_min_healthy_percentages is a proto MAP, so its wire order is not the
// order built here — config.TypedConfig marshals the filter deterministically
// (which canonicalises map fields) before freezing it into the Any, so the
// listener's bytes do not depend on Go map iteration. Requires is nonetheless a
// sorted slice (NewHealthGatewayProbe) so the intent is visible at the call
// site rather than resting on that guarantee alone.
func healthGatewayFilter(probe HealthGatewayProbe) *http_connection_managerv3.HttpFilter {
	minHealthy := make(map[string]*typev3.Percent, len(probe.Requires))
	for _, cluster := range probe.Requires {
		minHealthy[cluster] = &typev3.Percent{Value: 100}
	}
	return httpFilter(httpHealthCheckFilterName, &health_checkv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{
							Exact: HealthGatewayPath(probe.Cluster),
						},
					},
				},
			},
		},
		ClusterMinHealthyPercentages: minHealthy,
	})
}
