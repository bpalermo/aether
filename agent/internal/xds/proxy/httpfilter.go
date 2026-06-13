package proxy

import (
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	"github.com/bpalermo/aether/common/constants"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	health_checkv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	on_demandv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/on_demand/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// httpRouterFilterName is the Envoy HTTP router filter name
	httpRouterFilterName = "envoy.filters.http.router"
	// httpHealthCheckFilterName is the Envoy HTTP health_check filter name
	httpHealthCheckFilterName = "envoy.filters.http.health_check"
	// httpOnDemandFilterName is the Envoy HTTP on_demand filter name (ODCDS)
	httpOnDemandFilterName = "envoy.filters.http.on_demand"

	// onDemandClusterTimeout bounds how long a request to a not-yet-distributed
	// cluster is paused while ODCDS fetches it from the node-local agent. With
	// the leading-edge reload + catalog-gated RPC fill, a legitimate cold warm
	// completes in tens of milliseconds, so this bound is purely the ghost
	// cap: requests for catalog-rejected services fail when it expires.
	onDemandClusterTimeout = 2 * time.Second
)

// routerHttpFilter creates a router HTTP filter for forwarding matched requests to clusters.
func routerHttpFilter() *http_connection_managerv3.HttpFilter {
	return httpFilter(httpRouterFilterName, &routerv3.Router{})
}

// onDemandHttpFilter creates the on_demand HTTP filter with ODCDS pointing at
// the agent's ADS (proposal 004 cold path): when an outbound request routes to
// a cluster the node snapshot does not carry (an undeclared upstream, reached
// via the catch-all authority route), the request pauses while Envoy requests
// the cluster on demand; the agent observes the miss, adds the service to the
// node dependency set (TTL'd), and serves it from the registrar snapshot.
func onDemandHttpFilter() *http_connection_managerv3.HttpFilter {
	return httpFilter(httpOnDemandFilterName, &on_demandv3.OnDemand{
		Odcds: &on_demandv3.OnDemandCds{
			Source:  config.XDSConfigSourceADS(),
			Timeout: durationpb.New(onDemandClusterTimeout),
		},
	})
}

// readinessHttpFilter creates a non-pass-through health_check filter answering
// GET <ProxyReadinessPath> directly from worker threads with pure server state:
// 200 while this Envoy epoch is serving, 503 while it is draining (hot restart).
// No cluster percentages are checked. The CNI plugin probes it from inside the
// pod's netns to confirm the data plane works before pod start completes; unlike
// an admin /config_dump check, a 200 proves the listener socket is bound in that
// netns and accepting on workers, not just that config was accepted.
func readinessHttpFilter() *http_connection_managerv3.HttpFilter {
	return httpFilter(httpHealthCheckFilterName, &health_checkv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{
							Exact: constants.ProxyReadinessPath,
						},
					},
				},
			},
		},
	})
}

// httpFilter creates an HTTP filter with the given name and configuration.
func httpFilter(name string, msg proto.Message) *http_connection_managerv3.HttpFilter {
	return &http_connection_managerv3.HttpFilter{
		Name: name,
		ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
