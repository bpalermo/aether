package proxy

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"aethermesh.dev/agent/internal/xds/config"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/common/extensionfilter"
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
//	    another namespace -> what the table gives a Host that is no mesh
//	        name: "echo" is not a mesh name for that caller
//	    no namespace on the connection -> 421 caller-namespace-unknown
//
// The arms are COPIES of what the full name gets in the same table, so a short
// name is routed, refused, parked and filtered exactly like the full name of
// the caller's namespace: per-port rules, HTTPRoute rules, refusals and the
// cold path are not written twice.
const (
	// CallerNamespaceFilterStateKey holds the namespace of the pod a
	// connection came from, on that pod's HTTP chains.
	CallerNamespaceFilterStateKey = "aether.source.namespace"

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

// ShortNameFloor says what a table's floor (its catch-all) does with the FULL
// name fqdn followed by suffix ("", ":<port>" or ":*"), as routes, for a mesh
// name no virtual host of the table claims. Nil routes mean no arm: the short
// name is then refused for that caller like a name its namespace does not
// have.
type ShortNameFloor func(fqdn, suffix string) []*routev3.Route

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
// known lists the mesh names ("<svc>.<ns>.<meshDomain>") the table answers
// for, whether or not a virtual host claims them. One that NO virtual host
// claims (a service the node knows by name only) is answered, under its full
// name, by the table's floor; floor says how, and its short name gets an arm
// that does the same. Without it the caller's namespace HAS the service and
// its short name was refused "name-not-in-namespace" as soon as another
// namespace's service of the same name had a virtual host, and fell to the
// catch-all, which no short authority matches, otherwise.
//
// A virtual host's own typed_per_filter_config (a service-wide HTTP filter is
// enabled there) goes onto every route of its arm: inside a virtual host
// shared by several namespaces, the route is the only place left that belongs
// to one service. When a route names the SAME filter (an HTTPRoute rule's
// ExtensionRef of the same kind), the proxy had two levels under the service's
// own virtual host and has one place here, so the route carries what the two
// levels amount to for that filter (extensionfilter.CombineLevels): the
// route's alone for a filter whose most specific level wins, both for one
// that applies or merges every level. Keeping only the route's entry ran the
// request without the service-wide filter.
//
// A route that names its cluster by the request's authority (the on-demand
// route of a service the node holds no cluster of) cannot be copied as it is:
// under a short name the authority is "echo:8080", which is no cluster's name
// and which the agent does not answer, so the request waited out the on-demand
// timeout. In an arm it names the cluster the FULL name spells,
// "<svc>.<ns>.<meshDomain>:<port>", when the short domain has a port; a
// wildcard domain has none to spell, and the request is refused at once
// (RefusalShortNameCold).
//
// notMesh are the routes of the table's floor: what a Host that is no mesh
// name gets in this table. A caller whose namespace has no mesh service of the
// short name gets exactly those (see shortNameVirtualHost).
func ShortNameVirtualHosts(vhosts []*routev3.VirtualHost, meshDomain string, known []string, floor ShortNameFloor, notMesh []*routev3.Route) []*routev3.VirtualHost {
	claims, suffixes := shortNameClaims(vhosts, meshDomain)
	unclaimed := unclaimedNames(claims, suffixes, meshDomain, known, floor)
	var out []*routev3.VirtualHost
	for _, svc := range slices.Sorted(maps.Keys(suffixes)) {
		for _, suffix := range slices.Sorted(maps.Keys(suffixes[svc])) {
			arms := shortNameArms(claims, svc, suffix, meshDomain)
			for _, o := range unclaimed[svc] {
				if routes := floor(o.svc+"."+o.ns+"."+meshDomain, suffix); len(routes) > 0 {
					arms[o.ns] = shortArm{routes: routes}
				}
			}
			if len(arms) > 0 {
				out = append(out, shortNameVirtualHost(svc, suffix, arms, notMesh))
			}
		}
	}
	return out
}

// unclaimedNames returns, per short name, the known mesh names no virtual
// host of the table claims, and adds the two suffixes their floor arms answer
// for ("" and ":*") to suffixes. Nothing when the table has no floor to say
// what such a name gets.
func unclaimedNames(claims map[shortOwner]map[string]*routev3.VirtualHost, suffixes map[string]map[string]struct{}, meshDomain string, known []string, floor ShortNameFloor) map[string][]shortOwner {
	if floor == nil {
		return nil
	}
	out := map[string][]shortOwner{}
	for _, name := range known {
		svc, ns, suffix, ok := parseMeshAuthority(name, meshDomain)
		o := shortOwner{svc: svc, ns: ns}
		if !ok || suffix != "" || len(claims[o]) > 0 {
			continue
		}
		out[svc] = append(out[svc], o)
		if suffixes[svc] == nil {
			suffixes[svc] = map[string]struct{}{}
		}
		suffixes[svc][""] = struct{}{}
		suffixes[svc][":*"] = struct{}{}
	}
	return out
}

// ShortNames is what a capture table's builder needs to add its short names.
type ShortNames struct {
	MeshDomain string
	// Known and Floor: see ShortNameVirtualHosts. Both empty for a table
	// that gives every name a virtual host.
	Known []string
	Floor ShortNameFloor
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
func shortNameArms(claims map[shortOwner]map[string]*routev3.VirtualHost, svc, suffix, meshDomain string) map[string]shortArm {
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
			arms[o.ns] = shortArm{routes: armRoutes(vh, o.svc+"."+o.ns+"."+meshDomain, suffix)}
		}
	}
	return arms
}

// armRoutes copies a virtual host's routes for use as a matcher arm, with the
// virtual host's typed_per_filter_config in force on each of them.
func armRoutes(vh *routev3.VirtualHost, fqdn, suffix string) []*routev3.Route {
	routes := make([]*routev3.Route, 0, len(vh.GetRoutes()))
	for _, r := range vh.GetRoutes() {
		routes = append(routes, nameOnDemandCluster(armRoute(vh, r), fqdn, suffix))
	}
	return routes
}

// RefusalShortNameCold: a short name of a service this node does not know the
// ports of yet, on a port it cannot tell is one of them.
const RefusalShortNameCold = "short-name-cold"

// nameOnDemandCluster makes a route that names its cluster by the request's
// authority name the cluster of the FULL name instead (see
// ShortNameVirtualHosts). r is the arm's own copy and is written to.
func nameOnDemandCluster(r *routev3.Route, fqdn, suffix string) *routev3.Route {
	if r.GetRoute().GetClusterHeader() != onDemandClusterHeader {
		return r
	}
	if suffix != "" && suffix != ":*" {
		r.GetRoute().ClusterSpecifier = &routev3.RouteAction_Cluster{Cluster: fqdn + suffix}
		return r
	}
	refusal := shortNameColdRefusal(fqdn)
	refusal.Match = r.GetMatch()
	return refusal
}

// shortNameColdRefusal is the route that refuses a short name on a port that
// cannot be told to be one of the service's: this node knows fqdn by name
// only, the request would have to wait for a cluster named by the authority,
// and a short authority names none.
func shortNameColdRefusal(fqdn string) *routev3.Route {
	return BuildRefusalVirtualHost("", nil, RefusalShortNameCold,
		"aether: this node does not know the ports of "+fqdn+" yet, and cannot fetch it by a name without a namespace. "+
			"Address it as "+fqdn+":<port>; the short name works once the node knows the service.\n").GetRoutes()[0]
}

// LoopbackShortNameFloor is the ShortNameFloor of the tables whose floor takes
// the port from the authority (the loopback table, and the proxy port's
// capture table, which is given the same virtual hosts): a mesh name with a
// port is offered on demand as "<fqdn>:<port>", except the mesh's own two
// ports; without a port it names nothing.
func LoopbackShortNameFloor(fqdn, suffix string) []*routev3.Route {
	switch suffix {
	case "":
		return BuildRefusalVirtualHost("", nil, RefusalPortRequired,
			"aether: "+fqdn+" was addressed without a port, and this node does not know its ports yet. Address it as "+fqdn+":<port>.\n").GetRoutes()
	case ":*":
		return []*routev3.Route{shortNameColdRefusal(fqdn)}
	case fmt.Sprintf(":%d", meshconst.ProxyOutboundPort), fmt.Sprintf(":%d", meshconst.ProxyL4OutboundPort):
		return BuildRefusalVirtualHost("", nil, RefusalPortNotDeclared, RefusalReservedPortBody).GetRoutes()
	}
	return BuildOutboundClusterVirtualHost(fqdn+suffix, nil).GetRoutes()
}

// FallthroughShortNameFloor is the ShortNameFloor of a table that claims
// nothing for a name nothing is known of and has no on-demand route for it
// (cap_http_other): the request leaves as any name that is not the mesh's
// does, through the ORIGINAL_DST passthrough with redirect-all, 404 without.
func FallthroughShortNameFloor(passthrough bool) ShortNameFloor {
	return func(string, string) []*routev3.Route {
		routes := buildOnDemandCatchAllVirtualHost("", passthrough).GetRoutes()
		return routes[len(routes)-1:]
	}
}

// RefusalFilterLevels: a short-name route on which a service-wide filter and
// the route's own filter of the same kind could not be combined.
const RefusalFilterLevels = "filter-levels"

// armRoute is one route of an arm. A route on which the two levels of a
// filter cannot be combined is not delivered at all: a 500 that says so. It
// cannot happen for an allow-listed filter with a config that decodes, and a
// request must not run without a filter because of a config that does not.
func armRoute(vh *routev3.VirtualHost, r *routev3.Route) *routev3.Route {
	c := proto.Clone(r).(*routev3.Route)
	for name, cfg := range vh.GetTypedPerFilterConfig() {
		combined, err := extensionfilter.CombineLevels(name, cfg, c.GetTypedPerFilterConfig()[name])
		if err != nil {
			return &routev3.Route{
				Match: c.GetMatch(),
				Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
					Status: 500,
					Body: &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{
						InlineString: "aether: the service's filter " + name + " and this route's own could not be combined for a name without a namespace: " + err.Error() + "\n",
					}},
				}},
				ResponseHeadersToAdd: []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: NoHTTPPortErrorHeader, Value: RefusalFilterLevels}}},
			}
		}
		if c.TypedPerFilterConfig == nil {
			c.TypedPerFilterConfig = map[string]*anypb.Any{}
		}
		c.TypedPerFilterConfig[name] = combined
	}
	return c
}

// ShortNameVirtualHostName names the virtual host of a short domain.
func ShortNameVirtualHostName(svc, suffix string) string {
	return svc + "|short|" + suffix
}

// RefusalCallerUnknown: a short name on a connection that carries no caller
// namespace. That is a defect of the proxy's own configuration (every pod's
// HTTP chains store it), never something a client can cause, and the one case
// a short name is refused in: without the caller nothing says whose name it
// is.
const RefusalCallerUnknown = "caller-namespace-unknown"

// shortNameVirtualHost builds the virtual host of one short domain. Its
// matcher answers three cases, and keeps them apart:
//
//   - the caller's namespace has a mesh service of the name: that service's
//     arm, the routes its full name has in this table;
//   - the caller's namespace has NONE: the name is not a mesh name for this
//     caller, whether or not another namespace has a mesh service called the
//     same, and the request gets notMesh, exactly what any Host that is no
//     mesh name gets in this table (on a captured dial with redirect-all, the
//     passthrough to the address that was dialled). The owner's ruling of
//     2026-10-12; until then it was refused "name-not-in-namespace", which
//     broke a plain Service of that name in the caller's own namespace as
//     soon as another namespace had a mesh service of it;
//   - the connection carries no caller namespace at all: refused. Without it
//     the first two cannot be told apart, and guessing either way is wrong.
//
// So the outer matcher asks only whether the namespace is there, and the
// inner one, reached when it is, picks the arm or the table's floor.
func shortNameVirtualHost(svc, suffix string, arms map[string]shortArm, notMesh []*routev3.Route) *routev3.VirtualHost {
	byNamespace := make(map[string]*xdsmatcherv3.Matcher_OnMatch, len(arms))
	for ns, arm := range arms {
		byNamespace[ns] = routeListOnMatch(arm.routes)
	}
	input := &xdscorev3.TypedExtensionConfig{
		Name:        shortNameInputName,
		TypedConfig: config.TypedConfig(&network_inputsv3.FilterStateInput{Key: CallerNamespaceFilterStateKey}),
	}
	byCaller := &xdsmatcherv3.Matcher{
		MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
			Input:    input,
			TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: byNamespace}},
		}},
		OnNoMatch: routeListOnMatch(notMeshRoutes(notMesh)),
	}
	unknown := BuildRefusalVirtualHost("", nil, RefusalCallerUnknown,
		"aether: "+svc+" is a name without a namespace, and this connection does not say which namespace it came from. "+
			"This is a fault of the node proxy's configuration, not of the request; the full name "+svc+".<namespace> is not affected.\n")
	return &routev3.VirtualHost{
		Name:    ShortNameVirtualHostName(svc, suffix),
		Domains: []string{svc + suffix},
		Matcher: &xdsmatcherv3.Matcher{
			MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{
				Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{{
					Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{
						SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
							Input: input,
							Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{
								ValueMatch: &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_SafeRegex{SafeRegex: &xdsmatcherv3.RegexMatcher{
									EngineType: &xdsmatcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &xdsmatcherv3.RegexMatcher_GoogleRE2{}},
									Regex:      ".+",
								}}},
							},
						},
					}},
					OnMatch: &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: byCaller}},
				}},
			}},
			OnNoMatch: routeListOnMatch(unknown.GetRoutes()),
		},
	}
}

// notMeshRoutes copies the routes of a table's floor for use as the answer to
// a short name that is no mesh name for its caller. A table with no floor
// (none is built without one) answers 404, the floor's own last resort.
func notMeshRoutes(floor []*routev3.Route) []*routev3.Route {
	if len(floor) == 0 {
		return []*routev3.Route{{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 404}},
		}}
	}
	out := make([]*routev3.Route, 0, len(floor))
	for _, r := range floor {
		out = append(out, proto.Clone(r).(*routev3.Route))
	}
	return out
}

func routeListOnMatch(routes []*routev3.Route) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{
		Name:        shortNameActionName,
		TypedConfig: config.TypedConfig(&routev3.RouteList{Routes: routes}),
	}}}
}
