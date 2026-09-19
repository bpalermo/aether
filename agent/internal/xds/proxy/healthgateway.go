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

// HealthGatewayProbe is one pod's entry on the health gateway.
//
// AppCluster is the pod's application health-probe cluster (health_<pod>) and
// InboundReadyCluster its inbound-readiness probe (inboundready_<pod>, issue
// #815), or "" when that probe is not programmed for this pod.
//
// EACH CLUSTER GETS ITS OWN PATH — /healthz/<cluster-name> — reflecting THAT
// cluster alone. #819 instead ANDed both clusters behind the single
// /healthz/health_<pod> path, which cost main-worker-03 four endpoints on
// 2026-09-19: the agent could see only the conjunction, so "the app is fine but
// the mesh inbound never came up" was indistinguishable from "the app died",
// and the liveness loop demoted the endpoints permanently with no signal. Two
// paths let the loop weigh the two facts differently, which is the whole of
// the can't-tell rule in agent/internal/cni/server/liveness.go.
type HealthGatewayProbe struct {
	AppCluster          string
	InboundReadyCluster string
}

// NewHealthGatewayProbe builds a probe entry for one pod. inboundReadyCluster
// may be "" (pod not gated).
func NewHealthGatewayProbe(appProbeCluster, inboundReadyCluster string) HealthGatewayProbe {
	return HealthGatewayProbe{AppCluster: appProbeCluster, InboundReadyCluster: inboundReadyCluster}
}

// clusters returns the probe's non-empty cluster names, each of which becomes
// its own gateway path.
func (p HealthGatewayProbe) clusters() []string {
	out := make([]string, 0, 2)
	for _, name := range []string{p.AppCluster, p.InboundReadyCluster} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// BuildHealthGatewayListener builds the agent-facing health gateway: a Unix
// domain socket listener whose HTTP filter chain holds one non-pass-through
// health_check filter per PROBE CLUSTER, matched on /healthz/<cluster-name> and
// gated on that cluster's active health check alone
// (cluster_min_healthy_percentages). The liveness loop probes these paths to
// reflect per-pod health into the registry — answered on worker threads
// from the same host-health state the load balancer uses, replacing the admin
// /clusters dump that was serialized on Envoy's main thread.
//
// Paths, per pod:
//
//	/healthz/health_<pod>        the APPLICATION probe, exactly as before #815
//	/healthz/inboundready_<pod>  the pod's own mesh inbound listener completing
//	                             an mTLS handshake with its own certificate;
//	                             absent (404) when the pod is not gated
//
// Response semantics per path: 200 = that cluster's host passes its active HC;
// 503 = it fails (which includes HC warm-up — hosts start failed until their
// first passing check); 404 (router catch-all) = not programmed. 404 is a
// first-class answer, not an error: it is how the agent learns a pod is
// UNGATED rather than unhealthy.
//
// Filters are emitted in sorted cluster order so the listener config is
// deterministic and pod-set-equal snapshots do not produce spurious LDS updates.
func BuildHealthGatewayListener(socketPath string, probes []HealthGatewayProbe) *listenerv3.Listener {
	names := make([]string, 0, 2*len(probes))
	for _, probe := range probes {
		names = append(names, probe.clusters()...)
	}
	slices.Sort(names)
	names = slices.Compact(names)

	filters := make([]*http_connection_managerv3.HttpFilter, 0, len(names)+1)
	for _, name := range names {
		filters = append(filters, healthGatewayFilter(name))
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

// healthGatewayFilter builds one probe cluster's health_check filter: requests
// to /healthz/<cluster> answer 200 only when that cluster's host passes its
// active HC, 503 otherwise; everything else passes through to the next filter.
//
// cluster_min_healthy_percentages is a proto MAP, but it holds exactly one
// entry here, and config.TypedConfig marshals the filter deterministically
// (which canonicalises map fields) before freezing it into the Any, so the
// listener's bytes never depend on Go map iteration.
func healthGatewayFilter(cluster string) *http_connection_managerv3.HttpFilter {
	return httpFilter(httpHealthCheckFilterName, &health_checkv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{
							Exact: HealthGatewayPath(cluster),
						},
					},
				},
			},
		},
		ClusterMinHealthyPercentages: map[string]*typev3.Percent{cluster: {Value: 100}},
	})
}
