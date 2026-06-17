package proxy

import (
	"regexp"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	previoushostsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/host/previous_hosts/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
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
// from: ":authority" makes the requested authority the cluster name. Cluster
// names ARE mesh authorities (<service>.<meshDomain>, see ServiceClusterName),
// so an undeclared upstream reaches the on_demand filter as a well-formed
// cluster reference for ODCDS to fetch — deterministically, with no
// name translation anywhere.
const onDemandClusterHeader = ":authority"

// buildOnDemandCatchAllVirtualHost builds the lowest-priority outbound virtual
// host (domain "*"). With the authority port retained (strip_any_host_port
// off, for FQDN:port routing), a scoped catch-all "*.<meshDomain>" can't match
// host:port, so the catch-all is universal and splits by an :authority regex:
// a mesh-shaped authority (<svc>.<meshDomain> with an optional :port) routes to
// the cluster named by the authority — the on_demand filter fetches it via
// ODCDS (proposal 004 cold path); anything else (foreign domains, typos)
// 404s immediately. This keeps instant foreign-404 determinism (spike-verified,
// Envoy 1.38) while supporting FQDN:port (proposal 005). Nonexistent in-domain
// services/ports fail when the ODCDS timeout expires.
func buildOnDemandCatchAllVirtualHost(meshDomain string) *routev3.VirtualHost {
	meshAuthorityRegex := "^[a-z0-9-]+\\." + regexp.QuoteMeta(meshDomain) + "(:[0-9]+)?$"
	return &routev3.VirtualHost{
		Name:    "on_demand_catch_all",
		Domains: []string{"*"},
		Routes: []*routev3.Route{
			// Egress liveness: a local-reply 200 on MeshLivePath, answered without
			// leaving the proxy (no upstream, no app), for the synthetic mesh
			// availability prober (proposal 013). First so it wins for this exact
			// path regardless of authority; the prober uses a reserved non-service
			// authority so it lands on this catch-all vhost. A 200 proves this
			// node's egress listener is serving + config loaded; a connection error
			// proves it is not — the signal aether_stats (proxy-emitted) is
			// structurally blind to during hot restarts.
			{
				Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: MeshLivePath}},
				Action: &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 200},
				},
			},
			{
				Match: &routev3.RouteMatch{
					PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
					Headers: []*routev3.HeaderMatcher{
						{
							Name: ":authority",
							HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
								StringMatch: &matcherv3.StringMatcher{
									MatchPattern: &matcherv3.StringMatcher_SafeRegex{
										SafeRegex: &matcherv3.RegexMatcher{Regex: meshAuthorityRegex},
									},
								},
							},
						},
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
			{
				Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
				Action: &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 404},
				},
			},
		},
	}
}

// BuildOutboundRouteConfiguration creates a route configuration for outbound traffic.
// It includes the provided virtual hosts plus the universal on-demand catch-all
// virtual host (mesh-shaped authorities → ODCDS, everything else → 404).
func BuildOutboundRouteConfiguration(vhosts []*routev3.VirtualHost, meshDomain string) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         OutboundHTTPRouteName,
		VirtualHosts: append(vhosts, buildOnDemandCatchAllVirtualHost(meshDomain)),
	}
}

// BuildOutboundClusterVirtualHost creates a virtual host routing the given
// authority domains to clusterName. The default-port cluster passes two domains
// (portless FQDN + FQDN:defaultPort) so both spellings reach it; a non-default
// port cluster passes the single FQDN:port. The authority port is meaningful
// (strip_any_host_port off), so exact-domain precedence keeps a ported
// authority from matching the portless default vhost (spike-verified).
func BuildOutboundClusterVirtualHost(clusterName string, domains []string) *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    clusterName,
		Domains: domains,
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
