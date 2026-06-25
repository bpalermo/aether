package proxy

import (
	"fmt"

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
	// binds for external (north-south) traffic. Privileged (the edge is an ingress
	// gateway); the pod is granted NET_BIND_SERVICE, so it binds unprivileged.
	DefaultEdgeHTTPPort = 80

	// DefaultEdgeHTTPSPort is the port the edge TLS-terminating listener binds
	// when downstream TLS is enabled.
	DefaultEdgeHTTPSPort = 443

	// defaultEdgeAddress binds the edge listener on all interfaces (it fronts
	// external traffic, unlike the node proxy's loopback-only outbound listener).
	defaultEdgeAddress = "0.0.0.0"
)

// EdgeTCPListenerName returns the name of an edge TCP listener for a given port.
// TCPRoute listeners are named edge_tcp_<port> so each Gateway TCP listener
// gets its own Envoy listener and stats prefix.
func EdgeTCPListenerName(port uint32) string {
	return fmt.Sprintf("edge_tcp_%d", port)
}

// EdgeTLSListenerName returns the name of an edge TLS passthrough listener for
// a given port. TLSRoute (Passthrough) listeners are named edge_tls_<port>.
func EdgeTLSListenerName(port uint32) string {
	return fmt.Sprintf("edge_tls_%d", port)
}

// EdgeL4TCPRoute describes one edge TCP listener: a Gateway TCP listener on
// port with all backends consolidated from the attached TCPRoute rules.
type EdgeL4TCPRoute struct {
	// Port is the Gateway listener port (TCP protocol) this route is bound to.
	Port uint32
	// Backends is the flat weighted backend list from all attached TCPRoute rules.
	Backends []L4Backend
}

// EdgeL4TLSRoute describes one edge TLS passthrough listener: a Gateway TLS
// listener (mode: Passthrough) on port with per-SNI route rules.
type EdgeL4TLSRoute struct {
	// Port is the Gateway listener port (TLS protocol, Passthrough mode).
	Port uint32
	// Rules are the ordered per-SNI rules from all attached TLSRoutes; each
	// rule's SNIHostnames become a filter_chain_match.server_names set.
	Rules []L4ServiceRoute
}

// BuildEdgeTCPListener builds a 0.0.0.0:<port> TCP listener for an edge TCPRoute.
// It routes all inbound TCP to the named upstream cluster via tcp_proxy (no TLS
// termination — the upstream hop to the mesh pod is SPIRE mTLS with no ALPN).
// The cluster name is the "tcp:<svc>.<meshDomain>" cluster whose transport socket
// is injected at snapshot time via InjectUpstreamTCPMTLS (same path as the east-west
// TCP floor). When rules is empty or all backends are missing, the listener is
// omitted (returns nil) — a Gateway TCP listener with no TCPRoute attached should
// not produce an Envoy listener.
func BuildEdgeTCPListener(port uint32, backends []L4Backend) *listenerv3.Listener {
	clusters := l4BackendsToWeightedClusters(backends)
	tcpProxy := buildWeightedTCPProxy(EdgeTCPListenerName(port), clusters)
	if tcpProxy == nil {
		return nil
	}
	fc := &listenerv3.FilterChain{
		Name:    EdgeTCPListenerName(port),
		Filters: []*listenerv3.Filter{tcpProxy},
	}
	return edgeListener(EdgeTCPListenerName(port), port, fc)
}

// BuildEdgeTLSPassthroughListener builds a 0.0.0.0:<port> TLS passthrough listener
// for an edge TLSRoute. It installs a tls_inspector listener filter so Envoy reads
// the client SNI; each rule's SNIHostnames become a filter_chain_match.server_names
// set routing via tcp_proxy to the rule's backends. No TLS termination: the SNI is
// read for routing only; the backend connection is SPIRE mTLS with no ALPN (TCP floor).
// Returns nil if rules is empty or none has both SNI hostnames and backends.
func BuildEdgeTLSPassthroughListener(port uint32, rules []L4ServiceRoute) *listenerv3.Listener {
	var chains []*listenerv3.FilterChain
	for i, rule := range rules {
		if len(rule.SNIHostnames) == 0 || len(rule.Backends) == 0 {
			continue
		}
		statPrefix := fmt.Sprintf("%s_%d", EdgeTLSListenerName(port), i)
		clusters := l4BackendsToWeightedClusters(rule.Backends)
		tcpProxy := buildWeightedTCPProxy(statPrefix, clusters)
		if tcpProxy == nil {
			continue
		}
		chains = append(chains, &listenerv3.FilterChain{
			Name: fmt.Sprintf("%s_%d", EdgeTLSListenerName(port), i),
			FilterChainMatch: &listenerv3.FilterChainMatch{
				ServerNames:       rule.SNIHostnames,
				TransportProtocol: "tls",
			},
			Filters: []*listenerv3.Filter{tcpProxy},
		})
	}
	if len(chains) == 0 {
		return nil
	}
	ln := &listenerv3.Listener{
		Name: EdgeTLSListenerName(port),
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
		StatPrefix:                    EdgeTLSListenerName(port),
		TrafficDirection:              corev3.TrafficDirection_INBOUND,
		// The tls_inspector reads the ClientHello SNI so filter_chain_match.server_names
		// works. No TLS termination: the SNI is for routing only; the raw mTLS stream
		// passes through to the backend.
		ListenerFilters: []*listenerv3.ListenerFilter{tlsInspector()},
		FilterChains:    chains,
	}
	return ln
}

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

// BuildEdgeRoute builds one Envoy route for the edge: a path match forwarding to
// the named cluster with the outbound retry policy. Exactly one of prefix/exact
// is non-empty (exact takes precedence if both are set). Prefix "/" matches all.
// mutation applies RequestHeaderModifier / ResponseHeaderModifier at the route
// level; nil mutation = no header mutations.
func BuildEdgeRoute(prefix, exact, cluster string, mutation *GammaHeaderMutation) *routev3.Route {
	match := &routev3.RouteMatch{}
	if exact != "" {
		match.PathSpecifier = &routev3.RouteMatch_Path{Path: exact}
	} else {
		match.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: prefix}
	}
	reqAdd, respAdd, reqRemove, respRemove := headerMutationFields(mutation)
	return &routev3.Route{
		Match: match,
		Action: &routev3.Route_Route{
			Route: &routev3.RouteAction{
				ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster},
				RetryPolicy:      outboundRetryPolicy(),
			},
		},
		RequestHeadersToAdd:     reqAdd,
		RequestHeadersToRemove:  reqRemove,
		ResponseHeadersToAdd:    respAdd,
		ResponseHeadersToRemove: respRemove,
	}
}

// BuildEdgeVirtualHost builds one edge virtual host: the given external domains
// with an ordered list of routes (first match wins, per VirtualHost CR order).
func BuildEdgeVirtualHost(name string, domains []string, routes []*routev3.Route) *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    name,
		Domains: domains,
		Routes:  routes,
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
