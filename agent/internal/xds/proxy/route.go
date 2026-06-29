package proxy

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
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

// KnownTargetRoute pins a known in-scope mesh service's non-mesh dial spellings
// (its cluster.local short names and FQDN, optionally with a port) to the
// service's data-plane cluster. The redirect-all capture catch-all emits one
// such route per in-scope mesh authority BEFORE the passthrough fallthrough so a
// captured request to a service the mesh KNOWS ABOUT can never be shadowed by the
// ORIGINAL_DST passthrough — even during the brief window where the service's
// dedicated cap_http vhost is being rebuilt (a GAMMA HTTPRoute add/delete clears
// and re-populates serviceRoutes between two reconciles; an in-flight captured
// request landing in that window used to miss the dedicated vhost and fall to the
// passthrough → kube-proxy → GAMMA features silently dropped). Because the
// catch-all routes are derived from the STABLE mesh-authority set
// (captureAuthorities ∩ dependency set, independent of HTTPRoute lifecycle), the
// request still reaches the correct backend cluster (mTLS, in-mesh) rather than
// leaking to kube-proxy. The dedicated vhost remains the precedence path when
// present (it carries the GAMMA filters); this is the safety net that fails into
// the mesh, never out of it.
type KnownTargetRoute struct {
	// AuthorityRegex is an RE2 pattern over all of the service's non-mesh dial
	// spellings (bare name, <name>.<ns>, <name>.<ns>.svc, cluster.local FQDN) with
	// an optional ":port" suffix. Port-agnostic by construction so it matches a
	// dial on any real Service port without the control plane needing to track it.
	AuthorityRegex string
	// Cluster is the service's data-plane cluster (<svc>.<meshDomain>) the matched
	// authority routes to.
	Cluster string
}

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
//
// knownTargets is consulted only in redirect-all (passthrough) mode: each pins a
// known in-scope service's non-mesh authority spellings to its cluster so the
// passthrough never shadows a service the mesh knows about (see KnownTargetRoute).
func buildOnDemandCatchAllVirtualHost(meshDomain string, passthrough bool, knownTargets ...KnownTargetRoute) *routev3.VirtualHost {
	// 020 Part 1: mesh authorities are <svc>.<ns>.<meshDomain> (two labels before the
	// mesh domain), with an optional :port. Cold/off-node services that miss the
	// scoped vhost fall here and resolve via ODCDS (ClusterHeader=:authority).
	meshAuthorityRegex := "^[a-z0-9-]+\\.[a-z0-9-]+\\." + regexp.QuoteMeta(meshDomain) + "(:[0-9]+)?$"
	// Final fallthrough for a non-mesh authority. Normally a hard 404 (foreign egress,
	// typos). But with redirect-all capture (proposal 022) ALL of the pod's egress is
	// intercepted — including requests to real Kubernetes Service names
	// (<svc>.<ns>.svc.cluster.local / a bare ClusterIP) that carry no mesh authority
	// and are in no node dependency set (a client dialing a Service with no
	// HTTPRoute/upstream declaration). A 404 there breaks plain Service-to-Service
	// reachability, so route those to the ORIGINAL_DST passthrough (the original
	// pre-REDIRECT destination, reached in plain HTTP), symmetric with the L4
	// cap_passthrough default chain. The capture HCM's set_filter_state filter
	// preserves the original destination for the ORIGINAL_DST cluster. Without
	// redirect-all (scoped capture / outbound) the 404 is kept — there is no
	// passthrough cluster to route to.
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
	}
	// Known-target safety net (redirect-all only): a captured request whose
	// authority is a known in-scope mesh service — under any of its non-mesh
	// cluster.local spellings, on any port — routes to that service's cluster
	// rather than falling to the passthrough. This holds even while the service's
	// dedicated cap_http vhost is being rebuilt across a GAMMA HTTPRoute add/delete
	// (the dedicated vhost, when present, wins on host-match precedence and carries
	// the GAMMA filters; this catch-all entry is the floor that keeps a known
	// target in the mesh instead of leaking to kube-proxy). Disjoint authorities,
	// so route order among them is irrelevant; all precede the passthrough. Only
	// meaningful in redirect-all mode — without a passthrough there is nothing to
	// shadow and the catch-all keeps its hard 404.
	for _, kt := range knownTargets {
		if !passthrough || kt.AuthorityRegex == "" || kt.Cluster == "" {
			continue
		}
		routes = append(routes, &routev3.Route{
			Match: &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
				Headers: []*routev3.HeaderMatcher{
					{
						Name: ":authority",
						HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
							StringMatch: &matcherv3.StringMatcher{
								MatchPattern: &matcherv3.StringMatcher_SafeRegex{
									SafeRegex: &matcherv3.RegexMatcher{Regex: kt.AuthorityRegex},
								},
							},
						},
					},
				},
			},
			Action: &routev3.Route_Route{
				Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: kt.Cluster},
					RetryPolicy:      outboundRetryPolicy(),
				},
			},
		})
	}
	routes = append(routes, fallthrough_)
	return &routev3.VirtualHost{
		Name:    "on_demand_catch_all",
		Domains: []string{"*"},
		Routes:  routes,
	}
}

// BuildOutboundRouteConfiguration creates a route configuration for outbound traffic.
// It includes the provided virtual hosts plus the universal on-demand catch-all
// virtual host (mesh-shaped authorities → ODCDS, everything else → 404). The outbound
// listener never passes through (it only sees mesh-DNS authorities), so the catch-all
// keeps its hard-404 fallthrough.
func BuildOutboundRouteConfiguration(vhosts []*routev3.VirtualHost, meshDomain string) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         OutboundHTTPRouteName,
		VirtualHosts: append(vhosts, buildOnDemandCatchAllVirtualHost(meshDomain, false)),
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

// GAMMA east-west L7 routing (proposal 018, Phase 2). A GammaRoute is one resolved
// HTTPRoute rule: ordered matches forwarding to one or more weighted backend
// clusters, with an optional timeout, header mutations, redirect, and URL rewrite.
// Backends are already resolved to data-plane cluster names (<backend>.<meshDomain>)
// by the reconciler.
type GammaRoute struct {
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
type GammaBackend struct {
	Service string
	Cluster string
	Weight  uint32
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
	if len(rules) == 0 {
		return BuildOutboundClusterVirtualHost(name, domains)
	}
	// One route per (rule, match). Gateway API mandates that routes be evaluated by
	// match SPECIFICITY, not document order: a longer/exact path (and tie-broken by
	// header-match count) wins over a shorter one regardless of where it appears in
	// the HTTPRoute. Envoy is first-match on the route list, so we must emit the
	// routes pre-sorted (descending specificity). Without this, an earlier rule's
	// "/" prefix shadows every later, more-specific rule (e.g. "/v2"). The edge path
	// already does this (sortRoutesBySpecificity); GAMMA must match.
	type scoredRoute struct {
		route *routev3.Route
		key   int64
	}
	var scored []scoredRoute
	for _, rule := range rules {
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []GammaMatch{{Prefix: "/"}}
		}
		reqAdd, respAdd, reqRemove, respRemove := headerMutationFields(rule.HeaderMutation)
		for _, m := range matches {
			r := &routev3.Route{
				Match:                   gammaRouteMatch(m),
				RequestHeadersToAdd:     reqAdd,
				RequestHeadersToRemove:  reqRemove,
				ResponseHeadersToAdd:    respAdd,
				ResponseHeadersToRemove: respRemove,
			}
			if rule.Redirect != nil {
				r.Action = gammaRedirectAction(rule.Redirect)
			} else {
				r.Action = gammaRouteAction(name, rule, m.Prefix)
			}
			scored = append(scored, scoredRoute{route: r, key: gammaMatchSpecificity(m)})
		}
	}
	// Stable sort by descending specificity: ties preserve document order (Gateway
	// API tie-break after path/method/header is creation order, approximated here).
	slices.SortStableFunc(scored, func(a, b scoredRoute) int {
		if a.key != b.key {
			if a.key > b.key {
				return -1
			}
			return 1
		}
		return 0
	})
	routes := make([]*routev3.Route, 0, len(scored)+1)
	for _, s := range scored {
		routes = append(routes, s.route)
	}
	routes = append(routes, &routev3.Route{
		Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{
			ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: name},
			RetryPolicy:      outboundRetryPolicy(),
		}},
	})
	return &routev3.VirtualHost{Name: name, Domains: domains, Routes: routes}
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
		ra.ClusterSpecifier = &routev3.RouteAction_Cluster{Cluster: serviceCluster}
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
