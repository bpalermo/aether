package proxy

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// MergeCaptureRouteTables compiles the per-port capture tables (proposal 043,
// "The addressing rule") into the ONE table the capture listener's cap_http
// chain serves.
//
// The per-port tables are the rule: perPort[D] is what a request dialled to
// port D gets, for every HTTP port number D of the mesh, and other is what a
// request dialled to any other port gets. The compiled table must give every
// request the answer the table of its dialled port gives it, and does so by
// construction:
//
//   - its domains are every domain of every per-port table, so the proxy
//     selects, for any Host, a virtual host holding the domain that is the
//     most specific match of that Host in ANY per-port table;
//   - that virtual host is a plain, ordered route list (no matcher): for each
//     dialled port whose answers differ from the other ports', the routes of
//     that port's answers, each route also matching the dialled port
//     (RouteMatch.filter_state on DialledPortFilterStateKey, which the
//     route's stream reads from the connection) and, when the port answers
//     some spellings of the name differently from others, the Host's port
//     suffix (an :authority regex: no port, ":<D>", exactly one port); then
//     the other ports' answers, matching no dialled port.
//
// Every answer's route list ends in a route every request takes (a service's
// fallback, a refusal), so a request a block's conditions take never reaches
// a later block, and first-match gives each request its dialled port's answer.
// "<name>:8080:8080" has no suffix the regex accepts: a mismatch, as it was.
//
// An answer is the per-port virtual host's routes, with that virtual host's
// typed_per_filter_config in force on each route (a service-wide HTTP filter
// is enabled there, and the compiled virtual host also answers the refusals,
// which must not run it), or, for a short name, its caller-namespace matcher
// flattened the same way: each namespace's routes under a filter-state match
// on the caller's namespace, then what a caller of no such namespace gets,
// then what a connection with no namespace gets.
//
// Domains are grouped into virtual hosts by the name of the per-port virtual
// host that first claims them: "<mesh name>|<what>" groups under the mesh
// name, so a service has one virtual host whatever spelling or port names it,
// and "<short name>|short|<suffix>" under the short name.
//
// When the dialled-port filter state is missing (a chain that does not set
// it), every request gets other's answer: a mesh name the node knows is
// refused, never served on a port it was not dialled on.
//
// An error means the per-port tables use a construct this compiler does not
// model (a domain wildcard other than "*" and "<name>:*", a virtual-host field
// other than its routes, matcher and per-filter config, names of one virtual
// host that one table answers differently for one suffix, a route list a
// request can fall out of). The caller must not publish the result then;
// nothing the agent builds today does that.
func MergeCaptureRouteTables(name string, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration) (*routev3.RouteConfiguration, error) {
	ports := slices.Sorted(maps.Keys(perPort))
	tables := make([]*mergeTable, 0, len(ports))
	for _, p := range ports {
		t, err := indexMergeTable(perPort[p])
		if err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	otherT, err := indexMergeTable(other)
	if err != nil {
		return nil, err
	}
	members := mergeMembers(append(slices.Clone(tables), otherT))
	out := make([]*routev3.VirtualHost, 0, len(members)+1)
	for _, g := range slices.Sorted(maps.Keys(members)) {
		vh, err := mergeGroup(g, members[g], ports, tables, otherT)
		if err != nil {
			return nil, err
		}
		out = append(out, vh)
	}
	star, err := mergeGroup("on_demand_catch_all", []string{"*"}, ports, tables, otherT)
	if err != nil {
		return nil, err
	}
	out = append(out, star)
	return &routev3.RouteConfiguration{Name: name, VirtualHosts: out}, nil
}

// mergeMembers returns, per compiled virtual host, its domains, sorted: every
// domain of every table but "*", in the group of the first table's virtual
// host to claim it.
func mergeMembers(all []*mergeTable) map[string][]string {
	group := map[string]string{}
	for _, t := range all {
		for _, vh := range t.rc.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				d = strings.ToLower(d)
				if _, ok := group[d]; !ok && d != "*" {
					group[d] = mergeGroupName(vh.GetName(), d)
				}
			}
		}
	}
	members := map[string][]string{}
	for d, g := range group {
		members[g] = append(members[g], d)
	}
	for _, doms := range members {
		slices.Sort(doms)
	}
	return members
}

// RefuseCaptureMeshNames is the table published in place of cap_http when the
// per-port tables cannot be compiled: every domain any of them claims is
// refused (421 port-not-declared), whatever was dialled, and every other Host
// gets the floor of the table of a port no service declares. No mesh name the
// node knows is delivered, and none leaves through the passthrough.
func RefuseCaptureMeshNames(name string, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration) *routev3.RouteConfiguration {
	doms := slices.Sorted(maps.Keys(mergeMembersFlat(append(slices.Collect(maps.Values(perPort)), other))))
	var star *routev3.VirtualHost
	for _, vh := range other.GetVirtualHosts() {
		if slices.Contains(vh.GetDomains(), starGroup) {
			star = vh
		}
	}
	out := &routev3.RouteConfiguration{Name: name}
	if len(doms) > 0 {
		out.VirtualHosts = append(out.VirtualHosts, BuildRefusalVirtualHost("capture_table_unbuilt", doms, RefusalPortNotDeclared,
			"aether: this node could not build its capture route table, and refuses every mesh name on the captured path until it can. The agent's log says why.\n"))
	}
	if star == nil {
		star = BuildCaptureCatchAllVirtualHost("", 0, false, false)
	}
	out.VirtualHosts = append(out.VirtualHosts, proto.Clone(star).(*routev3.VirtualHost))
	return out
}

// mergeMembersFlat returns every domain but "*" of tables, lower-cased.
func mergeMembersFlat(tables []*routev3.RouteConfiguration) map[string]bool {
	out := map[string]bool{}
	for _, rc := range tables {
		for _, vh := range rc.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				if d = strings.ToLower(d); d != starGroup {
					out[d] = true
				}
			}
		}
	}
	return out
}

// mergeGroupName is the compiled virtual host a domain belongs to: the part
// of the claiming virtual host's name before its first "|", or the domain
// without its port when the virtual host has no name.
func mergeGroupName(vhost, domain string) string {
	if g, _, _ := strings.Cut(vhost, "|"); g != "" {
		return g
	}
	g, _, _ := strings.Cut(domain, ":")
	return g
}

// mergeTable is one per-port table, indexed the way the proxy selects a
// virtual host: an exact domain, else the longest "<prefix>*" wildcard, else
// "*". Domains are compared lower-cased, as the proxy does.
type mergeTable struct {
	rc    *routev3.RouteConfiguration
	exact map[string]*routev3.VirtualHost
	// wild maps a wildcard's prefix ("<name>:") to its virtual host.
	wild map[string]*routev3.VirtualHost
	star *routev3.VirtualHost
	// answers memoizes the answer of each virtual host.
	answers map[*routev3.VirtualHost]mergeAnswer
}

type mergeAnswer struct {
	onMatch *xdsmatcherv3.Matcher_OnMatch
	key     string
}

func indexMergeTable(rc *routev3.RouteConfiguration) (*mergeTable, error) {
	if rc == nil {
		return nil, fmt.Errorf("a per-port capture table is missing")
	}
	bare := &routev3.RouteConfiguration{}
	probe := proto.Clone(rc).(*routev3.RouteConfiguration)
	probe.Name, probe.VirtualHosts = "", nil
	if !proto.Equal(probe, bare) {
		return nil, fmt.Errorf("table %s sets a route-configuration field the merge does not model", rc.GetName())
	}
	t := &mergeTable{rc: rc, exact: map[string]*routev3.VirtualHost{}, wild: map[string]*routev3.VirtualHost{}, answers: map[*routev3.VirtualHost]mergeAnswer{}}
	for _, vh := range rc.GetVirtualHosts() {
		if err := checkMergeVhost(rc.GetName(), vh); err != nil {
			return nil, err
		}
		for _, d := range vh.GetDomains() {
			d = strings.ToLower(d)
			switch {
			case d == "*":
				t.star = vh
			case strings.HasSuffix(d, ":*") && !strings.ContainsAny(strings.TrimSuffix(d, ":*"), ":*"):
				t.wild[strings.TrimSuffix(d, "*")] = vh
			case !strings.Contains(d, "*"):
				t.exact[d] = vh
			default:
				return nil, fmt.Errorf("table %s: domain %q is a wildcard the merge does not model", rc.GetName(), d)
			}
		}
	}
	if t.star == nil {
		return nil, fmt.Errorf("table %s has no \"*\" virtual host", rc.GetName())
	}
	return t, nil
}

// checkMergeVhost refuses a virtual host that sets anything the compiled
// table could not carry over.
func checkMergeVhost(table string, vh *routev3.VirtualHost) error {
	probe := proto.Clone(vh).(*routev3.VirtualHost)
	probe.Name, probe.Domains, probe.Routes, probe.Matcher, probe.TypedPerFilterConfig = "", nil, nil, nil, nil
	if !proto.Equal(probe, &routev3.VirtualHost{}) {
		return fmt.Errorf("table %s: virtual host %s sets a field the merge does not model", table, vh.GetName())
	}
	if vh.GetMatcher() != nil && (len(vh.GetRoutes()) > 0 || len(vh.GetTypedPerFilterConfig()) > 0) {
		return fmt.Errorf("table %s: virtual host %s has a matcher and routes or per-filter config", table, vh.GetName())
	}
	return nil
}

// pick returns the virtual host the proxy selects for host (lower-cased) in
// this table.
func (t *mergeTable) pick(host string) *routev3.VirtualHost {
	if vh, ok := t.exact[host]; ok {
		return vh
	}
	// A wildcard needs at least one character where its "*" stands.
	var best *routev3.VirtualHost
	bestLen := -1
	for prefix, vh := range t.wild {
		if len(host) > len(prefix) && strings.HasPrefix(host, prefix) && len(prefix) > bestLen {
			best, bestLen = vh, len(prefix)
		}
	}
	if best != nil {
		return best
	}
	return t.star
}

// answer returns what this table answers a Host that selects domain d (as a
// host: an exact domain is that host, a "<name>:*" wildcard stands for every
// host it selects in the compiled table, which no exact domain of any table
// matches, and "*" for a host no other domain matches).
func (t *mergeTable) answer(d string) (mergeAnswer, error) {
	vh := t.star
	if d != "*" {
		vh = t.pick(d)
	}
	if a, ok := t.answers[vh]; ok {
		return a, nil
	}
	var om *xdsmatcherv3.Matcher_OnMatch
	if vh.GetMatcher() != nil {
		om = &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: proto.Clone(vh.GetMatcher()).(*xdsmatcherv3.Matcher)}}
	} else {
		routes := make([]*routev3.Route, 0, len(vh.GetRoutes()))
		for _, r := range vh.GetRoutes() {
			routes = append(routes, armRoute(vh, r))
		}
		om = routeListOnMatch(routes)
	}
	a, err := newMergeAnswer(om)
	if err != nil {
		return mergeAnswer{}, err
	}
	t.answers[vh] = a
	return a, nil
}

func newMergeAnswer(om *xdsmatcherv3.Matcher_OnMatch) (mergeAnswer, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(om)
	if err != nil {
		return mergeAnswer{}, err
	}
	return mergeAnswer{onMatch: om, key: string(b)}, nil
}

// mergeGroup builds one compiled virtual host from the domains doms: a plain
// route list, one block of routes per dialled port whose answers differ from
// the other ports', each route conditioned on that dialled port, and the
// other ports' block last, unconditioned on the port.
func mergeGroup(name string, doms []string, ports []uint32, tables []*mergeTable, other *mergeTable) (*routev3.VirtualHost, error) {
	suffixes, err := groupSuffixes(doms)
	if err != nil {
		return nil, err
	}
	otherAnswers, err := suffixAnswers(name, doms, other)
	if err != nil {
		return nil, err
	}
	var routes []*routev3.Route
	for i, p := range ports {
		answers, err := suffixAnswers(name, doms, tables[i])
		if err != nil {
			return nil, err
		}
		if sameAnswers(answers, otherAnswers) {
			continue // the port gets what every other port gets
		}
		port := strconv.Itoa(int(p))
		block, err := flatBlock(answers, suffixes, []routeCond{dialledCond(port)})
		if err != nil {
			return nil, fmt.Errorf("virtual host %s, dialled port %d: %w", name, p, err)
		}
		routes = append(routes, block...)
	}
	block, err := flatBlock(otherAnswers, suffixes, nil)
	if err != nil {
		return nil, fmt.Errorf("virtual host %s, any other port: %w", name, err)
	}
	return &routev3.VirtualHost{Name: name, Domains: mergeDomains(doms), Routes: append(routes, block...)}, nil
}

// A group's domains are its names, each with a SUFFIX: "" (no port), ":<P>"
// or ":*". The virtual host's domains already restrict a Host to the group's
// names, so a route need only tell suffixes apart, which a small regex on
// :authority does. That is right only if every name of the group gets the
// same answer for the same suffix in every table; suffixAnswers checks it.
const (
	suffixNone = ""
	suffixAny  = ":*"
	starGroup  = "*"
)

func domainSuffix(d string) string {
	if d == starGroup {
		return starGroup
	}
	if i := strings.IndexByte(d, ':'); i >= 0 {
		return d[i:]
	}
	return suffixNone
}

// groupSuffixes returns the suffixes of doms, sorted.
func groupSuffixes(doms []string) ([]string, error) {
	seen := map[string]bool{}
	for _, d := range doms {
		s := domainSuffix(d)
		if s != suffixNone && s != suffixAny && s != starGroup {
			if _, err := strconv.ParseUint(s[1:], 10, 16); err != nil {
				return nil, fmt.Errorf("domain %q has a suffix the merge does not model", d)
			}
		}
		seen[s] = true
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// suffixAnswers returns, per suffix, what t answers the group's domains with
// that suffix, and refuses a group whose names disagree on one.
func suffixAnswers(group string, doms []string, t *mergeTable) (map[string]mergeAnswer, error) {
	out := map[string]mergeAnswer{}
	for _, d := range doms {
		a, err := t.answer(d)
		if err != nil {
			return nil, err
		}
		s := domainSuffix(d)
		if prev, ok := out[s]; ok && prev.key != a.key {
			return nil, fmt.Errorf("table %s, virtual host %s: two names answer the suffix %q differently", t.rc.GetName(), group, s)
		}
		out[s] = a
	}
	return out, nil
}

func sameAnswers(a, b map[string]mergeAnswer) bool {
	if len(a) != len(b) {
		return false
	}
	for s, x := range a {
		if y, ok := b[s]; !ok || y.key != x.key {
			return false
		}
	}
	return true
}

// flatBlock is one table's answers for the group as routes: every answer but
// the default one conditioned on the :authority suffixes it is for, then the
// default, each route also under conds. The default is the answer of ":*"
// when the group has it (it stands for every port spelling no exact domain
// names, which no regex lists), else the answer most suffixes get.
func flatBlock(answers map[string]mergeAnswer, suffixes []string, conds []routeCond) ([]*routev3.Route, error) {
	bySuffix := map[string][]string{}
	var keys []string
	for _, s := range suffixes {
		k := answers[s].key
		if _, ok := bySuffix[k]; !ok {
			keys = append(keys, k)
		}
		bySuffix[k] = append(bySuffix[k], s)
	}
	def := defaultAnswer(keys, bySuffix)
	var out []*routev3.Route
	for _, k := range keys {
		if k == def {
			continue
		}
		auth, err := authorityCond(bySuffix[k])
		if err != nil {
			return nil, err
		}
		routes, err := flattenOnMatch(answers[bySuffix[k][0]].onMatch, append(slices.Clone(conds), auth))
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	routes, err := flattenOnMatch(answers[bySuffix[def][0]].onMatch, conds)
	if err != nil {
		return nil, err
	}
	return append(out, routes...), nil
}

// defaultAnswer is the answer of ":*" when there is one, else the one most
// suffixes get (the first on a tie).
func defaultAnswer(keys []string, bySuffix map[string][]string) string {
	def := keys[0]
	for _, k := range keys {
		if slices.Contains(bySuffix[k], suffixAny) {
			return k
		}
		if len(bySuffix[k]) > len(bySuffix[def]) {
			def = k
		}
	}
	return def
}

// routeCond is one condition added to a route's match: a filter-state value,
// or a request header.
type routeCond struct {
	filterState *typematcherv3.FilterStateMatcher
	header      *routev3.HeaderMatcher
}

func (c routeCond) apply(r *routev3.Route) {
	if c.filterState != nil {
		r.Match.FilterState = append(r.Match.FilterState, proto.Clone(c.filterState).(*typematcherv3.FilterStateMatcher))
	}
	if c.header != nil {
		r.Match.Headers = append(r.Match.Headers, proto.Clone(c.header).(*routev3.HeaderMatcher))
	}
}

// dialledCond: the connection was dialled on port.
func dialledCond(port string) routeCond {
	return routeCond{filterState: &typematcherv3.FilterStateMatcher{
		Key:     DialledPortFilterStateKey,
		Matcher: &typematcherv3.FilterStateMatcher_StringMatch{StringMatch: &typematcherv3.StringMatcher{MatchPattern: &typematcherv3.StringMatcher_Exact{Exact: port}}},
	}}
}

// authorityCond: the Host's suffix is one of suffixes (never ":*"). The name
// before it is any: the virtual host's domains have already chosen it. One
// port at most: "<name>:8080:8080" has no suffix the regex accepts.
func authorityCond(suffixes []string) (routeCond, error) {
	portless := false
	var ports []string
	for _, s := range suffixes {
		switch s {
		case suffixNone:
			portless = true
		case suffixAny, starGroup:
			return routeCond{}, fmt.Errorf("the suffix %q cannot be told apart by a route", s)
		default:
			ports = append(ports, s[1:])
		}
	}
	var re string
	switch {
	case portless && len(ports) > 0:
		re = "^[^:]+(:(" + strings.Join(ports, "|") + "))?$"
	case portless:
		re = "^[^:]+$"
	default:
		re = "^[^:]+:(" + strings.Join(ports, "|") + ")$"
	}
	return routeCond{header: &routev3.HeaderMatcher{
		Name: ":authority",
		HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: &typematcherv3.StringMatcher{
			MatchPattern: &typematcherv3.StringMatcher_SafeRegex{SafeRegex: &typematcherv3.RegexMatcher{Regex: re}},
		}},
	}}, nil
}

// flattenOnMatch turns an answer into routes under conds. A RouteList answer
// is its routes. A matcher answer (a short name's, by the caller's namespace)
// is the routes of each of its branches under that branch's condition, in
// the matcher's order, then its on_no_match's. That is the matcher's answer
// only because every route list ends in a route every request takes
// (requireTotal): a request that a branch's condition takes never falls
// through to a later branch.
func flattenOnMatch(om *xdsmatcherv3.Matcher_OnMatch, conds []routeCond) ([]*routev3.Route, error) {
	switch {
	case om == nil:
		return nil, nil
	case om.GetMatcher() != nil:
		return flattenMatcher(om.GetMatcher(), conds)
	case om.GetAction() != nil:
		list := &routev3.RouteList{}
		if err := om.GetAction().GetTypedConfig().UnmarshalTo(list); err != nil {
			return nil, err
		}
		if err := requireTotal(list.GetRoutes()); err != nil {
			return nil, err
		}
		out := make([]*routev3.Route, 0, len(list.GetRoutes()))
		for _, r := range list.GetRoutes() {
			c := proto.Clone(r).(*routev3.Route)
			if c.Match == nil {
				c.Match = &routev3.RouteMatch{}
			}
			for _, cond := range conds {
				cond.apply(c)
			}
			out = append(out, c)
		}
		return out, nil
	}
	return nil, fmt.Errorf("a matcher branch that is neither a matcher nor an action")
}

func flattenMatcher(m *xdsmatcherv3.Matcher, conds []routeCond) ([]*routev3.Route, error) {
	var out []*routev3.Route
	add := func(om *xdsmatcherv3.Matcher_OnMatch, more ...routeCond) error {
		routes, err := flattenOnMatch(om, append(slices.Clone(conds), more...))
		out = append(out, routes...)
		return err
	}
	branches, err := matcherBranches(m)
	if err != nil {
		return nil, err
	}
	for _, b := range branches {
		if err := add(b.onMatch, b.cond); err != nil {
			return nil, err
		}
	}
	if err := add(m.GetOnNoMatch()); err != nil {
		return nil, err
	}
	return out, nil
}

// matcherBranch is one branch of a matcher: its condition and its on_match.
type matcherBranch struct {
	cond    routeCond
	onMatch *xdsmatcherv3.Matcher_OnMatch
}

// matcherBranches lists a matcher's branches in the order it tries them: a
// list's field matchers, or an exact map's keys (disjoint, so in any order).
func matcherBranches(m *xdsmatcherv3.Matcher) ([]matcherBranch, error) {
	var out []matcherBranch
	switch {
	case m.GetMatcherList() != nil:
		for _, fm := range m.GetMatcherList().GetMatchers() {
			cond, err := filterStatePredicate(fm.GetPredicate())
			if err != nil {
				return nil, err
			}
			out = append(out, matcherBranch{cond: cond, onMatch: fm.GetOnMatch()})
		}
	case m.GetMatcherTree().GetExactMatchMap() != nil:
		key, err := filterStateKey(m.GetMatcherTree().GetInput())
		if err != nil {
			return nil, err
		}
		branches := m.GetMatcherTree().GetExactMatchMap().GetMap()
		for _, v := range slices.Sorted(maps.Keys(branches)) {
			out = append(out, matcherBranch{onMatch: branches[v], cond: routeCond{filterState: &typematcherv3.FilterStateMatcher{
				Key:     key,
				Matcher: &typematcherv3.FilterStateMatcher_StringMatch{StringMatch: &typematcherv3.StringMatcher{MatchPattern: &typematcherv3.StringMatcher_Exact{Exact: v}}},
			}}})
		}
	default:
		return nil, fmt.Errorf("a matcher the merge does not model")
	}
	return out, nil
}

func filterStateKey(input interface {
	GetTypedConfig() *anypb.Any
},
) (string, error) {
	in := &network_inputsv3.FilterStateInput{}
	if err := input.GetTypedConfig().UnmarshalTo(in); err != nil {
		return "", fmt.Errorf("a matcher input other than filter state: %w", err)
	}
	return in.GetKey(), nil
}

// filterStatePredicate translates a single filter-state predicate with a
// string match (a short name's "is the caller's namespace there").
func filterStatePredicate(p *xdsmatcherv3.Matcher_MatcherList_Predicate) (routeCond, error) {
	sp := p.GetSinglePredicate()
	if sp == nil || sp.GetValueMatch() == nil {
		return routeCond{}, fmt.Errorf("a matcher predicate the merge does not model")
	}
	key, err := filterStateKey(sp.GetInput())
	if err != nil {
		return routeCond{}, err
	}
	sm := &typematcherv3.StringMatcher{IgnoreCase: sp.GetValueMatch().GetIgnoreCase()}
	switch v := sp.GetValueMatch().GetMatchPattern().(type) {
	case *xdsmatcherv3.StringMatcher_Exact:
		sm.MatchPattern = &typematcherv3.StringMatcher_Exact{Exact: v.Exact}
	case *xdsmatcherv3.StringMatcher_SafeRegex:
		sm.MatchPattern = &typematcherv3.StringMatcher_SafeRegex{SafeRegex: &typematcherv3.RegexMatcher{Regex: v.SafeRegex.GetRegex()}}
	default:
		return routeCond{}, fmt.Errorf("a string match the merge does not model")
	}
	return routeCond{filterState: &typematcherv3.FilterStateMatcher{Key: key, Matcher: &typematcherv3.FilterStateMatcher_StringMatch{StringMatch: sm}}}, nil
}

// requireTotal refuses a route list whose last route some request could fail
// to match: it must be a "/" prefix with no other condition. Every list the
// table builders make ends so (a service's fallback, a refusal, a floor's
// fallthrough).
func requireTotal(routes []*routev3.Route) error {
	if len(routes) == 0 {
		return fmt.Errorf("an empty route list")
	}
	m := routes[len(routes)-1].GetMatch()
	bare := proto.Clone(m).(*routev3.RouteMatch)
	bare.PathSpecifier = nil
	if m.GetPrefix() != "/" || !proto.Equal(bare, &routev3.RouteMatch{}) {
		return fmt.Errorf("a route list whose last route is not taken by every request: %v", m)
	}
	return nil
}

// mergeDomains returns the compiled virtual host's domains: doms without the
// exact "<name>:<port>" spellings a "<name>:*" of the same virtual host
// already selects (its routes tell them apart).
func mergeDomains(doms []string) []string {
	wild := map[string]bool{}
	for _, d := range doms {
		if prefix, ok := strings.CutSuffix(d, "*"); ok && d != "*" {
			wild[prefix] = true
		}
	}
	out := make([]string, 0, len(doms))
	for _, d := range doms {
		if i := strings.IndexByte(d, ':'); i >= 0 && !strings.HasSuffix(d, "*") && wild[d[:i+1]] {
			continue
		}
		out = append(out, d)
	}
	return out
}
