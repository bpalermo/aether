package proxy

import (
	"regexp"
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
	// Port is the target port (0 = unchanged).
	Port uint32
	// StatusCode is the HTTP redirect code (301 or 302; default 301).
	StatusCode int
	// PathType selects how to rewrite the path (empty = no path rewrite).
	// "ReplaceFullPath" → Envoy PathRedirect; "ReplacePrefixMatch" → PrefixRewrite.
	PathType  string
	PathValue string
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
	var routes []*routev3.Route
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
				r.Action = gammaRouteAction(name, rule)
			}
			routes = append(routes, r)
		}
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
		rm.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: m.Prefix}
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

func gammaRouteAction(serviceCluster string, rule GammaRoute) *routev3.Route_Route {
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
	applyURLRewrite(ra, rule.URLRewrite)
	return &routev3.Route_Route{Route: ra}
}

// gammaRedirectAction converts a GammaRedirect into an Envoy Route_Redirect action.
// The returned action replaces the RouteAction entirely — no backend cluster is used.
func gammaRedirectAction(rd *GammaRedirect) *routev3.Route_Redirect {
	a := &routev3.RedirectAction{}
	if rd.Scheme != "" {
		a.SchemeRewriteSpecifier = &routev3.RedirectAction_SchemeRedirect{SchemeRedirect: rd.Scheme}
	}
	if rd.Hostname != "" {
		a.HostRedirect = rd.Hostname
	}
	if rd.Port != 0 {
		a.PortRedirect = rd.Port
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
// hostname → HostRewriteLiteral; ReplacePrefixMatch → PrefixRewrite;
// ReplaceFullPath → RegexRewrite matching .* → the new path.
func applyURLRewrite(ra *routev3.RouteAction, rw *GammaURLRewrite) {
	if rw == nil {
		return
	}
	if rw.Hostname != "" {
		ra.HostRewriteSpecifier = &routev3.RouteAction_HostRewriteLiteral{HostRewriteLiteral: rw.Hostname}
	}
	switch rw.PathType {
	case "ReplacePrefixMatch":
		ra.PrefixRewrite = rw.PathValue
	case "ReplaceFullPath":
		ra.RegexRewrite = &matcherv3.RegexMatchAndSubstitute{
			Pattern:      &matcherv3.RegexMatcher{Regex: ".*"},
			Substitution: rw.PathValue,
		}
	}
}
