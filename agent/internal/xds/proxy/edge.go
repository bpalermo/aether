package proxy

import (
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/durationpb"
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

	// EdgeListenerName is the name of the edge proxy's plain-HTTP public listener.
	EdgeListenerName = "edge_http"

	// EdgeHTTPSListenerName is the name of the edge proxy's TLS-terminating public
	// listener. It MUST differ from EdgeListenerName: when TLS is enabled and no
	// Gateway opts into HTTP→HTTPS redirect, both the HTTPS (:443) and the plain-HTTP
	// (:80) routing listeners are emitted; a shared name collides in the snapshot/LDS
	// and one clobbers the other (regression that dropped the :443 listener).
	EdgeHTTPSListenerName = "edge_https"

	// EdgeRedirectListenerName is the :80 listener that 301-redirects to https
	// when TLS is enabled.
	EdgeRedirectListenerName = "edge_redirect"

	// EdgeReadinessListenerName is a dedicated, always-bound plain-HTTP listener
	// serving only the drain-aware /aether/readyz endpoint. It is independent of
	// any Gateway listener so the edge readiness probe has a stable target even
	// under proposal 021 Phase 2 (where the public listeners move to per-Gateway
	// internal ports and nothing binds the HTTPS port).
	EdgeReadinessListenerName = "edge_readiness"

	// DefaultEdgeHTTPPort is the port the edge plain-HTTP / redirect listener
	// binds for external (north-south) traffic. Privileged (the edge is an ingress
	// gateway); the pod is granted NET_BIND_SERVICE, so it binds unprivileged.
	DefaultEdgeHTTPPort = 80

	// DefaultEdgeHTTPSPort is the port the edge TLS-terminating listener binds
	// when downstream TLS is enabled.
	DefaultEdgeHTTPSPort = 443

	// DefaultEdgeReadinessPort is the (unprivileged, internal) port the dedicated
	// readiness listener binds. The kubelet readiness probe targets it over plain
	// HTTP; it is never exposed via a Service.
	DefaultEdgeReadinessPort = 15021

	// defaultEdgeAddress binds the edge listener on all interfaces (it fronts
	// external traffic, unlike the node proxy's loopback-only outbound listener).
	defaultEdgeAddress = "0.0.0.0"
)

// EdgeGatewayListenerName returns the UNIQUE name for a per-Gateway edge listener.
// Naming scheme: edge_gw_<ns>_<gwname>_<port> — distinct per (Gateway, port).
// This guarantees no LDS collision across Gateways (regression guard for #332,
// where all listeners shared "edge_http" and the :443 listener was silently dropped).
func EdgeGatewayListenerName(namespace, gatewayName string, internalPort uint32) string {
	return fmt.Sprintf("edge_gw_%s_%s_%d", namespace, gatewayName, internalPort)
}

// EdgeGatewayRouteName returns the RDS route configuration name for a per-Gateway
// edge listener. Scheme: edge_rt_<ns>_<gwname>. Each Gateway gets an isolated
// route table so route leakage between Gateways is impossible.
func EdgeGatewayRouteName(namespace, gatewayName string) string {
	return fmt.Sprintf("edge_rt_%s_%s", namespace, gatewayName)
}

// EdgeGatewayListener is the inputs for building one per-Gateway edge listener
// (proposal 021 Phase 2). The listener binds on InternalPort (the container/pod
// port the per-Gateway LoadBalancer Service's targetPort points at) and serves
// only the routes for this Gateway.
type EdgeGatewayListener struct {
	// Namespace is the Gateway's namespace.
	Namespace string
	// GatewayName is the Gateway's name.
	GatewayName string
	// InternalPort is the container port this listener binds. It is uniquely
	// allocated per (Gateway, listener section) by the port allocator.
	InternalPort uint32
	// TLSSecretNames are the SDS cert names to present for downstream TLS on this
	// listener (empty = plain HTTP). Cert bytes are already in the snapshot Secrets.
	TLSSecretNames []string
	// HTTPRedirect when true emits an HTTP→HTTPS 301 redirect (replaces the route
	// listener for this Gateway's HTTP port).
	HTTPRedirect bool
}

// BuildEdgeGatewayHTTPListener builds a per-Gateway plain-HTTP or HTTP→HTTPS
// redirect listener. Listener name and RDS route config name are unique per
// Gateway (proposal 021 Phase 2). When httpRedirect is true, the listener emits
// a 301 redirect to HTTPS (no RDS reference); otherwise it serves routes via RDS.
func BuildEdgeGatewayHTTPListener(namespace, gatewayName string, internalPort uint32, httpRedirect bool) *listenerv3.Listener {
	name := EdgeGatewayListenerName(namespace, gatewayName, internalPort)
	if httpRedirect {
		// Redirect listener: inline route config (no RDS), no separate route config
		// resource needed. Re-uses the redirect route logic but names the HCM and
		// vhost after the Gateway to keep stats distinct.
		hcm := buildHTTPConnectionManager(name, ReporterSource, "", "", buildEdgeRedirectRouteConfig())
		hcm.GetRouteConfig().Name = name // unique inline config name avoids any residual collision
		filterChain := &listenerv3.FilterChain{
			Name:    name,
			Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
		}
		return edgeListener(name, internalPort, filterChain)
	}
	// Plain HTTP routing listener: RDS-driven, per-Gateway route config.
	routeName := EdgeGatewayRouteName(namespace, gatewayName)
	hcm := buildHTTPConnectionManager(name, ReporterSource, "", "", nil)
	hcm.HttpFilters = append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter()}, hcm.HttpFilters...)
	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: routeName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}
	filterChain := &listenerv3.FilterChain{
		Name:    name,
		Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
	}
	return edgeListener(name, internalPort, filterChain)
}

// BuildEdgeGatewayHTTPSListener builds a per-Gateway TLS-terminating HTTPS listener.
// It terminates downstream TLS using the named SDS cert(s) and routes via the
// per-Gateway RDS route config. The listener name is unique per (Gateway, port).
func BuildEdgeGatewayHTTPSListener(namespace, gatewayName string, internalPort uint32, tlsSecretNames []string) *listenerv3.Listener {
	name := EdgeGatewayListenerName(namespace, gatewayName, internalPort)
	routeName := EdgeGatewayRouteName(namespace, gatewayName)
	hcm := buildHTTPConnectionManager(name, ReporterSource, "", "", nil)
	hcm.HttpFilters = append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter()}, hcm.HttpFilters...)
	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: routeName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}
	filterChain := &listenerv3.FilterChain{
		Name:    name,
		Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
	}
	if len(tlsSecretNames) > 0 {
		filterChain.TransportSocket = edgeDownstreamTLSFromSDS(tlsSecretNames)
	}
	return edgeListener(name, internalPort, filterChain)
}

// BuildEdgeGatewayRouteConfiguration builds the per-Gateway route table from the
// given virtual hosts. It has a catch-all 404 (no ODCDS — the edge serves only
// explicitly routed services). The route config name is unique per Gateway.
func BuildEdgeGatewayRouteConfiguration(namespace, gatewayName string, vhosts []*routev3.VirtualHost) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         EdgeGatewayRouteName(namespace, gatewayName),
		VirtualHosts: appendEdgeCatchAll404(vhosts),
	}
}

// appendEdgeCatchAll404 ensures the route table has exactly ONE wildcard "*" vhost
// ending in a 404 for unmatched paths (Envoy permits only a single "*" domain per
// route config). If a "*" vhost already exists — the catch-all built from
// hostname-less HTTPRoutes (see buildEdgeVhostsLocked) — the 404 is appended to it
// as the last (lowest-priority) route; otherwise a dedicated edge_not_found "*"
// vhost is added. An unmatched authority thus gets an immediate 404.
func appendEdgeCatchAll404(vhosts []*routev3.VirtualHost) []*routev3.VirtualHost {
	notFound := &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 404}},
	}
	all := append([]*routev3.VirtualHost{}, vhosts...)
	for i, vh := range all {
		if len(vh.GetDomains()) == 1 && vh.GetDomains()[0] == "*" {
			// Append the 404 to a COPY of the "*" vhost — never mutate the input vhost
			// (it may be shared/reused across snapshot builds, which would accumulate
			// duplicate 404 routes).
			all[i] = &routev3.VirtualHost{
				Name:    vh.GetName(),
				Domains: vh.GetDomains(),
				Routes:  append(append([]*routev3.Route{}, vh.GetRoutes()...), notFound),
			}
			return all
		}
	}
	return append(all, &routev3.VirtualHost{
		Name:    "edge_not_found",
		Domains: []string{"*"},
		Routes:  []*routev3.Route{notFound},
	})
}

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
func BuildEdgeListener(name string, port uint32, tlsSecretNames []string) *listenerv3.Listener {
	hcm := buildHTTPConnectionManager(name, ReporterSource, "", "", nil)
	hcm.HttpFilters = append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter()}, hcm.HttpFilters...)
	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			// Both the HTTP and HTTPS listeners serve the same edge route table.
			RouteConfigName: EdgeHTTPRouteName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	filterChain := &listenerv3.FilterChain{
		Name:    name,
		Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
	}
	if len(tlsSecretNames) > 0 {
		filterChain.TransportSocket = edgeDownstreamTLSFromSDS(tlsSecretNames)
	}

	return edgeListener(name, port, filterChain)
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

// BuildEdgeReadinessListener builds the dedicated, always-bound plain-HTTP
// readiness listener. It carries the drain-aware readiness health_check filter
// (answers /aether/readyz with 200/503 from the server's drain state) ahead of a
// catch-all that 404s every other path — this listener exists ONLY to give the
// kubelet readiness probe a stable target that does not depend on which public
// listeners are bound (the public listeners move to per-Gateway internal ports
// under proposal 021 Phase 2, so probing :443 there fails).
func BuildEdgeReadinessListener(port uint32) *listenerv3.Listener {
	hcm := buildHTTPConnectionManager(EdgeReadinessListenerName, ReporterSource, "", "", buildEdgeReadinessRouteConfig())
	hcm.HttpFilters = append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter()}, hcm.HttpFilters...)
	filterChain := &listenerv3.FilterChain{
		Name:    EdgeReadinessListenerName,
		Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
	}
	return edgeListener(EdgeReadinessListenerName, port, filterChain)
}

// buildEdgeReadinessRouteConfig is the inline route table for the readiness
// listener: a single catch-all that direct-responds 404. The readiness
// health_check filter intercepts /aether/readyz before the router, so this only
// covers stray non-readyz requests.
func buildEdgeReadinessRouteConfig() *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name: EdgeReadinessListenerName,
		VirtualHosts: []*routev3.VirtualHost{
			{
				Name:    EdgeReadinessListenerName,
				Domains: []string{"*"},
				Routes: []*routev3.Route{
					{
						Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
						Action: &routev3.Route_DirectResponse{
							DirectResponse: &routev3.DirectResponseAction{Status: 404},
						},
					},
				},
			},
		},
	}
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

// WeightedRouteBackend is one entry in a weighted_clusters route action. Cluster
// is the resolved Envoy cluster name; Weight is the Gateway API backendRef weight
// (default 1; 0 = receives no traffic but is a valid backend per spec).
type WeightedRouteBackend struct {
	Cluster string
	Weight  uint32
}

// BuildEdgeRouteWeighted builds one Envoy route for the edge with a
// weighted_clusters action when there are multiple backends, or a plain
// single-cluster action when there is exactly one (keeping the common path clean).
//
// Gateway API weight semantics:
//   - total_weight = sum of all entry weights; each entry's share = weight/total.
//   - Weight 0 = the backend receives no traffic (remains valid per spec).
//   - If all backends have weight 0 (total_weight = 0), Envoy treats this as
//     "no healthy backend" and returns 500 — this is the expected behavior per
//     the Gateway API spec for that edge case; we do not special-case it.
//
// redirect is applied ahead of backend resolution (no backends needed for a
// redirect). urlRewrite is applied on forwarding routes only.
func BuildEdgeRouteWeighted(prefix, exact string, backends []WeightedRouteBackend, mutation *GammaHeaderMutation, redirect *GammaRedirect, urlRewrite *GammaURLRewrite) *routev3.Route {
	if redirect != nil || len(backends) <= 1 {
		// Redirect path or single backend: delegate to the plain builder.
		cluster := ""
		if len(backends) == 1 {
			cluster = backends[0].Cluster
		}
		return BuildEdgeRoute(prefix, exact, cluster, mutation, redirect, urlRewrite)
	}
	// Multiple backends: weighted_clusters action.
	match := &routev3.RouteMatch{}
	if exact != "" {
		match.PathSpecifier = &routev3.RouteMatch_Path{Path: exact}
	} else {
		match.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: prefix}
	}
	reqAdd, respAdd, reqRemove, respRemove := headerMutationFields(mutation)

	var totalWeight uint32
	clusters := make([]*routev3.WeightedCluster_ClusterWeight, 0, len(backends))
	for _, b := range backends {
		totalWeight += b.Weight
		clusters = append(clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   b.Cluster,
			Weight: wrapperspb.UInt32(b.Weight),
		})
	}
	wc := &routev3.WeightedCluster{
		Clusters:    clusters,
		TotalWeight: wrapperspb.UInt32(totalWeight),
	}

	ra := &routev3.RouteAction{
		ClusterSpecifier: &routev3.RouteAction_WeightedClusters{WeightedClusters: wc},
		RetryPolicy:      outboundRetryPolicy(),
	}
	applyURLRewrite(ra, urlRewrite)

	return &routev3.Route{
		Match:                   match,
		RequestHeadersToAdd:     reqAdd,
		RequestHeadersToRemove:  reqRemove,
		ResponseHeadersToAdd:    respAdd,
		ResponseHeadersToRemove: respRemove,
		Action:                  &routev3.Route_Route{Route: ra},
	}
}

// BuildEdgeRoute builds one Envoy route for the edge. Exactly one of prefix/exact
// is non-empty (exact takes precedence if both are set). Prefix "/" matches all.
// mutation applies RequestHeaderModifier / ResponseHeaderModifier at the route level.
//
// When redirect is non-nil the route emits a Route_Redirect (no cluster). When
// urlRewrite is non-nil the route rewrite fields are applied before forwarding to
// cluster. Both nil → plain forwarding route.
func BuildEdgeRoute(prefix, exact, cluster string, mutation *GammaHeaderMutation, redirect *GammaRedirect, urlRewrite *GammaURLRewrite) *routev3.Route {
	match := &routev3.RouteMatch{}
	if exact != "" {
		match.PathSpecifier = &routev3.RouteMatch_Path{Path: exact}
	} else {
		match.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: prefix}
	}
	reqAdd, respAdd, reqRemove, respRemove := headerMutationFields(mutation)
	r := &routev3.Route{
		Match:                   match,
		RequestHeadersToAdd:     reqAdd,
		RequestHeadersToRemove:  reqRemove,
		ResponseHeadersToAdd:    respAdd,
		ResponseHeadersToRemove: respRemove,
	}
	if redirect != nil {
		r.Action = gammaRedirectAction(redirect)
	} else {
		ra := &routev3.RouteAction{
			ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster},
			RetryPolicy:      outboundRetryPolicy(),
		}
		applyURLRewrite(ra, urlRewrite)
		r.Action = &routev3.Route_Route{Route: ra}
	}
	return r
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
	return &routev3.RouteConfiguration{
		Name:         EdgeHTTPRouteName,
		VirtualHosts: appendEdgeCatchAll404(vhosts),
	}
}

// EdgeK8sClusterName returns the data-plane name for a cleartext k8s-Service
// cluster that reaches a non-mesh (non-registry) HTTPRoute backend. The name is
// unique per (namespace, service, port) and DNS-safe.
// Scheme: edge_k8s_<namespace>_<service>_<port>
func EdgeK8sClusterName(namespace, service string, port uint32) string {
	return fmt.Sprintf("edge_k8s_%s_%s_%d", namespace, service, port)
}

// BuildEdgeK8sCluster builds a STRICT_DNS cleartext cluster that reaches a
// non-mesh HTTPRoute backend via the k8s Service FQDN. It has NO transport socket
// (cleartext — the backend is a plain k8s Service, not a mesh endpoint). HTTP/1.1
// upstream (no h2 options), STRICT_DNS so CoreDNS resolves the FQDN on demand.
// fqdn must be the full cluster-local FQDN (e.g. "svc.ns.svc.cluster.local").
func BuildEdgeK8sCluster(name, fqdn string, port uint32) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                          name,
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ConnectTimeout:                durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STRICT_DNS,
		},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{
					LbEndpoints: []*endpointv3.LbEndpoint{
						{
							HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
								Endpoint: &endpointv3.Endpoint{
									Address: &corev3.Address{
										Address: &corev3.Address_SocketAddress{
											SocketAddress: &corev3.SocketAddress{
												Protocol: corev3.SocketAddress_TCP,
												Address:  fqdn,
												PortSpecifier: &corev3.SocketAddress_PortValue{
													PortValue: port,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		// No TransportSocket: cleartext connection to the k8s Service (non-mesh backend).
		// No TypedExtensionProtocolOptions: plain HTTP/1.1 upstream.
	}
}
