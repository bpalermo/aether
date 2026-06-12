package proxy

import (
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	previoushostsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/host/previous_hosts/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// OutboundHTTPRouteName is the name of the outbound HTTP route configuration
	OutboundHTTPRouteName = "out_http"
)

// outboundRetryPolicy returns the retry policy applied to every client-side
// service route. It masks the sub-second windows inherent to endpoint churn —
// a dial racing a pod that just received SIGTERM (connection refused before the
// EDS removal propagates) or a 503 while a replacement endpoint finishes its
// first health-check round — by retrying on a *different* host
// (previous_hosts predicate).
//
// Only conditions that are safe for non-idempotent requests are retried:
// connect-failure, refused-stream and reset-before-request all fail before the
// request reaches an application, and 503 is the standard
// "try-another-endpoint" drain signal (Envoy's no-healthy-upstream and
// service overload both use it; applications returning 503 explicitly opt
// into retry semantics).
func outboundRetryPolicy() *routev3.RetryPolicy {
	return &routev3.RetryPolicy{
		RetryOn:              "connect-failure,refused-stream,reset-before-request,retriable-status-codes",
		NumRetries:           wrapperspb.UInt32(2),
		RetriableStatusCodes: []uint32{503},
		RetryHostPredicate: []*routev3.RetryPolicy_RetryHostPredicate{{
			Name: "envoy.retry_host_predicates.previous_hosts",
			ConfigType: &routev3.RetryPolicy_RetryHostPredicate_TypedConfig{
				TypedConfig: config.TypedConfig(&previoushostsv3.PreviousHostsPredicate{}),
			},
		}},
		HostSelectionRetryMaxAttempts: 3,
		RetryBackOff: &routev3.RetryPolicy_RetryBackOff{
			BaseInterval: durationpb.New(25 * time.Millisecond),
			MaxInterval:  durationpb.New(250 * time.Millisecond),
		},
	}
}

// onDemandClusterHeader is the header the catch-all route resolves its cluster
// from: ":authority" makes the requested service name the cluster name, so an
// undeclared upstream called by its bare service name reaches the on_demand
// filter as a well-formed cluster reference for ODCDS to fetch.
const onDemandClusterHeader = ":authority"

// buildOnDemandCatchAllVirtualHost builds the lowest-priority ("*") outbound
// virtual host: requests whose authority matches no distributed service vhost
// route to the cluster named by the authority. The scoped snapshot does not
// carry that cluster, so the on_demand HTTP filter pauses the request and
// fetches it via ODCDS (proposal 004 cold path). Cold requests therefore work
// for bare service names (svc-name, no port); nonexistent services fail when
// the ODCDS timeout expires.
func buildOnDemandCatchAllVirtualHost() *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    "on_demand_catch_all",
		Domains: []string{"*"},
		Routes: []*routev3.Route{
			{
				Match: &routev3.RouteMatch{
					PathSpecifier: &routev3.RouteMatch_Prefix{
						Prefix: "/",
					},
				},
				Action: &routev3.Route_Route{
					Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_ClusterHeader{
							ClusterHeader: onDemandClusterHeader,
						},
						RetryPolicy: outboundRetryPolicy(),
					},
				},
			},
		},
	}
}

// BuildOutboundRouteConfiguration creates a route configuration for outbound traffic.
// It includes the provided virtual hosts plus the on-demand catch-all virtual
// host, which routes unmatched authorities by name for ODCDS resolution.
// Each service virtual host matches requests by cluster name and FQDN.
func BuildOutboundRouteConfiguration(vhosts []*routev3.VirtualHost) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         OutboundHTTPRouteName,
		VirtualHosts: append(vhosts, buildOnDemandCatchAllVirtualHost()),
	}
}

// BuildOutboundClusterVirtualHost creates a virtual host that routes traffic to a specific cluster.
// The virtual host matches both the cluster name and its FQDN (cluster.aether.internal).
func BuildOutboundClusterVirtualHost(clusterName string) *routev3.VirtualHost {
	fqdn := fmt.Sprintf("%s.%s", clusterName, "aether.internal")
	return &routev3.VirtualHost{
		Name:    clusterName,
		Domains: []string{clusterName, fqdn},
		Routes: []*routev3.Route{
			{
				Match: &routev3.RouteMatch{
					PathSpecifier: &routev3.RouteMatch_Prefix{
						Prefix: "/",
					},
				},
				Action: &routev3.Route_Route{
					Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{
							Cluster: clusterName,
						},
						RetryPolicy: outboundRetryPolicy(),
					},
				},
			},
		},
	}
}
