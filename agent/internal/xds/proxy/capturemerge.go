package proxy

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"aethermesh.dev/agent/internal/xds/config"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
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
//   - that virtual host's matcher branches on the dialled port (filter state,
//     DialledPortFilterStateKey) and, inside each branch, on the Host itself
//     (:authority, compared case-insensitively as the proxy compares domains),
//     to the answer the dialled port's table gives that Host.
//
// The Host-level branch is needed because one compiled virtual host stands for
// several domains that one per-port table may answer differently: in the table
// of port 8080, "<name>:8080" is routed and "<name>:9090" is a port mismatch,
// and both are reached through the compiled table's "<name>:*". The branch
// names exact spellings, so "<name>:8080:8080" is a mismatch as it was.
//
// An answer is the per-port virtual host's routes, with that virtual host's
// typed_per_filter_config in force on each route (a service-wide HTTP filter
// is enabled there, and the compiled virtual host also answers the refusals,
// which must not run it), or its matcher (a short name's caller-namespace
// matcher), unchanged.
//
// Domains are grouped into virtual hosts by the name of the per-port virtual
// host that first claims them: "<mesh name>|<what>" groups under the mesh
// name, so a service has one virtual host whatever spelling or port names it,
// and "<short name>|short|<suffix>" under the short name. The grouping decides
// nothing about answers, only how many virtual hosts there are.
//
// When the dialled-port filter state is missing (a chain that does not set
// it), every request gets other's answer: a mesh name the node knows is
// refused, never served on a port it was not dialled on.
//
// An error means the per-port tables use a construct this compiler does not
// model (a domain wildcard other than "*" and "<name>:*", a virtual-host field
// other than its routes, matcher and per-filter config). The caller must not
// publish a table then; nothing the agent builds today does that.
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

// mergeGroup builds one compiled virtual host from the domains doms.
func mergeGroup(name string, doms []string, ports []uint32, tables []*mergeTable, other *mergeTable) (*routev3.VirtualHost, error) {
	otherAnswer, err := mergeAuthority(doms, other)
	if err != nil {
		return nil, err
	}
	byPort := map[string]*xdsmatcherv3.Matcher_OnMatch{}
	for i, p := range ports {
		a, err := mergeAuthority(doms, tables[i])
		if err != nil {
			return nil, err
		}
		if a.key != otherAnswer.key {
			byPort[strconv.Itoa(int(p))] = a.onMatch
		}
	}
	vh := &routev3.VirtualHost{Name: name, Domains: mergeDomains(doms)}
	if len(byPort) == 0 {
		// Every dialled port gets one answer.
		if m := otherAnswer.onMatch.GetMatcher(); m != nil {
			vh.Matcher = m
		} else {
			list := &routev3.RouteList{}
			if err := otherAnswer.onMatch.GetAction().GetTypedConfig().UnmarshalTo(list); err != nil {
				return nil, err
			}
			vh.Routes = list.GetRoutes()
		}
		return vh, nil
	}
	vh.Matcher = &xdsmatcherv3.Matcher{
		MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
			Input: &xdscorev3.TypedExtensionConfig{
				Name:        dialledPortInputName,
				TypedConfig: config.TypedConfig(&network_inputsv3.FilterStateInput{Key: DialledPortFilterStateKey}),
			},
			TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: byPort}},
		}},
		OnNoMatch: otherAnswer.onMatch,
	}
	return vh, nil
}

const (
	dialledPortInputName = "aether.matching.inputs.dialled_port"
	authorityInputName   = "aether.matching.inputs.authority"
)

// mergeAuthority is what one table answers for the domains doms, as one
// matcher branch: a single answer when the table answers them all alike, else
// a list that names the spellings of every answer but the most common one,
// which is the list's on_no_match.
func mergeAuthority(doms []string, t *mergeTable) (mergeAnswer, error) {
	classes, err := answerClasses(doms, t)
	if err != nil {
		return mergeAnswer{}, err
	}
	if len(classes) == 1 {
		return classes[0].answer, nil
	}
	// The class with the most spellings answers the rest; the first such
	// class on a tie, in domain order.
	def := classes[0]
	for _, c := range classes[1:] {
		if len(c.doms) > len(def.doms) {
			def = c
		}
	}
	return newMergeAnswer(&xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: &xdsmatcherv3.Matcher{
		MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{Matchers: authorityList(classes, def)}},
		OnNoMatch:   def.answer.onMatch,
	}}})
}

// answerClass is the domains a table answers alike.
type answerClass struct {
	answer mergeAnswer
	doms   []string
}

// answerClasses groups doms by what t answers them, in domain order.
func answerClasses(doms []string, t *mergeTable) ([]*answerClass, error) {
	var classes []*answerClass
	byKey := map[string]*answerClass{}
	for _, d := range doms {
		a, err := t.answer(d)
		if err != nil {
			return nil, err
		}
		c, ok := byKey[a.key]
		if !ok {
			c = &answerClass{answer: a}
			byKey[a.key] = c
			classes = append(classes, c)
		}
		c.doms = append(c.doms, d)
	}
	return classes, nil
}

// authorityList is the field matchers of mergeAuthority's list. It is
// first-match, and an exact spelling must be tested before any "<name>:"
// prefix that would also take it: so the exact spellings of every class come
// first, including those of the on_no_match class (def) that a prefix of
// another class would take, and the prefixes last.
func authorityList(classes []*answerClass, def *answerClass) []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher {
	var prefixes []string
	for _, c := range classes {
		if c != def {
			prefixes = append(prefixes, wildcardPrefixes(c.doms)...)
		}
	}
	input := &xdscorev3.TypedExtensionConfig{
		Name:        authorityInputName,
		TypedConfig: config.TypedConfig(&typematcherv3.HttpRequestHeaderMatchInput{HeaderName: ":authority"}),
	}
	var list []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher
	for _, wildcards := range []bool{false, true} {
		for _, c := range classes {
			if fm := authorityField(input, c, wildcards, c == def, prefixes); fm != nil {
				list = append(list, fm)
			}
		}
	}
	return list
}

// authorityField is the field matcher of one class: its exact spellings, or
// its "<name>:*" ones. The on_no_match class (isDef) is listed only for the
// exact spellings a prefix of another class would take, and never for its
// wildcards. nil when there is nothing to list.
func authorityField(input *xdscorev3.TypedExtensionConfig, c *answerClass, wildcards, isDef bool, prefixes []string) *xdsmatcherv3.Matcher_MatcherList_FieldMatcher {
	if isDef && wildcards {
		return nil
	}
	var doms []string
	for _, d := range c.doms {
		if strings.HasSuffix(d, "*") != wildcards || (isDef && !hasAnyPrefix(d, prefixes)) {
			continue
		}
		doms = append(doms, d)
	}
	if len(doms) == 0 {
		return nil
	}
	return &xdsmatcherv3.Matcher_MatcherList_FieldMatcher{Predicate: authorityPredicate(input, doms), OnMatch: c.answer.onMatch}
}

// wildcardPrefixes returns the "<name>:" of every "<name>:*" in doms.
func wildcardPrefixes(doms []string) []string {
	var out []string
	for _, d := range doms {
		if p, wild := strings.CutSuffix(d, "*"); wild {
			out = append(out, p)
		}
	}
	return out
}

func hasAnyPrefix(d string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(d, p) {
			return true
		}
	}
	return false
}

// authorityPredicate matches a Host that selects one of doms: an exact domain
// is that Host, and "<name>:*" every Host that starts "<name>:" and matched no
// exact domain (those are tested first, by the class that holds them, or are
// the on_no_match class).
func authorityPredicate(input *xdscorev3.TypedExtensionConfig, doms []string) *xdsmatcherv3.Matcher_MatcherList_Predicate {
	preds := make([]*xdsmatcherv3.Matcher_MatcherList_Predicate, 0, len(doms))
	for _, d := range doms {
		sm := &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: d}, IgnoreCase: true}
		if prefix, wild := strings.CutSuffix(d, "*"); wild {
			sm = &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Prefix{Prefix: prefix}, IgnoreCase: true}
		}
		preds = append(preds, singlePredicate(input, sm))
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_OrMatcher{
		OrMatcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_PredicateList{Predicate: preds},
	}}
}

// mergeDomains returns the compiled virtual host's domains: doms without the
// exact "<name>:<port>" spellings a "<name>:*" of the same virtual host
// already selects (the virtual host's matcher tells them apart).
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
