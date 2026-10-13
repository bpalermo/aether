package proxy

import (
	"maps"
	"slices"
	"strings"

	"aethermesh.dev/agent/internal/xds/config"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// A SHORT NAME is a Host with no namespace: "echo", "echo:8080". It means the
// service of that name in the CALLER's namespace, as the cluster's DNS search
// path does, and never a service of another namespace, whether or not the name
// is unique in the cluster.
//
// The route tables are shared by every pod of the node, so the caller is not
// in the table: it is on the connection. Each pod's HTTP chains store the
// pod's namespace in filter state (CallerNamespaceFilterStateKey), which a
// client cannot set, and a short name's virtual host selects its routes by
// that value (the generic matching API on the virtual host):
//
//	virtual host "echo|short|:8080", domains ["echo:8080"]
//	  matcher, input = filter state aether.source.namespace
//	    "a" -> the routes "echo.a.<mesh>:8080" has in this table
//	    "b" -> the routes "echo.b.<mesh>:8080" has in this table
//	    no match (another namespace, or no namespace on the connection)
//	        -> 421 name-not-in-namespace
//
// The arms are COPIES of what the full name gets in the same table, so a short
// name is routed, refused, parked and filtered exactly like the full name of
// the caller's namespace: per-port rules, HTTPRoute rules, refusals and the
// cold path are not written twice.
const (
	// CallerNamespaceFilterStateKey holds the namespace of the pod a
	// connection came from, on that pod's HTTP chains.
	CallerNamespaceFilterStateKey = "aether.source.namespace"

	// RefusalNameNotInNamespace: a short name whose service does not exist in
	// the caller's namespace (or a connection that carries no namespace).
	RefusalNameNotInNamespace = "name-not-in-namespace"

	shortNameInputName  = "aether.matching.inputs.caller_namespace"
	shortNameActionName = "aether.matching.actions.routes"
)

// BuildCallerNamespaceFilterState returns the network filter that stores a
// pod's namespace under CallerNamespaceFilterStateKey. It is not shared with
// the upstream connection: only the route tables read it.
func BuildCallerNamespaceFilterState(namespace string) *listenerv3.Filter {
	return networkFilter("envoy.filters.network.set_filter_state", &set_filter_state_v3.Config{
		OnNewConnection: []*setFilterStatev3.FilterStateValue{{
			Key:        &setFilterStatev3.FilterStateValue_ObjectKey{ObjectKey: CallerNamespaceFilterStateKey},
			FactoryKey: genericStringFactory,
			Value: &setFilterStatev3.FilterStateValue_FormatString{FormatString: &corev3.SubstitutionFormatString{
				Format: &corev3.SubstitutionFormatString_TextFormatSource{TextFormatSource: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineString{InlineString: namespace},
				}},
			}},
		}},
	})
}

// parseMeshAuthority splits a virtual-host domain of the form
// "<svc>.<ns>.<meshDomain><suffix>", where suffix is "", ":<port>" or ":*".
func parseMeshAuthority(domain, meshDomain string) (svc, ns, suffix string, ok bool) {
	name := domain
	if i := strings.IndexByte(domain, ':'); i >= 0 {
		name, suffix = domain[:i], domain[i:]
	}
	rest, isMesh := strings.CutSuffix(name, "."+meshDomain)
	if !isMesh {
		return "", "", "", false
	}
	svc, ns, two := strings.Cut(rest, ".")
	if !two || svc == "" || ns == "" || strings.Contains(ns, ".") {
		return "", "", "", false
	}
	return svc, ns, suffix, true
}

// shortArm is what one namespace's service answers for one short domain.
type shortArm struct {
	routes []*routev3.Route
}

// ShortNameVirtualHosts returns the virtual hosts of the short names of a
// route table, built from the table's own virtual hosts (see the comment at
// the top of the file). vhosts is not modified.
//
// For every mesh authority "<svc>.<ns>.<meshDomain><suffix>" some virtual host
// claims, the short domain "<svc><suffix>" gets one virtual host whose matcher
// has an arm per namespace. The arm of a namespace is the route list of the
// virtual host that the FULL name of that namespace would be matched by for
// the same suffix: the one that claims it exactly, or, for a suffix with a
// port, the one that claims "<full name>:*". A namespace that has neither has
// no arm, and a request from it is refused like one from a namespace that has
// no such service.
//
// A virtual host's own typed_per_filter_config (a service-wide HTTP filter is
// enabled there) is copied onto every route of its arm: inside a virtual host
// shared by several namespaces, the route is the only place left that belongs
// to one service. A route's own entry for the same filter wins, as it does
// under its own virtual host.
func ShortNameVirtualHosts(vhosts []*routev3.VirtualHost, meshDomain string) []*routev3.VirtualHost {
	claims, suffixes := shortNameClaims(vhosts, meshDomain)
	var out []*routev3.VirtualHost
	for _, svc := range slices.Sorted(maps.Keys(suffixes)) {
		for _, suffix := range slices.Sorted(maps.Keys(suffixes[svc])) {
			out = append(out, shortNameVirtualHost(svc, suffix, shortNameArms(claims, svc, suffix)))
		}
	}
	return out
}

// shortOwner is one namespace's service of a short name.
type shortOwner struct{ svc, ns string }

// shortNameClaims indexes a table's virtual hosts by the mesh authorities they
// claim: per (service, namespace) and suffix, the virtual host; and per short
// name, the suffixes any namespace's service is claimed with.
func shortNameClaims(vhosts []*routev3.VirtualHost, meshDomain string) (map[shortOwner]map[string]*routev3.VirtualHost, map[string]map[string]struct{}) {
	claims := map[shortOwner]map[string]*routev3.VirtualHost{}
	suffixes := map[string]map[string]struct{}{}
	for _, vh := range vhosts {
		for _, d := range vh.GetDomains() {
			svc, ns, suffix, ok := parseMeshAuthority(d, meshDomain)
			if !ok {
				continue
			}
			o := shortOwner{svc: svc, ns: ns}
			if claims[o] == nil {
				claims[o] = map[string]*routev3.VirtualHost{}
			}
			claims[o][suffix] = vh
			if suffixes[svc] == nil {
				suffixes[svc] = map[string]struct{}{}
			}
			suffixes[svc][suffix] = struct{}{}
		}
	}
	return claims, suffixes
}

// shortNameArms returns, per namespace, what that namespace's service of svc
// answers for the authority suffix: the routes of the virtual host that
// claims its full name with that suffix exactly, or, for a suffix with a port,
// the one that claims "<full name>:*".
func shortNameArms(claims map[shortOwner]map[string]*routev3.VirtualHost, svc, suffix string) map[string]shortArm {
	arms := map[string]shortArm{}
	for o, bySuffix := range claims {
		if o.svc != svc {
			continue
		}
		vh := bySuffix[suffix]
		if vh == nil && suffix != "" && suffix != ":*" {
			vh = bySuffix[":*"]
		}
		if vh != nil {
			arms[o.ns] = shortArm{routes: armRoutes(vh)}
		}
	}
	return arms
}

// armRoutes copies a virtual host's routes for use as a matcher arm, with the
// virtual host's typed_per_filter_config on each of them.
func armRoutes(vh *routev3.VirtualHost) []*routev3.Route {
	routes := make([]*routev3.Route, 0, len(vh.GetRoutes()))
	for _, r := range vh.GetRoutes() {
		c := proto.Clone(r).(*routev3.Route)
		for name, cfg := range vh.GetTypedPerFilterConfig() {
			if _, own := c.GetTypedPerFilterConfig()[name]; own {
				continue
			}
			if c.TypedPerFilterConfig == nil {
				c.TypedPerFilterConfig = map[string]*anypb.Any{}
			}
			c.TypedPerFilterConfig[name] = cfg
		}
		routes = append(routes, c)
	}
	return routes
}

// ShortNameVirtualHostName names the virtual host of a short domain.
func ShortNameVirtualHostName(svc, suffix string) string {
	return svc + "|short|" + suffix
}

func shortNameVirtualHost(svc, suffix string, arms map[string]shortArm) *routev3.VirtualHost {
	byNamespace := make(map[string]*xdsmatcherv3.Matcher_OnMatch, len(arms))
	for ns, arm := range arms {
		byNamespace[ns] = routeListOnMatch(arm.routes)
	}
	body := "aether: " + svc + " is a name without a namespace, and the namespace this request came from has no mesh service of that name. " +
		"Address it as " + svc + ".<namespace>" + strings.TrimSuffix(suffix, "*") + "<port>.\n"
	if suffix != ":*" {
		body = "aether: " + svc + " is a name without a namespace, and the namespace this request came from has no mesh service of that name. " +
			"Address it as " + svc + ".<namespace>" + suffix + ".\n"
	}
	refusal := BuildRefusalVirtualHost("", nil, RefusalNameNotInNamespace, body)
	return &routev3.VirtualHost{
		Name:    ShortNameVirtualHostName(svc, suffix),
		Domains: []string{svc + suffix},
		Matcher: &xdsmatcherv3.Matcher{
			MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
				Input: &xdscorev3.TypedExtensionConfig{
					Name:        shortNameInputName,
					TypedConfig: config.TypedConfig(&network_inputsv3.FilterStateInput{Key: CallerNamespaceFilterStateKey}),
				},
				TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{
					ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: byNamespace},
				},
			}},
			// No arm: another namespace, or a connection that carries no
			// namespace at all. Refused; never another namespace's service,
			// never the table's catch-all.
			OnNoMatch: routeListOnMatch(refusal.GetRoutes()),
		},
	}
}

func routeListOnMatch(routes []*routev3.Route) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{
		Name:        shortNameActionName,
		TypedConfig: config.TypedConfig(&routev3.RouteList{Routes: routes}),
	}}}
}
