package proxy

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	meshconst "aethermesh.dev/common/constants/mesh"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	matcher_cluster_specifierv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/router/cluster_specifiers/matcher/v3"

	"aethermesh.dev/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	previoushostsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/host/previous_hosts/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// OutboundHTTPRouteName is the name of the outbound HTTP route configuration
	OutboundHTTPRouteName = "out_http"

	// OutboundRouteInitialFetchTimeout bounds how long the egress listener stays
	// WARMING for the first out_http RDS delivery before Envoy gives up waiting
	// and activates it with an unresolved route table (issue #817).
	//
	// 15s is deliberate on both sides:
	//
	//   - It is Envoy's own default for ConfigSource.initial_fetch_timeout, so
	//     stating it changes no behaviour — it pins the value where a test can
	//     see it instead of leaving it to an upstream default that could drift.
	//   - It equals the agent's registryReadyTimeout (the budget PreListen
	//     spends trying to build a registry-backed initial snapshot before it
	//     falls back to local-only). A shorter warming budget would let a fresh
	//     Envoy epoch activate the egress listener while the agent that feeds it
	//     is still inside its own initial-snapshot budget — reopening exactly the
	//     404 NR window this constant exists for.
	//
	// Longer is not free: on a COLD proxy start there is no previous epoch
	// holding the listen socket, so extra warming is extra time with no egress
	// listener at all, and listeners_warming holds the proxy un-Ready for it.
	OutboundRouteInitialFetchTimeout = 15 * time.Second
)

// outboundRetryPolicy returns the retry policy applied to every client-side
// route to a mesh service. It masks the sub-second windows inherent to
// endpoint churn — a dial racing a pod that just received SIGTERM (connection
// refused before the EDS removal propagates) or a 503 while a replacement
// endpoint finishes its first health-check round — by retrying on a
// *different* host (previous_hosts predicate).
//
// What is retried:
//
//   - connect-failure, refused-stream and reset-before-request: THIS proxy
//     never sent the request. Any method.
//   - retriable-headers: a response whose outcome header, stamped by the
//     destination proxy, says it is safe (outcomeRetriableRegex): the
//     application answered 503 itself; the destination never began sending
//     the request to its application; it had begun and the method is
//     idempotent; or the application refused the stream. That covers a gRPC
//     request too, whose failures come back as HTTP 200.
//
// What is not: everything else, and in particular a request with a
// non-idempotent method that the destination had already begun sending to its
// application when the connection closed or reset. The application may have
// run it (aether#1641).
//
// There is deliberately NO status-code condition (retriable-status-codes, 5xx,
// gateway-error) and no reset condition. Retry conditions are OR-ed: with any
// of them back in the list, the 503 of a begun POST is retried again whatever
// its outcome header says. The price is that a response WITHOUT the header is
// never retried: a destination proxy older than the header, for the length of
// an agent upgrade (docs/runbook.md, "the request had begun").
//
// A route that carries this policy also removes the outcome header from the
// response (stripOutcomeHeader), so that it never reaches the client
// application.
func outboundRetryPolicy() *routev3.RetryPolicy {
	rp := baseRetryPolicy()
	rp.RetryOn = "connect-failure,refused-stream,reset-before-request,retriable-headers"
	rp.RetriableHeaders = []*routev3.HeaderMatcher{outcomeRetriableHeader()}
	return rp
}

// nonMeshRetryPolicy is the retry policy of an edge route whose backends are
// all cleartext Kubernetes Services (EdgeK8sClusterName). No destination proxy
// stands in front of such a backend, so nothing stamps an outcome header and
// the header rule would retry nothing. It is also not needed there: the edge
// is the last proxy before the application, so a 503 RESPONSE it receives was
// written by the application itself, and a connection the application drops
// mid-request is a reset, which this policy does not retry.
//
// It must never be used on a route that can reach a mesh service.
func nonMeshRetryPolicy() *routev3.RetryPolicy {
	rp := baseRetryPolicy()
	rp.RetryOn = "connect-failure,refused-stream,reset-before-request,retriable-status-codes"
	rp.RetriableStatusCodes = []uint32{503}
	return rp
}

// baseRetryPolicy is what the two policies share: how often, where and how
// fast, but not on what.
func baseRetryPolicy() *routev3.RetryPolicy {
	return &routev3.RetryPolicy{
		NumRetries: wrapperspb.UInt32(2),
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

// MeshAuthorityRegex is the RE2 pattern of a mesh authority,
// <svc>.<ns>.<meshDomain> (two labels before the mesh domain, 020 Part 1),
// followed by portSuffix: a pattern for what may follow the name, e.g.
// "(:[0-9]+)?" for any port or none, ":8080" for exactly that port.
func MeshAuthorityRegex(meshDomain, portSuffix string) string {
	return "^[a-z0-9-]+\\.[a-z0-9-]+\\." + regexp.QuoteMeta(meshDomain) + portSuffix + "$"
}

// buildOnDemandCatchAllVirtualHost builds the lowest-priority virtual host
// (domain "*") of a route table. With the authority port retained
// (strip_any_host_port off, for FQDN:port routing), a scoped catch-all
// "*.<meshDomain>" can't match host:port, so the catch-all is universal and
// splits by an :authority regex: an authority matching onDemandAuthorityRegex
// routes to the cluster named by the authority -- the on_demand filter fetches
// it via ODCDS (proposal 004 cold path); anything else falls through. This
// keeps instant foreign-404 determinism (spike-verified, Envoy 1.38) while
// supporting FQDN:port (proposal 005). Nonexistent in-domain services/ports
// fail when the ODCDS timeout expires.
//
// onDemandAuthorityRegex decides which authorities may NAME a cluster. The
// outbound (loopback) table accepts any mesh authority, with or without a
// port, because there the Host header is the only address there is. A capture
// table for port P accepts only "<name>:P" (and the legacy tables the
// port-less form): the client dialled P, so no authority may select a cluster
// of another port (proposal 043, "The addressing rule"; #1682 was a route here
// that accepted any port and sent it to the default cluster). Empty means no
// on-demand route at all.
//
// The final fallthrough for an authority nothing claimed is a hard 404, or,
// with passthrough (redirect-all capture, proposal 022), the ORIGINAL_DST
// passthrough: ALL of the pod's egress is intercepted there, including
// requests to Kubernetes Services that are not in the mesh, and a 404 would
// break plain Service-to-Service reachability. A name that IS a mesh service
// never reaches the fallthrough: every table carries a virtual host for it,
// one that routes or one that refuses.
func buildOnDemandCatchAllVirtualHost(onDemandAuthorityRegex string, passthrough bool) *routev3.VirtualHost {
	fallthrough_ := &routev3.Route{
		Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_DirectResponse{
			DirectResponse: &routev3.DirectResponseAction{Status: 404},
		},
	}
	if passthrough {
		fallthrough_ = &routev3.Route{
			Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_Route{
				Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: PassthroughClusterName},
				},
			},
		}
	}
	routes := []*routev3.Route{
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
	}
	if onDemandAuthorityRegex != "" {
		routes = append(routes, stripOutcomeHeader(&routev3.Route{
			Match: &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
				Headers: []*routev3.HeaderMatcher{
					{
						Name: ":authority",
						HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
							StringMatch: &matcherv3.StringMatcher{
								MatchPattern: &matcherv3.StringMatcher_SafeRegex{
									SafeRegex: &matcherv3.RegexMatcher{Regex: onDemandAuthorityRegex},
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
		}))
	}
	routes = append(routes, fallthrough_)
	return &routev3.VirtualHost{
		Name:    "on_demand_catch_all",
		Domains: []string{"*"},
		Routes:  routes,
	}
}

// RefusalReservedPortBody is the body of the refusal of a mesh authority that
// names one of the mesh's own two ports.
const RefusalReservedPortBody = "aether: ports 18081 and 18082 are the mesh's own (its HTTP proxy port and its L4 port) and are a port of no service. " +
	"Put the service's own port in the Host: <service>.<namespace>.<mesh domain>:<port>.\n"

// refuseReservedPorts makes the catch-all of a table whose on-demand route
// takes the port from the authority (the loopback table and the proxy port's
// capture table) refuse a mesh authority that names 18081 or 18082, before
// that route: 421 port-not-declared, at once.
//
// Those two are ports of no service, so "<fqdn>:18081" names a cluster that
// can never exist. Left to the on-demand route it was parked while that
// cluster was asked for, and through chart 2.6 an alias cluster of that name
// answered: the request reached the port the pods declare. A service the node
// knows refuses the spelling in its own virtual host; this is the same answer
// for a name the node knows nothing of, so the spelling is never parked,
// whatever enters the node's scope meanwhile.
func refuseReservedPorts(catchAll *routev3.VirtualHost, meshDomain string) *routev3.VirtualHost {
	reserved := fmt.Sprintf(":(%d|%d)", meshconst.ProxyOutboundPort, meshconst.ProxyL4OutboundPort)
	refusal := BuildRefusalVirtualHost("", nil, RefusalPortNotDeclared, RefusalReservedPortBody).GetRoutes()[0]
	refusal.Match.Headers = []*routev3.HeaderMatcher{{
		Name: ":authority",
		HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{
			MatchPattern: &matcherv3.StringMatcher_SafeRegex{SafeRegex: &matcherv3.RegexMatcher{Regex: MeshAuthorityRegex(meshDomain, reserved)}},
		}},
	}}
	// After the liveness route (an exact path, whatever the authority),
	// before the on-demand route.
	routes := make([]*routev3.Route, 0, len(catchAll.GetRoutes())+1)
	routes = append(routes, catchAll.GetRoutes()[0], refusal)
	catchAll.Routes = append(routes, catchAll.GetRoutes()[1:]...)
	return catchAll
}

// BuildOnDemandServiceVirtualHost builds the loopback table's virtual host of
// ONE mesh service the node holds no cluster of: its authorities, routed like
// the catch-all's on-demand route (the cluster the authority names, fetched by
// the on-demand filter).
//
// It exists so that the service has a virtual host of its OWN before its
// cluster arrives. What a service-wide HTTP filter does is enabled by the
// service's virtual host; a request the catch-all parked resumes on the
// catch-all's route and was delivered with that filter skipped. The caller
// applies the filter to what this returns.
//
// rules are the service's route rules that apply to these authorities. Their
// routes come first, as on the service's real virtual host, so a request that
// was parked is matched, filtered and sent to the rule's backends exactly as
// one that arrives later; a rule with no backendRef routes to the cluster the
// authority names.
func BuildOnDemandServiceVirtualHost(name string, domains []string, rules []GammaRoute) *routev3.VirtualHost {
	return buildRuleVirtualHost(name, "", domains, rules)
}

// BuildCaptureCatchAllVirtualHost builds the "*" virtual host of ONE capture
// route table (proposal 043).
//
//   - port is the table's dialled port, 0 for cap_http_other. A mesh authority
//     that spells exactly that port may still name its cluster on demand (the
//     cold path of a service this node holds no endpoint of yet).
//   - portless additionally lets a port-less mesh authority name its cluster:
//     the two legacy tables (the mesh HTTP port, and port 80), where a
//     port-less name is the legacy spelling of the service's default cluster.
//   - redirectAll selects the fallthrough: the ORIGINAL_DST passthrough, or a
//     hard 404 in scoped capture.
//
// cap_http_other gets no on-demand route: no cluster can be named by a port
// number that no service declares as HTTP.
func BuildCaptureCatchAllVirtualHost(meshDomain string, port uint32, portless, redirectAll bool) *routev3.VirtualHost {
	regex := ""
	if port != 0 {
		suffix := fmt.Sprintf(":%d", port)
		if portless {
			suffix = "(" + suffix + ")?"
		}
		regex = MeshAuthorityRegex(meshDomain, suffix)
	}
	return buildOnDemandCatchAllVirtualHost(regex, redirectAll)
}

// BuildProxyPortCaptureRouteConfiguration builds the route table of the
// capture listener's chain for the mesh's HTTP proxy port (a captured dial to
// "<anything>:18081"): the LOOPBACK table's virtual hosts under the capture
// table's name. 18081 is not a port of any service, so nothing was dialled
// that could decide: as on the loopback listener, the port in Host is the
// port, and a Host that names 18081 itself is a port the service does not
// declare. This is how a pod that is not on redirect-all, which has only the
// mesh ports diverted, reaches a service by name.
//
// The floor differs from the loopback table's in one way: with passthrough
// (redirect-all), an authority that is not a mesh name leaves through the
// ORIGINAL_DST passthrough, as on every other capture table, instead of 404:
// all of the pod's egress is captured, and something else may listen on 18081.
func BuildProxyPortCaptureRouteConfiguration(name string, vhosts []*routev3.VirtualHost, meshDomain string, passthrough bool) *routev3.RouteConfiguration {
	withCatchAll := make([]*routev3.VirtualHost, 0, len(vhosts)+1)
	withCatchAll = append(withCatchAll, vhosts...)
	withCatchAll = append(withCatchAll, refuseReservedPorts(buildOnDemandCatchAllVirtualHost(MeshAuthorityRegex(meshDomain, ":[0-9]+"), passthrough), meshDomain))
	return &routev3.RouteConfiguration{Name: name, VirtualHosts: withCatchAll}
}

// BuildOutboundRouteConfiguration creates a route configuration for outbound traffic.
// It includes the provided virtual hosts plus the universal on-demand catch-all
// virtual host (mesh-shaped authorities → ODCDS, everything else → 404). The outbound
// listener never passes through (it only sees mesh-DNS authorities), so the catch-all
// keeps its hard-404 fallthrough.
//
// The catch-all is the FLOOR of this table, not a decoration on a non-empty one:
// vhosts may legitimately be empty (an agent that started local-only and has no
// registry-derived service vhosts yet) and the result is still a complete,
// resolvable out_http — liveness 200 on MeshLivePath, mesh-shaped authorities to
// ODCDS, everything else an instant 404. Callers must therefore emit this
// UNCONDITIONALLY; see the RDS-resolution note in the agent snapshot builder
// (issue #817).
//
// The result owns its virtual_hosts slice. Appending the catch-all straight onto
// the caller's slice would write it into that slice's spare capacity, handing a
// caller that reuses the backing array a table with someone else's catch-all in
// it.
func BuildOutboundRouteConfiguration(vhosts []*routev3.VirtualHost, meshDomain string) *routev3.RouteConfiguration {
	all := make([]*routev3.VirtualHost, 0, len(vhosts)+1)
	all = append(all, vhosts...)
	// Only an authority WITH a port names a cluster on demand: the cluster
	// is the one the authority spells, and "<fqdn>:<port>" is a port's
	// cluster. A port-less mesh authority would spell the bare cluster name,
	// which is no port's (it is the internal name of the cluster of the port
	// the pods declare): a name without a port is port 80, and a service the
	// node knows gets a virtual host that says so. Unknown and port-less
	// falls through.
	all = append(all, refuseReservedPorts(buildOnDemandCatchAllVirtualHost(MeshAuthorityRegex(meshDomain, ":[0-9]+"), false), meshDomain))
	return &routev3.RouteConfiguration{
		Name:         OutboundHTTPRouteName,
		VirtualHosts: all,
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
			stripOutcomeHeader(&routev3.Route{
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
			}),
		},
	}
}

// GAMMA east-west L7 routing (proposal 018, Phase 2). A GammaRoute is one resolved
// HTTPRoute rule: ordered matches forwarding to one or more weighted backend
// clusters, with an optional timeout, header mutations, redirect, and URL rewrite.
// Backends are already resolved to data-plane cluster names (<backend>.<meshDomain>)
// by the reconciler.
type GammaRoute struct {
	// DroppedBackends reports that the rule had backendRefs the projection
	// dropped: another kind than a core Service, or a Service in another
	// namespace that no ReferenceGrant permits. They are invalid backendRefs
	// and the Gateway API gives them the same answer as any other, 500 for
	// their share of the rule's weight (DroppedBackendWeight; weight 0 takes
	// nothing). Until chart 2.7.0 they were dropped silently: their share went
	// to the rule's other backends, and a rule left with none routed to its
	// parent Service. Set by the GAMMA reconciler, not by a projector.
	DroppedBackends      bool
	DroppedBackendWeight uint32
	// ParentPort is the Service port the rule's parentRef names, or 0 when the
	// parentRef names none (proposal 043). A rule without a parent port
	// applies on every HTTP port of the parent Service; with one, on that
	// port only. It is the PARENT SERVICE's port: for a Service the registrar
	// generated that is the pod's port, for a user Service it is resolved
	// through the Service's port map before the rule is placed.
	ParentPort uint32
	// Imported marks a rule that came from a peer cluster's projection
	// (proposal 026) and not from a route of this cluster. Such a rule carries
	// no parent port and no backend ports (the projection has no field for
	// them yet), so it is kept on the virtual hosts of the service's legacy
	// target and never spread over the service's other ports. Set by the
	// cache when it takes the imported rules in.
	Imported       bool
	Matches        []GammaMatch
	Backends       []GammaBackend
	Timeout        *durationpb.Duration
	HeaderMutation *GammaHeaderMutation
	// Redirect, when non-nil, makes this route return a redirect response instead
	// of forwarding to a backend. Coexists with Matches; takes precedence over
	// Backends per the Gateway API spec.
	Redirect *GammaRedirect
	// URLRewrite, when non-nil, rewrites the request URL (host and/or path) before
	// forwarding. Applied on top of the normal RouteAction (Backends still used).
	URLRewrite *GammaURLRewrite
	// ExtensionFilters are proxy-extension escape-hatch filters (proposal 025)
	// attached to this route via an HTTPRoute/GRPCRoute `ExtensionRef` → HTTPFilter
	// CRD. Each is emitted into the route's typed_per_filter_config (enabling +
	// configuring an allow-listed Envoy HTTP filter that aether adds to the HCM chain
	// default-disabled). Empty for the common case.
	ExtensionFilters []ExtensionFilter
}

// ExtensionFilter is one proxy-extension escape-hatch filter (proposal 025): a named
// Envoy HTTP filter configured opaquely via an HTTPFilter CRD. Name is the Envoy
// filter name (allow-listed — the proxy build must compile it in); Config is the
// opaque per-route typed_config (validated by the admission webhook, M2). aether adds
// the filter to the HCM chain default-disabled; the route's typed_per_filter_config
// (Config) re-enables + configures it for that route.
type ExtensionFilter struct {
	Name   string
	Config *anypb.Any
}

// GammaRedirect describes a RequestRedirect filter: it replaces the route action
// with an Envoy RedirectAction instead of forwarding to a backend cluster.
type GammaRedirect struct {
	// Scheme is the target scheme ("http" or "https"). Empty = unchanged.
	Scheme string
	// Hostname is the target hostname. Empty = unchanged.
	Hostname string
	// Port is the target port (0 = unchanged / preserve original).
	Port uint32
	// StatusCode is the HTTP redirect code (301 or 302; default 301).
	StatusCode int
	// PathType selects how to rewrite the path (empty = no path rewrite).
	// "ReplaceFullPath" → Envoy PathRedirect; "ReplacePrefixMatch" → PrefixRewrite.
	PathType  string
	PathValue string
	// ListenerPort is the external port of the Gateway listener serving this route
	// (e.g. 8080 when the Gateway declares port: 8080 and the LB Service maps
	// 8080 → some internal port). It is set by the per-Gateway route builder and
	// ONLY used when Port == 0 and Scheme == "" (i.e. the redirect preserves the
	// original port). Without this, Envoy's RedirectAction with port_redirect=0
	// emits the listener's bound (internal) port in the Location header — not the
	// external port the client connected to. For default HTTP ports (80, 443) this
	// field is left 0 so the port is omitted from Location (correct for default
	// ports). For non-default ports it is set to ExternalPort so Envoy encodes the
	// correct port in Location.
	ListenerPort uint32
}

// GammaURLRewrite describes a URLRewrite filter: rewrites the request host
// and/or path before forwarding to the backend. Applied within the RouteAction.
type GammaURLRewrite struct {
	// Hostname is the Host header to rewrite to. Empty = unchanged.
	Hostname string
	// PathType selects how to rewrite the path (empty = no path rewrite).
	// "ReplaceFullPath" → Envoy RegexRewrite (.*) → value.
	// "ReplacePrefixMatch" → Envoy PrefixRewrite.
	PathType  string
	PathValue string
}

// GammaMatch is one HTTPRoute match: a path (prefix OR exact OR regex; empty =
// prefix "/") and zero or more exact header matches (ANDed). Exactly one of
// Prefix, Exact, or Regex should be set; Exact takes precedence over Prefix, and
// Regex is only used when neither Prefix nor Exact is set.
type GammaMatch struct {
	Prefix string
	Exact  string
	// Regex is an RE2 safe_regex path match, used for gRPC RegularExpression method
	// matches: the reconciler builds a /<serviceRegex>/<methodRegex> pattern and
	// stores it here. Empty means no regex path match.
	Regex   string
	Headers []GammaHeaderMatch
}

// GammaHeaderMatch is an exact request-header match.
type GammaHeaderMatch struct {
	Name  string
	Value string
}

// GammaBackend is one weighted backend of a route: the resolved data-plane Cluster
// the route forwards to, plus the bare Service name (for the dependency set).
//
// Port is the backendRef's port, a port of the backend SERVICE, or 0 when the
// producer of the rule did not carry one (a rule imported from a peer cluster,
// proposal 026). The cache resolves it through the backend's port map to the
// cluster of that pod port before the route is built; Cluster as projected is
// the backend's default cluster and is what a backend with no usable port
// keeps.
type GammaBackend struct {
	Service string
	Cluster string
	Weight  uint32
	Port    uint32
	// Invalid marks a backendRef that names no HTTP port of its Service (the
	// Service has no such port, or the port is not HTTP). Its share of the
	// rule's traffic is answered 500, as the Gateway API specifies for an
	// invalid backendRef; it is never delivered to another port. Set by the
	// snapshot cache when it resolves the backend's port, not by a projector.
	Invalid bool
}

// GammaHeaderMutation describes the header modifications carried by one
// RequestHeaderModifier or ResponseHeaderModifier filter on a route rule.
// Multiple filters merge (later set/add entries win for set, remove is additive).
type GammaHeaderMutation struct {
	// SetRequest are key/value pairs to set (overwrite) on the request.
	SetRequest []GammaHeaderKV
	// AddRequest are key/value pairs to append to the request.
	AddRequest []GammaHeaderKV
	// RemoveRequest are header names to strip from the request.
	RemoveRequest []string
	// SetResponse are key/value pairs to set (overwrite) on the response.
	SetResponse []GammaHeaderKV
	// AddResponse are key/value pairs to append to the response.
	AddResponse []GammaHeaderKV
	// RemoveResponse are header names to strip from the response.
	RemoveResponse []string
}

// GammaHeaderKV is one header name+value pair for a header mutation action.
type GammaHeaderKV struct {
	Name  string
	Value string
}

// BuildOutboundServiceVirtualHost builds a service's outbound virtual host enriched
// with GAMMA rules: each rule's matches become Envoy routes to its weighted backend
// cluster(s), with the outbound retry policy, an optional timeout, and optional
// header mutations (RequestHeaderModifier / ResponseHeaderModifier filters). A
// trailing catch-all routes anything the rules don't match to the service's own
// cluster (name), so producer rules are additive. With no rules it is the passthrough
// vhost.
//
// Rules with a Redirect field produce a redirect route (Route_Redirect) with no
// backend cluster. Rules with a URLRewrite field apply host/path rewriting on the
// RouteAction before forwarding to the backend.
func BuildOutboundServiceVirtualHost(name string, domains []string, rules []GammaRoute) *routev3.VirtualHost {
	return BuildServiceVirtualHost(name, name, domains, rules)
}

// BuildServiceVirtualHost is BuildOutboundServiceVirtualHost with the virtual
// host's name and its own cluster given separately: name identifies the
// virtual host within its route table, cluster is where a request that no rule
// sends elsewhere goes. A per-port table has several virtual hosts for one
// service (one per port and name family), and a service's port P is served by
// the cluster of that port, not by a cluster named after the virtual host.
func BuildServiceVirtualHost(name, cluster string, domains []string, rules []GammaRoute) *routev3.VirtualHost {
	if len(rules) == 0 {
		vh := BuildOutboundClusterVirtualHost(cluster, domains)
		vh.Name = name
		return vh
	}
	return buildRuleVirtualHost(name, cluster, domains, rules)
}

func buildRuleVirtualHost(vhostName, name string, domains []string, rules []GammaRoute) *routev3.VirtualHost {
	// One route per (rule, match). Gateway API mandates that routes be evaluated by
	// match SPECIFICITY, not document order: a longer/exact path (and tie-broken by
	// header-match count) wins over a shorter one regardless of where it appears in
	// the HTTPRoute. Envoy is first-match on the route list, so we must emit the
	// routes pre-sorted (descending specificity). Without this, an earlier rule's
	// "/" prefix shadows every later, more-specific rule (e.g. "/v2"). The edge path
	// already does this (sortRoutesBySpecificity); GAMMA must match.
	scored := buildScoredRoutes(name, rules)
	// Stable sort by descending specificity: ties preserve document order (Gateway
	// API tie-break after path/method/header is creation order, approximated here).
	slices.SortStableFunc(scored, func(a, b scoredRoute) int {
		if a.key > b.key {
			return -1
		}
		if a.key < b.key {
			return 1
		}
		return 0
	})
	routes := make([]*routev3.Route, 0, len(scored)+1)
	for _, s := range scored {
		routes = append(routes, s.route)
	}
	fallback := &routev3.RouteAction{RetryPolicy: outboundRetryPolicy()}
	setServiceCluster(fallback, name)
	routes = append(routes, stripOutcomeHeader(&routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Route{Route: fallback},
	}))
	return &routev3.VirtualHost{Name: vhostName, Domains: domains, Routes: routes}
}

// scoredRoute pairs an Envoy route with its Gateway API specificity key for sorting.
type scoredRoute struct {
	route *routev3.Route
	key   int64
}

// buildScoredRoutes expands each GammaRule+match into a scoredRoute.
func buildScoredRoutes(name string, rules []GammaRoute) []scoredRoute {
	var scored []scoredRoute
	for _, rule := range rules {
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []GammaMatch{{Prefix: "/"}}
		}
		// Once per rule: every match of the rule shares its backends.
		refusal := invalidBackendRefusal(&rule)
		for _, m := range matches {
			key := gammaMatchSpecificity(m)
			if refused := refusal.route(m); refused != nil {
				scored = append(scored, scoredRoute{route: refused, key: key})
			}
			if !refusal.all {
				scored = append(scored, scoredRoute{route: gammaRuleRoute(name, rule, m), key: key})
			}
		}
	}
	return scored
}

// backendRefusal is what a rule's invalid backendRefs take of it: nothing
// (the zero value), a share of its requests, or all of them.
type backendRefusal struct {
	share *corev3.RuntimeFractionalPercent
	all   bool
}

// invalidBackendRefusal decides the refusal of a rule and leaves the rule with
// its valid backends only. A redirect rule sends nothing to a backend and is
// never refused.
func invalidBackendRefusal(rule *GammaRoute) backendRefusal {
	if rule.Redirect != nil {
		return backendRefusal{}
	}
	valid, share := splitInvalidBackends(rule.Backends, rule.DroppedBackendWeight)
	// No valid backendRef at all, whatever the weights: the rule has nowhere
	// to send a request, and its parent's own cluster is not what it names.
	all := (len(rule.Backends) > 0 || rule.DroppedBackends) && len(valid) == 0
	rule.Backends = valid
	return backendRefusal{share: share, all: all}
}

// route returns the 500 route of one match of the rule, nil when the rule
// refuses nothing. It stands before the rule's own route with the same match
// (the sort is stable) and takes the invalid backendRefs' share of the
// requests, or every request.
func (b backendRefusal) route(m GammaMatch) *routev3.Route {
	if !b.all && b.share == nil {
		return nil
	}
	refused := &routev3.Route{Match: gammaRouteMatch(m), Action: invalidBackendResponse()}
	if !b.all {
		refused.Match.RuntimeFraction = b.share
	}
	return refused
}

// gammaRuleRoute builds the route of one match of a rule: its redirect, or
// the route to its backends (or, with none, to the service's own cluster).
func gammaRuleRoute(name string, rule GammaRoute, m GammaMatch) *routev3.Route {
	reqAdd, respAdd, reqRemove, respRemove := headerMutationFields(rule.HeaderMutation)
	r := &routev3.Route{
		Match:                   gammaRouteMatch(m),
		RequestHeadersToAdd:     reqAdd,
		RequestHeadersToRemove:  reqRemove,
		ResponseHeadersToAdd:    respAdd,
		ResponseHeadersToRemove: respRemove,
		TypedPerFilterConfig:    extensionPerFilterConfig(rule.ExtensionFilters),
	}
	if rule.Redirect != nil {
		r.Action = gammaRedirectAction(rule.Redirect)
		return r
	}
	r.Action = gammaRouteAction(name, rule, m.Prefix)
	// Slices of their own: stripOutcomeHeader appends, and the mutation's
	// slices may be shared with the rule.
	r.ResponseHeadersToRemove = slices.Clone(respRemove)
	r.RequestHeadersToRemove = slices.Clone(reqRemove)
	return stripOutcomeHeader(r)
}

// InvalidBackendBody is the body of the 500 an invalid backendRef answers.
const InvalidBackendBody = "aether: the route's backendRef names no HTTP port of its Service (see the route's ResolvedRefs condition)\n"

// invalidBackendResponse is the answer of an invalid backendRef: 500, as the
// Gateway API specifies, with a body that says where to look.
func invalidBackendResponse() *routev3.Route_DirectResponse {
	return &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
		Status: 500,
		Body:   &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: InvalidBackendBody}},
	}}
}

// splitInvalidBackends separates a rule's backends into the valid ones and
// the share of the rule's weight the invalid ones hold, as the fraction of
// requests that are answered 500. share is nil when no backend is invalid, or
// when the invalid ones hold no weight (a backendRef with weight 0 receives
// nothing, valid or not).
func splitInvalidBackends(backends []GammaBackend, dropped uint32) (valid []GammaBackend, share *corev3.RuntimeFractionalPercent) {
	// dropped is the weight of the backendRefs that never became a backend
	// (GammaRoute.DroppedBackendWeight): invalid, like the ones marked here.
	invalid, total := uint64(dropped), uint64(dropped)
	for _, b := range backends {
		total += uint64(b.Weight)
		if b.Invalid {
			invalid += uint64(b.Weight)
		}
	}
	if invalid == 0 {
		for _, b := range backends {
			if b.Invalid {
				// Weight 0 and invalid: dropped, nothing is sent to it.
				return slices.DeleteFunc(slices.Clone(backends), func(b GammaBackend) bool { return b.Invalid }), nil
			}
		}
		return backends, nil
	}
	valid = slices.DeleteFunc(slices.Clone(backends), func(b GammaBackend) bool { return b.Invalid })
	return valid, &corev3.RuntimeFractionalPercent{DefaultValue: &typev3.FractionalPercent{
		Numerator:   uint32(invalid * 1_000_000 / total),
		Denominator: typev3.FractionalPercent_MILLION,
	}}
}

// setServiceCluster points a route action at a service's own cluster: the
// named one, or, for a virtual host that knows only the service's authorities
// (BuildOnDemandServiceVirtualHost), the cluster the request's authority
// names.
func setServiceCluster(ra *routev3.RouteAction, cluster string) {
	if cluster == "" {
		ra.ClusterSpecifier = &routev3.RouteAction_ClusterHeader{ClusterHeader: onDemandClusterHeader}
		return
	}
	ra.ClusterSpecifier = &routev3.RouteAction_Cluster{Cluster: cluster}
}

// headerMutationFields converts a GammaHeaderMutation into the four Envoy route-level
// header mutation slices. Returns nil slices when mutation is nil or empty (no
// allocations for the common no-filter case).
func headerMutationFields(m *GammaHeaderMutation) (
	reqAdd []*corev3.HeaderValueOption,
	respAdd []*corev3.HeaderValueOption,
	reqRemove []string,
	respRemove []string,
) {
	if m == nil {
		return
	}
	for _, kv := range m.SetRequest {
		reqAdd = append(reqAdd, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: kv.Name, Value: kv.Value},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	for _, kv := range m.AddRequest {
		reqAdd = append(reqAdd, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: kv.Name, Value: kv.Value},
			AppendAction: corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
		})
	}
	reqRemove = append(reqRemove, m.RemoveRequest...)
	for _, kv := range m.SetResponse {
		respAdd = append(respAdd, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: kv.Name, Value: kv.Value},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	for _, kv := range m.AddResponse {
		respAdd = append(respAdd, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: kv.Name, Value: kv.Value},
			AppendAction: corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
		})
	}
	respRemove = append(respRemove, m.RemoveResponse...)
	return
}

func gammaRouteMatch(m GammaMatch) *routev3.RouteMatch {
	rm := &routev3.RouteMatch{}
	switch {
	case m.Exact != "":
		rm.PathSpecifier = &routev3.RouteMatch_Path{Path: m.Exact}
	case m.Regex != "":
		rm.PathSpecifier = &routev3.RouteMatch_SafeRegex{
			SafeRegex: &matcherv3.RegexMatcher{Regex: m.Regex},
		}
	case m.Prefix != "":
		// Gateway API PathPrefix is SEGMENT-BOUNDARY: "/v2" matches "/v2" and
		// "/v2/x" but NOT "/v2example". Envoy's path_separated_prefix implements
		// this; a raw Prefix would mis-match "/v2example". The "/" catch-all is the
		// exception (Envoy rejects path_separated_prefix:"/"), and a trailing slash
		// is normalized away since it is itself only a segment boundary.
		setGammaPrefixPathSpecifier(rm, m.Prefix)
	default:
		rm.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
	}
	for _, h := range m.Headers {
		rm.Headers = append(rm.Headers, &routev3.HeaderMatcher{
			Name: h.Name,
			HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
				StringMatch: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: h.Value},
				},
			},
		})
	}
	return rm
}

// gammaMatchSpecificity scores one GammaMatch for Gateway API precedence ordering
// (higher = more specific, evaluated first). It mirrors the edge's routeSpecificity:
// an Exact path outranks any prefix; among prefixes a longer one outranks a shorter;
// header-match count is the next tie-break. A Regex path (gRPC method match) is
// treated as exact-strength since it pins a full method path.
func gammaMatchSpecificity(m GammaMatch) int64 {
	const exactPathKey = int64(1) << 29 // sentinel above any prefix length
	var pathKey int64
	switch {
	case m.Exact != "" || m.Regex != "":
		pathKey = exactPathKey
	default:
		p := m.Prefix
		if p == "" {
			p = "/"
		}
		pathKey = int64(len(strings.TrimRight(p, "/")))
		if pathKey >= exactPathKey {
			pathKey = exactPathKey - 1
		}
	}
	hdrCount := int64(len(m.Headers))
	if hdrCount > 0xffff {
		hdrCount = 0xffff
	}
	return (pathKey << 16) | hdrCount
}

// setGammaPrefixPathSpecifier sets the PathSpecifier for a Gateway API PathPrefix
// match using segment-boundary semantics (mirrors setEdgePrefixPathSpecifier).
// Envoy's path_separated_prefix matches "/v2" and "/v2/x" but NOT "/v2example";
// a raw RouteMatch_Prefix would wrongly match the latter. "/" stays a plain prefix
// (Envoy rejects path_separated_prefix:"/"), and a trailing slash is trimmed since
// it is itself only a segment boundary that path_separated_prefix already requires.
func setGammaPrefixPathSpecifier(rm *routev3.RouteMatch, prefix string) {
	if prefix == "/" {
		rm.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
		return
	}
	p := strings.TrimRight(prefix, "/")
	if p == "" {
		rm.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
		return
	}
	rm.PathSpecifier = &routev3.RouteMatch_PathSeparatedPrefix{PathSeparatedPrefix: p}
}

func gammaRouteAction(serviceCluster string, rule GammaRoute, matchPrefix string) *routev3.Route_Route {
	ra := &routev3.RouteAction{RetryPolicy: outboundRetryPolicy()}
	if rule.Timeout != nil {
		ra.Timeout = rule.Timeout
	}
	switch len(rule.Backends) {
	case 0:
		setServiceCluster(ra, serviceCluster)
	case 1:
		ra.ClusterSpecifier = &routev3.RouteAction_Cluster{Cluster: rule.Backends[0].Cluster}
	default:
		weighted := &routev3.WeightedCluster{}
		for _, b := range rule.Backends {
			weighted.Clusters = append(weighted.Clusters, &routev3.WeightedCluster_ClusterWeight{
				Name:   b.Cluster,
				Weight: wrapperspb.UInt32(b.Weight),
			})
		}
		ra.ClusterSpecifier = &routev3.RouteAction_WeightedClusters{WeightedClusters: weighted}
	}
	applyURLRewrite(ra, rule.URLRewrite, matchPrefix)
	return &routev3.Route_Route{Route: ra}
}

// gammaRedirectAction converts a GammaRedirect into an Envoy Route_Redirect action.
// The returned action replaces the RouteAction entirely — no backend cluster is used.
//
// Port semantics (Gateway API compliance):
//
//   - Explicit Port set: use it directly (rd.Port != 0).
//
//   - Scheme changed but no explicit Port: use the scheme-default port (443 for https,
//     80 for http). Without this, Envoy's port_redirect=0 would copy the original
//     request port — wrong when changing scheme (e.g. https://host:80/path).
//
//   - No Port, no Scheme (pure "preserve original"): rd.ListenerPort carries the
//     external port the Gateway listener declared (e.g. 8080). Envoy's port_redirect=0
//     emits the listener's BOUND port, which is the internal/container port allocated
//     by aether's port allocator — NOT the external port the client used. For a
//     non-standard port (not 80 or 443) we must set port_redirect explicitly so the
//     Location reflects the external port. For default ports (80/443) we leave
//     port_redirect=0 so the port is omitted from Location (correct per RFC 3986).
func gammaRedirectAction(rd *GammaRedirect) *routev3.Route_Redirect {
	a := &routev3.RedirectAction{}
	if rd.Scheme != "" {
		a.SchemeRewriteSpecifier = &routev3.RedirectAction_SchemeRedirect{SchemeRedirect: rd.Scheme}
	}
	if rd.Hostname != "" {
		a.HostRedirect = rd.Hostname
	}
	switch {
	case rd.Port != 0:
		// Explicit port from the HTTPRoute RequestRedirect filter: use as specified.
		a.PortRedirect = rd.Port
	case rd.Scheme == "https":
		// Scheme changed to https, no explicit port: use the https default (443) so
		// the original request port is not preserved in the redirect URL.
		a.PortRedirect = 443
	case rd.Scheme == "http":
		// Scheme changed to http, no explicit port: use the http default (80).
		a.PortRedirect = 80
	case rd.ListenerPort != 0 && rd.ListenerPort != 80 && rd.ListenerPort != 443:
		// No explicit port, no scheme change, non-standard listener port: set
		// port_redirect to the external port so Location carries the right port.
		// Envoy's port_redirect=0 would use the listener's bound (internal) port
		// instead. For default ports (80/443) we leave port_redirect=0 so the port
		// is omitted from Location (browsers treat absent port as the scheme default).
		a.PortRedirect = rd.ListenerPort
	}
	switch rd.StatusCode {
	case 302:
		a.ResponseCode = routev3.RedirectAction_FOUND
	default:
		// 301 is the Gateway API default; also covers 0 (unset).
		a.ResponseCode = routev3.RedirectAction_MOVED_PERMANENTLY
	}
	switch rd.PathType {
	case "ReplaceFullPath":
		a.PathRewriteSpecifier = &routev3.RedirectAction_PathRedirect{PathRedirect: rd.PathValue}
	case "ReplacePrefixMatch":
		a.PathRewriteSpecifier = &routev3.RedirectAction_PrefixRewrite{PrefixRewrite: rd.PathValue}
	}
	return &routev3.Route_Redirect{Redirect: a}
}

// applyURLRewrite writes URLRewrite fields onto an existing RouteAction.
// hostname → HostRewriteLiteral; ReplacePrefixMatch → PrefixRewrite (normalized)
// or RegexRewrite (empty-remainder case); ReplaceFullPath → RegexRewrite matching
// .* → the new path.
//
// Gateway API ReplacePrefixMatch segment semantics:
//
//	prefix "/strip-prefix", replacement "/" →
//	  /strip-prefix        → /       (exact match, empty remainder)
//	  /strip-prefix/foo    → /foo
//	  /strip-prefix/       → /
//
// Envoy's prefix_rewrite substitutes the matched prefix bytes, so:
//   - A trailing slash in the replacement causes a double slash when the suffix
//     starts with "/": match "/prefix", replacement "/", path "/prefix/foo"
//     → "/" + "/foo" = "//foo". Fix: strip the trailing slash.
//   - But stripping "/" to "" causes an empty path when the match is exact (no
//     suffix): match "/strip-prefix", replacement "", path "/strip-prefix" → "".
//     Fix: when the replacement is all-slash (e.g. "/"), use regex_rewrite with
//     pattern ^<prefix>/?(.*)$ and substitution /$1, which correctly handles both
//     the empty-remainder (→ /) and suffix (→ /suffix) cases.
//
// matchPrefix is the Envoy path match value (e.g. "/strip-prefix") used to build
// the regex pattern for the empty-remainder case. When empty (unknown at call
// site), the fallback behaviour is the legacy TrimRight approach.
func applyURLRewrite(ra *routev3.RouteAction, rw *GammaURLRewrite, matchPrefix string) {
	if rw == nil {
		return
	}
	if rw.Hostname != "" {
		ra.HostRewriteSpecifier = &routev3.RouteAction_HostRewriteLiteral{HostRewriteLiteral: rw.Hostname}
	}
	switch rw.PathType {
	case "ReplacePrefixMatch":
		trimmed := strings.TrimRight(rw.PathValue, "/")
		if trimmed == "" && matchPrefix != "" {
			// Replacement is "/" (all-slash): plain prefix_rewrite = "" would produce
			// an empty path for exact-match requests (e.g. GET /strip-prefix with no
			// trailing content). Use regex_rewrite instead.
			//
			// Pattern: ^<prefix>/?(.*)$
			//   - ^<prefix>         matches the prefix exactly
			//   - /?                optionally consumes the separator slash
			//   - (.*)              captures the remainder (empty or path tail without leading /)
			//   - $                 anchors the end
			//
			// Substitution: /\1  (Envoy/RE2 capture-group syntax is \1, NOT $1)
			//   - empty remainder → / (the replacement value)
			//   - /foo remainder   → /foo
			ra.RegexRewrite = &matcherv3.RegexMatchAndSubstitute{
				Pattern:      &matcherv3.RegexMatcher{Regex: "^" + regexp.QuoteMeta(matchPrefix) + `/?(.*)$`},
				Substitution: `/\1`,
			}
			return
		}
		// Non-empty trimmed value or unknown match prefix: use plain prefix_rewrite.
		// Trailing slash stripped to prevent double-slash when the suffix starts with "/".
		ra.PrefixRewrite = trimmed
	case "ReplaceFullPath":
		ra.RegexRewrite = &matcherv3.RegexMatchAndSubstitute{
			Pattern:      &matcherv3.RegexMatcher{Regex: ".*"},
			Substitution: rw.PathValue,
		}
	}
}

// NoHTTPPortErrorHeader is the response header naming why a mesh HTTP request
// was refused with 421 (see the Refusal constants).
const NoHTTPPortErrorHeader = "x-aether-error"

// The values of the x-aether-error response header: why the mesh refused an
// HTTP request for a service it knows (proposal 043, "The rule, per entry
// path"). Every refusal is an immediate 421 Misdirected Request with a body
// that lists the service's ports, and never a passthrough: passing an HTTP
// request for a known mesh service to the cluster's own routing fails open.
const (
	// RefusalNoHTTPPort: the service serves no HTTP port at all (proposal 037).
	RefusalNoHTTPPort = "no-http-port"
	// RefusalPortMismatch: the Host header names a port other than the one
	// the client dialled. The dialled port decides; a request is never
	// answered by a port the client did not connect to.
	RefusalPortMismatch = "port-mismatch"
	// RefusalPortNotHTTP: the port named is a port of the service, and is
	// declared raw TCP or UDP.
	RefusalPortNotHTTP = "port-not-http"
	// RefusalPortNotDeclared: the service declares no such port.
	RefusalPortNotDeclared = "port-not-declared"
	// RefusalPortRequired: the address names no port (a port-less name, or
	// the mesh HTTP port) and the service's legacy target, the only thing
	// such an address ever meant, is not an HTTP port.
	RefusalPortRequired = "port-required"
)

// BuildNoHTTPPortVirtualHost returns a virtual host whose every route answers
// 421 Misdirected Request, for a service in scope that serves NO HTTP port
// (proposal 037).
//
// Without it the request gets a 503 with cluster_not_found in
// %RESPONSE_CODE_DETAILS%: deterministic, but indistinguishable from a cluster
// that vanished mid-reload, and it never reaches ODCDS so the coordinator's
// expected 404 does not occur either. The caller asked for HTTP on a service
// that serves none, and nothing said so.
//
// 421 is the status whose definition is "the server is not able to produce a
// response for the combination of scheme and authority" — precisely this case.
// It is also a status no application behind the mesh produces, so it cannot be
// confused with an app's own 404 or 503. And it is immediate: no 2s ODCDS wait
// for a lookup that was never going to succeed.
//
// Exact-domain vhosts outrank the wildcard catch-all, so this cannot be
// shadowed by the ODCDS vhost (route.go's "spike-verified" note on
// exact-domain precedence covers exactly this).
func BuildNoHTTPPortVirtualHost(name string, domains []string, tcpSpellings string) *routev3.VirtualHost {
	body := fmt.Sprintf(
		"aether: %s serves no HTTP port. Reach it over TCP instead: %s\n",
		name, tcpSpellings)
	return BuildRefusalVirtualHost(name, domains, RefusalNoHTTPPort, body)
}

// BuildRefusalVirtualHost returns a virtual host whose every route answers
// 421 Misdirected Request with reason in the x-aether-error response header
// and body as the response body. name identifies the virtual host within its
// route table.
//
// A direct response: it is answered by the proxy at once, it reaches no
// cluster, and it cannot be retried into a different answer.
func BuildRefusalVirtualHost(name string, domains []string, reason, body string) *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    name,
		Domains: domains,
		Routes: []*routev3.Route{{
			Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_DirectResponse{
				DirectResponse: &routev3.DirectResponseAction{
					Status: 421,
					Body: &corev3.DataSource{
						Specifier: &corev3.DataSource_InlineString{InlineString: body},
					},
				},
			},
			ResponseHeadersToAdd: []*corev3.HeaderValueOption{{
				Header: &corev3.HeaderValue{
					Key:   NoHTTPPortErrorHeader,
					Value: reason,
				},
			}},
		}},
	}
}

const (
	// quicClusterSpecifierPluginName is Envoy's matcher-based cluster specifier
	// (envoy.router.cluster_specifier_plugin.matcher), compiled into the proxy.
	quicClusterSpecifierPluginName = "envoy.router.cluster_specifier_plugin.matcher"
	// quicSourceIdentityInputName / quicClusterActionName are cosmetic names for
	// the matcher's input and action extensions; only the typed configs matter.
	quicSourceIdentityInputName = "source-identity"
	quicClusterActionName       = "cluster"
)

// ApplyQUICClusterSelection rewrites, in place, every route on vh that sends
// traffic to h2Cluster so that it selects per SOURCE identity between that
// cluster and the source's own `quic:` cluster (proposal 038 Phase 4b). It
// returns how many routes it rewrote.
//
// The mechanism is a route-level matcher cluster specifier keyed on the
// SourceIdentityCertMapperFilterStateKey filter state -- the source-identity
// stamp every mesh-originating chain already carries (BuildSourceFilterStates;
// the same key the cluster's certificate mapper reads, so the arm a request
// takes and the client certificate it presents come from one value). Until
// #1165 this read the retired aether.source.spiffe_id copy. Nothing
// else would work here: the route tables are node-shared (one cap_http /
// out_http per snapshot), so a route cannot NAME a per-source cluster; and a
// header written by request_headers_to_add is applied AFTER route matching,
// so cluster_header cannot read it. The filter-state input is evaluated at
// match time on the stream's filter state, whose parent is the connection's,
// where set_filter_state put the identity.
//
// arms maps a local source SPIFFE ID to its quic: cluster name; a request
// from an identity with no arm (or with no stamp at all -- a chain built
// before the trust domain was known) takes on_no_match, the h2 cluster, which
// is byte-for-byte the route it had before.
//
// An arm may name a twin the snapshot does NOT carry (issue #1020): the agent
// builds a twin only for a (source, destination) pair that has dialled. The
// matcher's action is a static cluster name (ClusterAction has nothing else), so
// the arm names the twin and the HCM's on_demand filter does the rest -- it
// resolves the route through this plugin, finds no such cluster, and asks for
// it by that name over ODCDS (on_demand_update.cc: entry->clusterName() on the
// plugin's route entry). The agent validates the name, records the pair and
// publishes the twin with its load assignment; the paused request resumes on
// it, over HTTP/3. If the name is refused or the fetch outlives the on_demand
// timeout, the router finds no cluster and answers 503 (NC) -- the same failure
// mode as the mesh catch-all's cold path; there is no per-route fallback to
// the h2 cluster in Envoy's API.
//
// Which routes this reaches (aether#961): EVERY route on the vhost whose
// action names the h2 cluster -- the default route, and a GAMMA (HTTPRoute)
// rule whose single backendRef is the parent Service itself, since it renders
// as the same `cluster:` action. Such a rule rides QUIC with the caller's own
// twin exactly like the default route; nothing is lost. What stays h2 is a
// WeightedClusters action (a GAMMA split, or a rule to another Service's
// cluster): the matcher plugin's action names ONE cluster, so a per-source
// weighted split has no representation here -- not a policy choice.
func ApplyQUICClusterSelection(vh *routev3.VirtualHost, h2Cluster string, arms map[string]string) int {
	if vh == nil || len(arms) == 0 {
		return 0
	}
	m := map[string]*xdsmatcherv3.Matcher_OnMatch{}
	for _, id := range slices.Sorted(maps.Keys(arms)) {
		m[id] = quicClusterOnMatch(arms[id])
	}
	rewritten := 0
	for _, r := range vh.GetRoutes() {
		ra := r.GetRoute()
		if ra == nil || ra.GetCluster() != h2Cluster {
			continue
		}
		ra.ClusterSpecifier = &routev3.RouteAction_InlineClusterSpecifierPlugin{
			InlineClusterSpecifierPlugin: &routev3.ClusterSpecifierPlugin{
				Extension: &corev3.TypedExtensionConfig{
					Name: quicClusterSpecifierPluginName,
					TypedConfig: config.TypedConfig(&matcher_cluster_specifierv3.MatcherClusterSpecifier{
						ClusterMatcher: &xdsmatcherv3.Matcher{
							MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{
								MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
									Input: &xdscorev3.TypedExtensionConfig{
										Name:        quicSourceIdentityInputName,
										TypedConfig: config.TypedConfig(&network_inputsv3.FilterStateInput{Key: SourceIdentityCertMapperFilterStateKey}),
									},
									TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{
										ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: m},
									},
								},
							},
							OnNoMatch: quicClusterOnMatch(h2Cluster),
						},
					}),
				},
			},
		}
		rewritten++
	}
	return rewritten
}

// quicClusterOnMatch is a matcher leaf selecting one cluster.
func quicClusterOnMatch(cluster string) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{
		OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{
			Action: &xdscorev3.TypedExtensionConfig{
				Name:        quicClusterActionName,
				TypedConfig: config.TypedConfig(&matcher_cluster_specifierv3.ClusterAction{Cluster: cluster}),
			},
		},
	}
}

// QUICSelectionArms reads back the (source SPIFFE ID -> cluster) arms and the
// on_no_match cluster of a route rewritten by ApplyQUICClusterSelection, or
// ok=false when the route carries no such plugin. Test and gate helper.
func QUICSelectionArms(r *routev3.Route) (arms map[string]string, noMatch string, ok bool) {
	p := r.GetRoute().GetInlineClusterSpecifierPlugin().GetExtension()
	if p == nil || p.GetName() != quicClusterSpecifierPluginName {
		return nil, "", false
	}
	spec := &matcher_cluster_specifierv3.MatcherClusterSpecifier{}
	if err := p.GetTypedConfig().UnmarshalTo(spec); err != nil {
		return nil, "", false
	}
	tree := spec.GetClusterMatcher().GetMatcherTree()
	arms = map[string]string{}
	for id, om := range tree.GetExactMatchMap().GetMap() {
		a := &matcher_cluster_specifierv3.ClusterAction{}
		if err := om.GetAction().GetTypedConfig().UnmarshalTo(a); err == nil {
			arms[id] = a.GetCluster()
		}
	}
	nm := &matcher_cluster_specifierv3.ClusterAction{}
	_ = spec.GetClusterMatcher().GetOnNoMatch().GetAction().GetTypedConfig().UnmarshalTo(nm)
	return arms, nm.GetCluster(), true
}
