package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// SpireAgentSDSClusterName is the bootstrap-defined static cluster the edge
	// proxy's transport sockets reference for SDS. It points at the SPIRE Agent's
	// Workload API socket, which serves the Envoy SDS API natively (the edge's
	// own SVID + trust bundle), so the edge needs no agent-side SPIRE bridge.
	SpireAgentSDSClusterName = "spire_agent"

	// EdgeHTTPRouteName is the RDS route configuration name the edge listener
	// references; the edge agent publishes the exposed-service virtual hosts
	// under it.
	EdgeHTTPRouteName = "edge_http"

	// EdgeListenerName is the name of the edge proxy's public-facing listener
	// (plain HTTP, or HTTPS-terminating when TLS is enabled).
	EdgeListenerName = "edge_http"

	// EdgeRedirectListenerName is the :80 listener that 301-redirects to https
	// when TLS is enabled.
	EdgeRedirectListenerName = "edge_redirect"

	// DefaultEdgeHTTPPort is the port the edge plain-HTTP / redirect listener
	// binds for external (north-south) traffic.
	DefaultEdgeHTTPPort = 8080

	// DefaultEdgeHTTPSPort is the port the edge TLS-terminating listener binds
	// when downstream TLS is enabled.
	DefaultEdgeHTTPSPort = 8443

	// defaultEdgeAddress binds the edge listener on all interfaces (it fronts
	// external traffic, unlike the node proxy's loopback-only outbound listener).
	defaultEdgeAddress = "0.0.0.0"
)

// BuildEdgeListener builds the edge proxy's public-facing HTTP listener: an HCM
// on 0.0.0.0:<port> that routes external traffic to mesh services via RDS
// (EdgeHTTPRouteName). The readiness health_check filter answers
// ProxyReadinessPath ahead of the router (backs the pod readinessProbe and is
// drain-aware), and the router forwards to the per-service clusters, which dial
// destination pods directly over single-identity mTLS. No on_demand/subset
// filters: the edge's routable set is its explicit exposed services, not the
// whole mesh.
//
// When tlsSecretNames is non-empty the filter chain terminates downstream TLS,
// presenting one of those SDS-served certs selected by SNI; otherwise the edge
// serves plain HTTP. Either way the upstream (edge -> pod) hop stays mTLS.
func BuildEdgeListener(port uint32, tlsSecretNames []string) *listenerv3.Listener {
	hcm := buildHTTPConnectionManager(EdgeListenerName, ReporterSource, "", "", nil)
	hcm.HttpFilters = append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter()}, hcm.HttpFilters...)
	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: EdgeHTTPRouteName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	filterChain := &listenerv3.FilterChain{
		Name:    EdgeListenerName,
		Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
	}
	if len(tlsSecretNames) > 0 {
		filterChain.TransportSocket = edgeDownstreamTLSFromSDS(tlsSecretNames)
	}

	return edgeListener(EdgeListenerName, port, filterChain)
}

// BuildEdgeRedirectListener builds the :80 listener that 301-redirects every
// request to its https URL (used alongside the TLS listener when downstream TLS
// is enabled).
func BuildEdgeRedirectListener(port uint32) *listenerv3.Listener {
	hcm := buildHTTPConnectionManager(EdgeRedirectListenerName, ReporterSource, "", "", buildEdgeRedirectRouteConfig())
	filterChain := &listenerv3.FilterChain{
		Name:    EdgeRedirectListenerName,
		Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
	}
	return edgeListener(EdgeRedirectListenerName, port, filterChain)
}

// edgeListener wraps a filter chain in a 0.0.0.0:<port> INBOUND listener.
func edgeListener(name string, port uint32, fc *listenerv3.FilterChain) *listenerv3.Listener {
	return &listenerv3.Listener{
		Name: name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultEdgeAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: port,
					},
				},
			},
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		StatPrefix:                    name,
		TrafficDirection:              corev3.TrafficDirection_INBOUND,
		FilterChains:                  []*listenerv3.FilterChain{fc},
	}
}

// buildEdgeRedirectRouteConfig is the inline route table for the redirect
// listener: a catch-all that rewrites the scheme to https (301).
func buildEdgeRedirectRouteConfig() *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name: EdgeRedirectListenerName,
		VirtualHosts: []*routev3.VirtualHost{
			{
				Name:    EdgeRedirectListenerName,
				Domains: []string{"*"},
				Routes: []*routev3.Route{
					{
						Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
						Action: &routev3.Route_Redirect{
							Redirect: &routev3.RedirectAction{
								SchemeRewriteSpecifier: &routev3.RedirectAction_HttpsRedirect{HttpsRedirect: true},
							},
						},
					},
				},
			},
		},
	}
}

// BuildEdgeRouteConfiguration builds the edge listener's route table from the
// exposed-service virtual hosts. Unlike the node proxy's outbound route config,
// it has NO on-demand catch-all (the edge serves only its explicit exposed set);
// an unmatched authority gets an immediate 404 from the catch-all default vhost.
func BuildEdgeRouteConfiguration(vhosts []*routev3.VirtualHost) *routev3.RouteConfiguration {
	all := append([]*routev3.VirtualHost{}, vhosts...)
	all = append(all, &routev3.VirtualHost{
		Name:    "edge_not_found",
		Domains: []string{"*"},
		Routes: []*routev3.Route{
			{
				Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
				Action: &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 404},
				},
			},
		},
	})
	return &routev3.RouteConfiguration{
		Name:         EdgeHTTPRouteName,
		VirtualHosts: all,
	}
}
