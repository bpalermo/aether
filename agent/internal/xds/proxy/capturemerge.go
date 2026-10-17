package proxy

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	meshconst "aethermesh.dev/common/constants/mesh"
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
// port D gets, for every HTTP port number D of the mesh and for the mesh's
// HTTP proxy port (18081, whose table is the loopback table's virtual hosts,
// BuildProxyPortCaptureRouteConfiguration: the port in Host decides there),
// and other is what a request dialled to any other port gets. The compiled
// table must give every request the answer the table of its dialled port
// gives it, and does so by construction:
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
// The order of the dialled-port blocks is therefore free: their conditions
// are disjoint (one dialled port each), and each ends in a route every request
// of its port takes. Only the other ports' block, which matches no dialled
// port, must come after all of them. The proxy port's block is placed after
// the declared ports' (mergePortOrder), so that a request to a service's own
// port, the common case, is matched against fewer routes first.
//
// An answer is the per-port virtual host's routes, or, for a short name, its
// caller-namespace matcher flattened: each namespace's routes under a
// filter-state match on the caller's namespace, then what a caller of no such
// namespace gets, then what a connection with no namespace gets.
//
// A per-port virtual host's typed_per_filter_config (a service-wide HTTP
// filter is enabled there) stays a VIRTUAL-HOST config: the compiled virtual
// host of a service carries it, so the proxy combines it with a route's own
// config exactly as it does on the per-port table (and on the proxy port).
// The routes of that compiled virtual host that come from an answer without
// it (a refusal) disable each of its filters (route-level FilterConfig
// disabled: the route's own answer to "is this filter disabled" wins over the
// virtual host's, source/common/router/config_impl.cc,
// RouteEntryImplBase::filterDisabled, and a disabled filter is not created
// for the stream, source/common/http/filter_chain_helper.cc), so a refusal
// runs none of them, as on the per-port table. Only when that cannot be said
// (two answers of one compiled virtual host with different virtual-host
// configs, or a route without the virtual-host config that sets one of its
// filters itself) is the virtual-host config folded into each route instead
// (extensionfilter.CombineLevels, which refuses a route it cannot fold
// exactly); no table the agent builds does that today.
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
	ports := mergePortOrder(slices.Collect(maps.Keys(perPort)))
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

// mergePortOrder is the order of the dialled-port blocks of a compiled
// virtual host: the ports in ascending order, the mesh's HTTP proxy port
// last. Any order gives every request the same answer (see
// MergeCaptureRouteTables); this one puts a service's own ports first.
func mergePortOrder(ports []uint32) []uint32 {
	slices.SortFunc(ports, func(a, b uint32) int {
		aProxy, bProxy := a == meshconst.ProxyOutboundPort, b == meshconst.ProxyOutboundPort
		switch {
		case aProxy && !bProxy:
			return 1
		case bProxy && !aProxy:
			return -1
		}
		return cmp.Compare(a, b)
	})
	return ports
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
	// onMatch is the answer's routes (or matcher) as the per-port table has
	// them; armed has the virtual host's config folded into each route.
	onMatch, armed *xdsmatcherv3.Matcher_OnMatch
	// cfg is the per-port virtual host's typed_per_filter_config.
	cfg map[string]*anypb.Any
	key string
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
	var om, armed *xdsmatcherv3.Matcher_OnMatch
	if vh.GetMatcher() != nil {
		om = &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: proto.Clone(vh.GetMatcher()).(*xdsmatcherv3.Matcher)}}
		armed = om
	} else {
		raw := make([]*routev3.Route, 0, len(vh.GetRoutes()))
		folded := make([]*routev3.Route, 0, len(vh.GetRoutes()))
		for _, r := range vh.GetRoutes() {
			raw = append(raw, proto.Clone(r).(*routev3.Route))
			folded = append(folded, armRoute(vh, r))
		}
		om, armed = routeListOnMatch(raw), routeListOnMatch(folded)
	}
	a, err := newMergeAnswer(om, armed, vh.GetTypedPerFilterConfig())
	if err != nil {
		return mergeAnswer{}, err
	}
	t.answers[vh] = a
	return a, nil
}

func newMergeAnswer(om, armed *xdsmatcherv3.Matcher_OnMatch, cfg map[string]*anypb.Any) (mergeAnswer, error) {
	det := proto.MarshalOptions{Deterministic: true}
	b, err := det.Marshal(om)
	if err != nil {
		return mergeAnswer{}, err
	}
	c, err := det.Marshal(&routev3.VirtualHost{TypedPerFilterConfig: cfg})
	if err != nil {
		return mergeAnswer{}, err
	}
	return mergeAnswer{onMatch: om, armed: armed, cfg: cfg, key: string(b) + "\x00" + string(c)}, nil
}

// groupLevels says where a compiled virtual host keeps a service-wide
// filter's config: on the virtual host (native: cfg, and the routes of an
// answer without it disable its filters), or folded into every route.
type groupLevels struct {
	native bool
	cfg    map[string]*anypb.Any
}

// levelsOf decides groupLevels for the answers of one compiled virtual host.
func levelsOf(answers []mergeAnswer) groupLevels {
	var cfg map[string]*anypb.Any
	for _, a := range answers {
		if len(a.cfg) == 0 {
			continue
		}
		if cfg != nil && !sameFilterConfigs(cfg, a.cfg) {
			return groupLevels{}
		}
		cfg = a.cfg
	}
	for _, a := range answers {
		if len(a.cfg) == 0 && !leavesFiltersAlone(a.onMatch, cfg) {
			return groupLevels{}
		}
	}
	return groupLevels{native: true, cfg: cfg}
}

// leavesFiltersAlone: no route of om configures a filter of cfg itself (a
// matcher answer is not looked into, and counts as configuring them).
func leavesFiltersAlone(om *xdsmatcherv3.Matcher_OnMatch, cfg map[string]*anypb.Any) bool {
	if len(cfg) == 0 {
		return true
	}
	list := &routev3.RouteList{}
	if om.GetAction() == nil || om.GetAction().GetTypedConfig().UnmarshalTo(list) != nil {
		return false
	}
	for _, r := range list.GetRoutes() {
		for name := range cfg {
			if _, ok := r.GetTypedPerFilterConfig()[name]; ok {
				return false
			}
		}
	}
	return true
}

func sameFilterConfigs(a, b map[string]*anypb.Any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !proto.Equal(v, w) {
			return false
		}
	}
	return true
}

// routes is answer a as routes of the compiled virtual host, under conds.
func (g groupLevels) routes(a mergeAnswer, conds []routeCond) ([]*routev3.Route, error) {
	if !g.native {
		return flattenOnMatch(a.armed, conds)
	}
	out, err := flattenOnMatch(a.onMatch, conds)
	if err != nil || len(a.cfg) > 0 || len(g.cfg) == 0 {
		return out, err
	}
	for _, r := range out {
		if r.TypedPerFilterConfig == nil {
			r.TypedPerFilterConfig = map[string]*anypb.Any{}
		}
		for name := range g.cfg {
			r.TypedPerFilterConfig[name] = disabledFilterConfig
		}
	}
	return out, nil
}

// disabledFilterConfig is a route-level per-filter entry that disables the
// filter on that route (envoy.config.route.v3.FilterConfig, disabled).
var disabledFilterConfig = func() *anypb.Any {
	a := &anypb.Any{}
	if err := anypb.MarshalFrom(a, &routev3.FilterConfig{Disabled: true}, proto.MarshalOptions{Deterministic: true}); err != nil {
		panic(err)
	}
	return a
}()

// mergeGroup builds one compiled virtual host from the domains doms: a plain
// route list, one block of routes per dialled port whose answers differ from
// the other ports', each route conditioned on that dialled port, and the
// other ports' block last, unconditioned on the port.
func mergeGroup(name string, doms []string, ports []uint32, tables []*mergeTable, other *mergeTable) (*routev3.VirtualHost, error) {
	if _, err := groupSuffixes(doms); err != nil {
		return nil, err
	}
	otherAnswers, err := nameAnswers(doms, other)
	if err != nil {
		return nil, err
	}
	portAnswers := make([]groupAnswers, len(ports))
	all := otherAnswers.all()
	for i := range ports {
		if portAnswers[i], err = nameAnswers(doms, tables[i]); err != nil {
			return nil, err
		}
		all = append(all, portAnswers[i].all()...)
	}
	levels := levelsOf(all)
	var routes []*routev3.Route
	for i, p := range ports {
		if sameAnswers(portAnswers[i], otherAnswers) {
			continue // the port gets what every other port gets
		}
		block, err := flatBlock(levels, portAnswers[i], []routeCond{dialledCond(strconv.Itoa(int(p)))})
		if err != nil {
			return nil, fmt.Errorf("virtual host %s, dialled port %d: %w", name, p, err)
		}
		routes = append(routes, block...)
	}
	block, err := flatBlock(levels, otherAnswers, nil)
	if err != nil {
		return nil, fmt.Errorf("virtual host %s, any other port: %w", name, err)
	}
	return &routev3.VirtualHost{Name: name, Domains: mergeDomains(doms), Routes: append(routes, block...), TypedPerFilterConfig: levels.cfg}, nil
}

// A group's domains are its names, each with a SUFFIX: "" (no port), ":<P>"
// or ":*". The virtual host's domains already restrict a Host to the group's
// names, so a route need only tell suffixes apart, which a small regex on
// :authority does, as long as every name of the group gets the same answer
// for the same suffix in the table of the block (one CLASS of names,
// nameClasses). Where the names of one table do not (the proxy port's table
// answers a mesh name by the port in Host, and its other names as any Host
// that is no mesh name), each class but the largest is told apart by the
// name itself as well (nameCond), and the largest takes what is left.
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

// groupAnswers is what one table answers the domains of one compiled virtual
// host: per name (a domain without its suffix), per suffix.
type groupAnswers map[string]map[string]mergeAnswer

// nameAnswers returns what t answers each of the group's domains.
func nameAnswers(doms []string, t *mergeTable) (groupAnswers, error) {
	out := groupAnswers{}
	for _, d := range doms {
		a, err := t.answer(d)
		if err != nil {
			return nil, err
		}
		n := domainName(d)
		if out[n] == nil {
			out[n] = map[string]mergeAnswer{}
		}
		out[n][domainSuffix(d)] = a
	}
	return out, nil
}

// domainName is a domain without its suffix ("*" is its own name).
func domainName(d string) string {
	n, _, _ := strings.Cut(d, ":")
	return n
}

// all returns every answer of g.
func (g groupAnswers) all() []mergeAnswer {
	var out []mergeAnswer
	for _, n := range slices.Sorted(maps.Keys(g)) {
		for _, s := range slices.Sorted(maps.Keys(g[n])) {
			out = append(out, g[n][s])
		}
	}
	return out
}

func sameAnswers(a, b groupAnswers) bool {
	if len(a) != len(b) {
		return false
	}
	for n, x := range a {
		y, ok := b[n]
		if !ok || len(x) != len(y) {
			return false
		}
		for s, ax := range x {
			if ay, ok := y[s]; !ok || ay.key != ax.key {
				return false
			}
		}
	}
	return true
}

// nameClass is a set of names of a group that one block of routes, told
// apart by suffix alone, answers exactly: merged is the answer of each suffix
// any of them has.
type nameClass struct {
	names  []string
	merged map[string]mergeAnswer
}

// nameClasses partitions the names of g, in name order, each into the first
// class it agrees with (fits), a new one otherwise. One class is the common
// case: every per-port table answers all the names of a service alike.
func nameClasses(g groupAnswers) []nameClass {
	var out []nameClass
	for _, n := range slices.Sorted(maps.Keys(g)) {
		placed := false
		for i := range out {
			if out[i].fits(g, n) {
				out[i].names = append(out[i].names, n)
				for s, a := range g[n] {
					out[i].merged[s] = a
				}
				placed = true
				break
			}
		}
		if !placed {
			out = append(out, nameClass{names: []string{n}, merged: maps.Clone(g[n])})
		}
	}
	return out
}

// fits reports whether name n can join the class: a block that tells
// suffixes apart and nothing else then still gives every Host of every name
// of the class the answer of its table. It must, for a Host of n:
//
//   - "<n><s>" with s a suffix of n: the class's answer of s is n's;
//   - "<n>:<P>" that only n's ":*" selects (":<P>" is a suffix of the class,
//     not of n): the class's answer of ":<P>" is n's answer of ":*";
//
// and the same for every Host of a name already in the class, with n's
// suffixes added.
func (c nameClass) fits(g groupAnswers, n string) bool {
	own := g[n]
	for s, a := range own {
		if b, ok := c.merged[s]; ok && b.key != a.key {
			return false
		}
	}
	if !wildcardAgrees(own, c.merged) {
		return false
	}
	for _, m := range c.names {
		if !wildcardAgrees(g[m], own) {
			return false
		}
	}
	return true
}

// wildcardAgrees: every port suffix of others that own does not have is
// answered as own's ":*" answers it (vacuous when own has no ":*": no Host of
// that name reaches the virtual host with such a suffix).
func wildcardAgrees(own, others map[string]mergeAnswer) bool {
	w, ok := own[suffixAny]
	if !ok {
		return true
	}
	for s, b := range others {
		if s == suffixNone || s == suffixAny {
			continue
		}
		if _, has := own[s]; !has && b.key != w.key {
			return false
		}
	}
	return true
}

// flatBlock is one table's answers for the group as routes, each under conds.
// One class of names (nameClasses): suffixBlock. Several: each name of every
// class but the largest gets its own answers under a condition on the name
// (nameCond), and the largest class's suffixBlock follows, with no such
// condition unless one of its routes could take a request with no path
// (takesAPathlessRequest). Every name's routes end in a route each of its
// Hosts takes with a path, so a Host never reaches the routes of another name.
func flatBlock(levels groupLevels, answers groupAnswers, conds []routeCond) ([]*routev3.Route, error) {
	classes := nameClasses(answers)
	largest := 0
	for i, c := range classes {
		if len(c.names) > len(classes[largest].names) {
			largest = i
		}
	}
	var out []*routev3.Route
	for i, c := range classes {
		if i == largest {
			continue
		}
		routes, err := namesBlock(levels, c.names, answers, conds)
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	routes, err := suffixBlock(levels, classes[largest].merged, conds)
	if err != nil {
		return nil, err
	}
	if len(classes) > 1 && slices.ContainsFunc(routes, takesAPathlessRequest) {
		// A request with no path (a CONNECT without one; any other path that
		// does not begin with "/" the connection manager answers 404 before
		// routing) is taken by no "/"-prefixed route, so it falls out of its
		// own name's routes. Unconditioned on the name, a route of the largest
		// class that takes it would give it another name's answer, where its
		// own table gives it none. The largest class is then conditioned on
		// its names as well.
		if routes, err = namesBlock(levels, classes[largest].names, answers, conds); err != nil {
			return nil, err
		}
	}
	return append(out, routes...), nil
}

// namesBlock is nameBlock for each of names, in order.
func namesBlock(levels groupLevels, names []string, answers groupAnswers, conds []routeCond) ([]*routev3.Route, error) {
	var out []*routev3.Route
	for _, n := range names {
		routes, err := nameBlock(levels, n, answers[n], conds)
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	return out, nil
}

// takesAPathlessRequest: the route's path match can take a request with an
// empty path (a regex, an empty prefix, a CONNECT matcher, a path-match
// extension). A non-empty prefix, an exact path and a separated prefix
// cannot. Conservative: a regex that never matches "" counts.
func takesAPathlessRequest(r *routev3.Route) bool {
	m := r.GetMatch()
	return m.GetPrefix() == "" && m.GetPath() == "" && m.GetPathSeparatedPrefix() == ""
}

// nameBlock is one name's own answers as routes under conds and a condition
// on the name: its port-less answer for a Host that is exactly the name, and
// a suffixBlock of the rest for a Host "<name>:...".
func nameBlock(levels groupLevels, n string, own map[string]mergeAnswer, conds []routeCond) ([]*routev3.Route, error) {
	var out []*routev3.Route
	if a, ok := own[suffixNone]; ok {
		routes, err := levels.routes(a, append(slices.Clone(conds), nameCond(n, false)))
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	ported := maps.Clone(own)
	delete(ported, suffixNone)
	if len(ported) == 0 {
		return out, nil
	}
	routes, err := suffixBlock(levels, ported, append(slices.Clone(conds), nameCond(n, true)))
	if err != nil {
		return nil, err
	}
	return append(out, routes...), nil
}

// nameCond: the Host's name is n, with no port (ported false: the Host is
// exactly n) or with one (ported: it begins "<n>:"). Names hold no ":", so no
// Host meets the condition of two names. Not a regex: a name is as long as a
// DNS name, and the proxy refuses a regex of more than 100 instructions.
func nameCond(n string, ported bool) routeCond {
	sm := &typematcherv3.StringMatcher{MatchPattern: &typematcherv3.StringMatcher_Exact{Exact: n}, IgnoreCase: true}
	if ported {
		sm = &typematcherv3.StringMatcher{MatchPattern: &typematcherv3.StringMatcher_Prefix{Prefix: n + ":"}, IgnoreCase: true}
	}
	return routeCond{header: &routev3.HeaderMatcher{
		Name:                 ":authority",
		HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: sm},
	}}
}

// suffixBlock is the answers of one class of names as routes: every answer
// but the default one conditioned on the :authority suffixes it is for, then
// the default, each route also under conds. The default is the answer of
// ":*" when the class has it (it stands for every port spelling no exact
// domain names, which no regex lists), else the answer most suffixes get.
func suffixBlock(levels groupLevels, answers map[string]mergeAnswer, conds []routeCond) ([]*routev3.Route, error) {
	suffixes := slices.Sorted(maps.Keys(answers))
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
		routes, err := levels.routes(answers[bySuffix[k][0]], append(slices.Clone(conds), auth))
		if err != nil {
			return nil, err
		}
		out = append(out, routes...)
	}
	routes, err := levels.routes(answers[bySuffix[def][0]], conds)
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
