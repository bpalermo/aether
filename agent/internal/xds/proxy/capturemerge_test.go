package proxy

import (
	"strconv"
	"testing"

	"aethermesh.dev/agent/test/xdseval"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The compiler of the published capture table (MergeCaptureRouteTables), on
// small hand-built per-port tables: what it does with each shape, and what it
// refuses. The cache's tests hold it to the agent's real tables.

func mergeTestTables() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
	return mergeTestTablesMode(true)
}

// mergeTestTablesMode is mergeTestTables with each table's floor the
// passthrough (redirectAll) or 404 (scoped capture).
func mergeTestTablesMode(redirectAll bool) (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
	const mesh = "a.ns.mesh"
	routed := func(p uint32) *routev3.VirtualHost {
		vh := BuildServiceVirtualHost(mesh+"|"+strconv.Itoa(int(p)), mesh+":"+strconv.Itoa(int(p)), []string{mesh, mesh + ":" + strconv.Itoa(int(p)), "a.ns", "a.ns:" + strconv.Itoa(int(p))}, nil)
		// A service-wide filter, enabled on the virtual host.
		cfg, _ := anypb.New(wrapperspb.String("filter-config"))
		vh.TypedPerFilterConfig = map[string]*anypb.Any{"test.filter": cfg}
		return vh
	}
	mismatch := func(p uint32) *routev3.VirtualHost {
		return BuildRefusalVirtualHost(mesh+"|mismatch", []string{mesh + ":*", "a.ns:*"}, RefusalPortMismatch, "dialled "+strconv.Itoa(int(p))+"\n")
	}
	table := func(p uint32, vhs ...*routev3.VirtualHost) *routev3.RouteConfiguration {
		return BuildCaptureRouteConfiguration(CaptureHTTPRouteName(p), vhs, BuildCaptureCatchAllVirtualHost("mesh", p, false, redirectAll), nil)
	}
	perPort := map[uint32]*routev3.RouteConfiguration{
		8080: table(8080, routed(8080), mismatch(8080)),
		9090: table(9090, routed(9090), mismatch(9090)),
		80: table(80, BuildRefusalVirtualHost(mesh+"|refused", []string{mesh, mesh + ":80", "a.ns", "a.ns:80"}, RefusalPortNotDeclared, "undeclared\n"),
			mismatch(80)),
	}
	other := BuildCaptureRouteConfiguration(CaptureHTTPOtherRouteName,
		[]*routev3.VirtualHost{BuildRefusalVirtualHost(mesh+"|undeclared", []string{mesh, mesh + ":*", "a.ns", "a.ns:*"}, RefusalPortNotDeclared, "undeclared\n")},
		BuildCaptureCatchAllVirtualHost("mesh", 0, false, redirectAll), nil)
	return perPort, other
}

func TestMergeCaptureRouteTables_OneVirtualHostPerName(t *testing.T) {
	perPort, other := mergeTestTables()
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	require.Equal(t, CaptureHTTPChainName, merged.GetName())
	names := []string{}
	for _, vh := range merged.GetVirtualHosts() {
		names = append(names, vh.GetName())
	}
	assert.Equal(t, []string{"a.ns.mesh", "on_demand_catch_all"}, names, "one virtual host per name, and the catch-all")
	// Its domains are the spellings port-less and ":*": an exact ":<port>"
	// spelling is told apart by its routes.
	assert.ElementsMatch(t, []string{"a.ns", "a.ns:*", "a.ns.mesh", "a.ns.mesh:*"}, merged.GetVirtualHosts()[0].GetDomains())
	for _, vh := range merged.GetVirtualHosts() {
		assert.Nil(t, vh.GetMatcher(), "a plain route list, no matcher: %s", vh.GetName())
	}
	// The service-wide filter stays a virtual-host config (the proxy combines
	// it with a route's own as on the per-port table); the refusals of the
	// same virtual host disable it.
	vhCfg := merged.GetVirtualHosts()[0].GetTypedPerFilterConfig()
	assert.Contains(t, vhCfg, "test.filter", "the service's filter config stays on its virtual host")
	for _, r := range merged.GetVirtualHosts()[0].GetRoutes() {
		_, isRefusal := r.GetAction().(*routev3.Route_DirectResponse)
		_, routeSaysSo := r.GetTypedPerFilterConfig()["test.filter"]
		assert.Equal(t, isRefusal, routeSaysSo, "only a refusal route sets the service's filter (to disable it): %v", r)
	}

	at := func(host string, dialled string) []*routev3.Route {
		fs := map[string]string{}
		if dialled != "" {
			fs[DialledPortFilterStateKey] = dialled
		}
		req := xdseval.Request{Host: host, FilterState: fs}
		a, err := xdseval.Resolve(merged, req)
		require.NoError(t, err)
		first, err := xdseval.FirstRoute(a.Routes, req)
		require.NoError(t, err)
		if first == nil {
			return nil
		}
		return []*routev3.Route{first}
	}
	for _, tc := range []struct {
		host, dialled, want string
		filter              bool
	}{
		{"a.ns.mesh", "8080", "a.ns.mesh:8080", true},
		{"A.NS.MESH:8080", "8080", "a.ns.mesh:8080", true},
		{"a.ns:9090", "9090", "a.ns.mesh:9090", true},
		{"a.ns.mesh:9090", "8080", RefusalPortMismatch, false},
		{"a.ns.mesh:8080:8080", "8080", RefusalPortMismatch, false},
		{"a.ns.mesh", "80", RefusalPortNotDeclared, false},
		{"a.ns.mesh:8080", "80", RefusalPortMismatch, false},
		{"a.ns.mesh", "7777", RefusalPortNotDeclared, false},
		{"a.ns.mesh:8080", "", RefusalPortNotDeclared, false},
	} {
		routes := at(tc.host, tc.dialled)
		require.NotEmpty(t, routes, "%s dialled %s", tc.host, tc.dialled)
		o := xdseval.OutcomeOf(routes[0])
		if o.Reason != "" {
			assert.Equal(t, tc.want, o.Reason, "%s dialled %s", tc.host, tc.dialled)
		} else {
			assert.Equal(t, tc.want, o.Cluster, "%s dialled %s", tc.host, tc.dialled)
		}
		vh := xdseval.PickVirtualHost(merged, tc.host)
		_, has := xdseval.FilterLevels(vh.GetTypedPerFilterConfig(), routes[0])["test.filter"]
		assert.Equal(t, tc.filter, has, "%s dialled %s: the service's filter runs", tc.host, tc.dialled)
	}
	// The catch-all names a cluster on demand only for the dialled port.
	for _, d := range []string{"80", "8080", "9090", "7777"} {
		routes := at("x.y.mesh:"+d, d)
		require.NotEmpty(t, routes)
		assert.Equal(t, d != "7777", routes[0].GetRoute().GetClusterHeader() != "", "the catch-all, dialled %s", d)
		if d != "8080" {
			other := at("x.y.mesh:8080", d)
			assert.Empty(t, other[0].GetRoute().GetClusterHeader(), "dialled %s, Host :8080: no cluster on demand", d)
		}
	}
}

// When one compiled virtual host's answers disagree on the virtual-host
// config (here, port 9090's service-wide filter is another config), it cannot
// stay on the virtual host: each route carries its answer's config folded
// (CombineLevels), and a route that cannot be folded exactly is refused with
// filter-levels rather than run in another order.
func TestMergeCaptureRouteTables_FoldsWhenTheVirtualHostConfigsDiffer(t *testing.T) {
	hm := func(t *testing.T, req, query bool) *anypb.Any {
		t.Helper()
		m := &header_mutationv3.Mutations{}
		if req {
			m.RequestMutations = []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Remove{Remove: "x-a"}}}
		}
		if query {
			m.QueryParameterMutations = []*corev3.KeyValueMutation{{Remove: "q"}}
		}
		a, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: m})
		require.NoError(t, err)
		return a
	}
	const name = "envoy.filters.http.header_mutation"
	perPort, other := mergeTestTables()
	for p, rc := range perPort {
		vh := rc.GetVirtualHosts()[0]
		if len(vh.GetTypedPerFilterConfig()) == 0 {
			continue
		}
		vh.TypedPerFilterConfig = map[string]*anypb.Any{name: hm(t, true, p == 9090)}
		if p == 8080 {
			// A rule that mutates the query, under a service that mutates a
			// request header: no single config applies the two in order.
			vh.Routes[0].TypedPerFilterConfig = map[string]*anypb.Any{name: hm(t, false, true)}
		}
	}
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	vh := merged.GetVirtualHosts()[0]
	assert.Empty(t, vh.GetTypedPerFilterConfig(), "two virtual-host configs: folded into the routes")
	first := func(dialled string) *routev3.Route {
		req := xdseval.Request{Host: "a.ns.mesh", FilterState: map[string]string{DialledPortFilterStateKey: dialled}}
		r, err := xdseval.FirstRoute(vh.GetRoutes(), req)
		require.NoError(t, err)
		require.NotNil(t, r)
		return r
	}
	assert.Equal(t, RefusalFilterLevels, xdseval.OutcomeOf(first("8080")).Reason, "an inexact fold is refused")
	served := first("9090")
	assert.Equal(t, "a.ns.mesh:9090", xdseval.OutcomeOf(served).Cluster)
	assert.True(t, proto.Equal(hm(t, true, true), served.GetTypedPerFilterConfig()[name]), "9090's service config, folded onto its route")
}

func TestMergeCaptureRouteTables_RefusesWhatItDoesNotModel(t *testing.T) {
	perPort, other := mergeTestTables()
	mutated := func(f func(rc *routev3.RouteConfiguration)) (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
		p, o := mergeTestTables()
		f(p[8080])
		return p, o
	}
	for name, in := range map[string]func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration){
		"a suffix wildcard": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				rc.VirtualHosts[0].Domains = append(rc.VirtualHosts[0].Domains, "*.ns.mesh")
			})
		},
		"a virtual host's retry policy": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				rc.VirtualHosts[0].RetryPolicy = &routev3.RetryPolicy{NumRetries: wrapperspb.UInt32(1)}
			})
		},
		"a table-level field": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				rc.ResponseHeadersToAdd = []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: "x", Value: "y"}}}
			})
		},
		"no catch-all": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) { rc.VirtualHosts = rc.VirtualHosts[:len(rc.VirtualHosts)-1] })
		},
		"a matcher with routes": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			p, o := mergeTestTables()
			short := shortNameVirtualHost("a", "", map[string]shortArm{"ns": {routes: p[8080].GetVirtualHosts()[0].GetRoutes()}}, nil)
			short.Routes = p[8080].GetVirtualHosts()[0].GetRoutes()
			p[8080].VirtualHosts = append([]*routev3.VirtualHost{short}, p[8080].VirtualHosts...)
			return p, o
		},
		"a missing table": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return perPort, nil
		},
		"a route list a request can fall out of": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				vh := rc.VirtualHosts[0]
				vh.Routes = []*routev3.Route{{Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/only"}}, Action: vh.Routes[0].Action}}
			})
		},
		// (Two names of one virtual host that one table answers differently
		// for one suffix were refused until chart 3.0.2; they are told apart
		// by name now: TestMergeCaptureRouteTables_TellsNamesApartWhereATableDoes.)
	} {
		p, o := in()
		_, err := MergeCaptureRouteTables(CaptureHTTPChainName, p, o)
		assert.Error(t, err, name)
	}
	_, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err, "the unmutated tables compile")
}

// mergeMismatches compares the compiled table with the per-port table of each
// dialled port ("" and any port without a table: other), for every Host,
// dialled port and caller given: the same first route (action, filters,
// status and x-aether-error), or why not.
func mergeMismatches(t *testing.T, merged *routev3.RouteConfiguration, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration, hosts []string, dialled []string, callers []string, paths ...string) []string {
	t.Helper()
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	var out []string
	for _, p := range paths {
		out = append(out, mergeMismatchesAt(t, merged, perPort, other, hosts, dialled, callers, p)...)
	}
	return out
}

func mergeMismatchesAt(t *testing.T, merged *routev3.RouteConfiguration, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration, hosts []string, dialled []string, callers []string, path string) []string {
	t.Helper()
	compiled := xdseval.NewTable(merged)
	first := func(tbl *xdseval.Table, req xdseval.Request) (*routev3.Route, map[string]*anypb.Any) {
		a, err := tbl.Resolve(req)
		require.NoError(t, err)
		var routes []*routev3.Route
		if !a.NoVhost {
			routes = a.Routes
		}
		r, err := xdseval.FirstRoute(routes, req)
		require.NoError(t, err)
		return r, a.VhostConfig
	}
	var out []string
	for _, host := range hosts {
		for _, d := range dialled {
			want := xdseval.NewTable(other)
			if p, err := strconv.Atoi(d); err == nil {
				if rc, ok := perPort[uint32(p)]; ok {
					want = xdseval.NewTable(rc)
				}
			}
			for _, caller := range callers {
				fs := map[string]string{}
				if d != "" {
					fs[DialledPortFilterStateKey] = d
				}
				if caller != "" {
					fs[CallerNamespaceFilterStateKey] = caller
				}
				// "" stands for a request with no path at all.
				req := xdseval.Request{Host: host, Path: path, NoPath: path == "", FilterState: fs}
				ra, ca := first(want, req)
				rb, cb := first(compiled, req)
				if same, why := xdseval.SameTreatmentIn(ca, ra, cb, rb); !same {
					out = append(out, "Host "+host+" dialled "+d+" caller "+caller+" path "+strconv.Quote(path)+": "+why)
				}
			}
		}
	}
	return out
}

// TestMergeCaptureRouteTables_TellsNamesApartWhereATableDoes: when one table
// answers two names of one compiled virtual host differently for the same
// suffix (here port 8080's table knows "a.ns.mesh" and leaves "a.ns" to its
// catch-all), the compiled routes of that port tell the names apart by the
// name itself (an exact or "<name>:" prefix :authority match, never a regex:
// a name can be longer than the proxy's regex budget), and every other port
// is compiled as before.
func TestMergeCaptureRouteTables_TellsNamesApartWhereATableDoes(t *testing.T) {
	perPort, other := mergeTestTables()
	vh := perPort[8080].VirtualHosts[0]
	vh.Domains = []string{"a.ns.mesh", "a.ns.mesh:8080"}
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	assert.Len(t, merged.GetVirtualHosts(), 2, "still one virtual host per name, and the catch-all")
	byName := 0
	for _, r := range merged.GetVirtualHosts()[0].GetRoutes() {
		for _, h := range r.GetMatch().GetHeaders() {
			if h.GetName() == ":authority" && (h.GetStringMatch().GetExact() != "" || h.GetStringMatch().GetPrefix() != "") {
				byName++
				assert.True(t, h.GetStringMatch().GetIgnoreCase(), "a Host's case does not matter")
				assert.Contains(t, []string{"a.ns.mesh", "a.ns.mesh:"}, h.GetStringMatch().GetExact()+h.GetStringMatch().GetPrefix(), "the smaller class is told apart")
			}
		}
	}
	assert.Positive(t, byName, "port 8080's routes name the name")
	hosts := []string{"a.ns.mesh", "A.NS.MESH", "a.ns.mesh:8080", "a.ns.mesh:9090", "a.ns.mesh:8080:8080", "a.ns", "a.ns:8080", "A.NS:8080", "a.ns:9090", "a.ns:7", "x.y.mesh:8080"}
	assert.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, []string{"80", "8080", "9090", "7777", ""}, []string{""}))
}

// TestMergeCaptureRouteTables_AWildcardOnlyPortIsAnsweredByTheWildcard: two
// names of one compiled virtual host, "a" with an exact ":9090" and "b" whose
// ":9090" only its ":*" selects, answered differently there. They must not
// share a class (nameClass.fits, wildcardAgrees): a suffix-only block would
// give "b:9090" a's answer. (Found by the adversarial review of #1709: no
// other test failed with the check made to always pass.)
func TestMergeCaptureRouteTables_AWildcardOnlyPortIsAnsweredByTheWildcard(t *testing.T) {
	x := BuildRefusalVirtualHost("a.ns.mesh|x", []string{"a.ns.mesh", "a.ns.mesh:9090", "b.ns.mesh"}, "reason-x", "x\n")
	y := BuildRefusalVirtualHost("a.ns.mesh|y", []string{"b.ns.mesh:*"}, "reason-y", "y\n")
	perPort := map[uint32]*routev3.RouteConfiguration{
		8080: BuildCaptureRouteConfiguration(CaptureHTTPRouteName(8080), []*routev3.VirtualHost{x, y}, BuildCaptureCatchAllVirtualHost("mesh", 8080, false, true), nil),
	}
	other := BuildCaptureRouteConfiguration(CaptureHTTPOtherRouteName, nil, BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	hosts := []string{"a.ns.mesh", "a.ns.mesh:9090", "a.ns.mesh:7", "b.ns.mesh", "b.ns.mesh:9090", "b.ns.mesh:7", "B.NS.MESH:9090"}
	assert.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, []string{"8080", "9090", ""}, []string{""}))
}

// TestMergeCaptureRouteTables_APathlessRequestNeverTakesAnotherNamesRoutes: a
// request with no path (a CONNECT without one) matches no "/"-prefixed route,
// so it falls out of its own name's routes. When the largest class of names
// answers with a route that can take it (a regex path), that class is
// conditioned on its names too: the request gets what its own table gives it
// (here no route), never another name's answer. Port 8080's table answers
// "a.ns.mesh" with a refusal and its other names with a regex rule.
func TestMergeCaptureRouteTables_APathlessRequestNeverTakesAnotherNamesRoutes(t *testing.T) {
	const mesh = "a.ns.mesh"
	rule := &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_SafeRegex{SafeRegex: &typematcherv3.RegexMatcher{Regex: ".*"}}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "by-regex"}}},
	}
	routed := BuildServiceVirtualHost(mesh+"|8080", mesh+":8080", []string{"a.ns", "a.ns:8080", "a.ns.svc", "a.ns.svc:8080"}, nil)
	routed.Routes = append([]*routev3.Route{rule}, routed.Routes...)
	refused := BuildRefusalVirtualHost(mesh+"|refused", []string{mesh, mesh + ":8080"}, RefusalPortRequired, "refused\n")
	perPort := map[uint32]*routev3.RouteConfiguration{
		8080: BuildCaptureRouteConfiguration(CaptureHTTPRouteName(8080), []*routev3.VirtualHost{routed, refused}, BuildCaptureCatchAllVirtualHost("mesh", 8080, false, true), nil),
	}
	other := BuildCaptureRouteConfiguration(CaptureHTTPOtherRouteName, nil, BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	hosts := []string{mesh, mesh + ":8080", "a.ns", "a.ns:8080", "a.ns.svc:8080", "A.NS:8080"}
	assert.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, []string{"8080", "9090", ""}, []string{""}, "/", "/x", ""))

	// Without a route that takes a pathless request, the largest class keeps
	// no name condition (no cost).
	routed.Routes = routed.Routes[1:]
	plain, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	byName := func(rc *routev3.RouteConfiguration) int {
		n := 0
		for _, r := range rc.GetVirtualHosts()[0].GetRoutes() {
			for _, h := range r.GetMatch().GetHeaders() {
				if h.GetStringMatch().GetExact() != "" || h.GetStringMatch().GetPrefix() != "" {
					n++
				}
			}
		}
		return n
	}
	assert.Greater(t, byName(merged), byName(plain), "the largest class is told apart by name only when it must be")
	assert.Empty(t, mergeMismatches(t, plain, perPort, other, hosts, []string{"8080", "9090", ""}, []string{""}, "/", ""))
}

// TestMergeCaptureRouteTables_APathlessRequestNeverTakesAnotherPortsAnswer:
// the same fall-through as above, between the answers one name gets by the
// port in its Host. Port 8080's table answers "a.ns.mesh:8080" with
// "/"-prefixed routes only, and the name's other spellings (the default
// answer, the one most suffixes get, or the one of ":*") with a regex rule. A
// CONNECT to "a.ns.mesh:8080" without a path gets no route from that table,
// and must get none compiled, never the regex rule of the default.
func TestMergeCaptureRouteTables_APathlessRequestNeverTakesAnotherPortsAnswer(t *testing.T) {
	const mesh = "a.ns.mesh"
	rule := &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_SafeRegex{SafeRegex: &typematcherv3.RegexMatcher{Regex: ".*"}}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "by-regex"}}},
	}
	for name, otherDoms := range map[string][]string{
		"the default most suffixes get": {mesh, mesh + ":9090", mesh + ":7070"},
		"the default of :*":             {mesh, mesh + ":*"},
	} {
		t.Run(name, func(t *testing.T) {
			routed := BuildServiceVirtualHost(mesh+"|8080", mesh+":8080", []string{mesh + ":8080"}, nil)
			others := BuildServiceVirtualHost(mesh+"|others", mesh+":9090", otherDoms, nil)
			others.Routes = append([]*routev3.Route{proto.Clone(rule).(*routev3.Route)}, others.Routes...)
			perPort := map[uint32]*routev3.RouteConfiguration{
				8080: BuildCaptureRouteConfiguration(CaptureHTTPRouteName(8080), []*routev3.VirtualHost{routed, others}, BuildCaptureCatchAllVirtualHost("mesh", 8080, false, true), nil),
			}
			other := BuildCaptureRouteConfiguration(CaptureHTTPOtherRouteName, nil, BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
			merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
			require.NoError(t, err)

			connect := xdseval.Request{Host: mesh + ":8080", NoPath: true, FilterState: map[string]string{DialledPortFilterStateKey: "8080"}}
			a, err := xdseval.Resolve(merged, connect)
			require.NoError(t, err)
			r, err := xdseval.FirstRoute(a.Routes, connect)
			require.NoError(t, err)
			assert.Nil(t, r, "a CONNECT to name:8080 with no path gets no route, as on its own table")

			hosts := []string{mesh, "A.NS.MESH", mesh + ":8080", "A.NS.MESH:8080", mesh + ":9090", mesh + ":7070", mesh + ":7", mesh + ":8080:8080", "x.y.mesh:8080"}
			assert.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, []string{"8080", "9090", ""}, []string{""}, "/", "/x", ""))
		})
	}

	// Without a route that takes a pathless request, the default keeps no
	// suffix condition (no cost): mergeTestTables compiles as it always did.
	perPort, other := mergeTestTables()
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	for _, vh := range merged.GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			for _, h := range r.GetMatch().GetHeaders() {
				assert.False(t, h.GetInvertMatch(), "virtual host %s: no inverted suffix condition", vh.GetName())
			}
		}
	}
}

// proxyPortTestTable is the shape of the proxy port's table
// (BuildProxyPortCaptureRouteConfiguration over the loopback table's virtual
// hosts) for the service of mergeTestTables: its mesh name only, by the port
// in Host, with the service-wide filter on its routed virtual hosts, and its
// short name for callers of its namespace.
func proxyPortTestTable(redirectAll bool) *routev3.RouteConfiguration {
	const mesh = "a.ns.mesh"
	cfg, _ := anypb.New(wrapperspb.String("filter-config"))
	routed := func(cluster string, doms ...string) *routev3.VirtualHost {
		vh := BuildOutboundClusterVirtualHost(cluster, doms)
		vh.TypedPerFilterConfig = map[string]*anypb.Any{"test.filter": cfg}
		return vh
	}
	loopback := []*routev3.VirtualHost{
		routed(mesh+":8080", mesh+":8080"),
		routed(mesh+":9090", mesh+":9090"),
		BuildRefusalVirtualHost(mesh+"|portless", []string{mesh}, RefusalPortRequired, "port required\n"),
		BuildRefusalVirtualHost(mesh+"|undeclared", []string{mesh + ":*"}, RefusalPortNotDeclared, "undeclared\n"),
		// A service the node holds endpoints of but has no generated Service
		// for: on the loopback table (by its endpoints) and in no per-port
		// table, so its compiled virtual hosts are built from the proxy
		// port's domains alone, one of them named after a virtual host with
		// no "|" ("e.ns.mesh:8080").
		BuildOutboundClusterVirtualHost(endpointOnly+":8080", []string{endpointOnly + ":8080"}),
		BuildRefusalVirtualHost(endpointOnly+"|portless", []string{endpointOnly}, RefusalPortRequired, "port required\n"),
		BuildRefusalVirtualHost(endpointOnly+"|undeclared", []string{endpointOnly + ":*"}, RefusalPortNotDeclared, "undeclared\n"),
	}
	return BuildProxyPortCaptureRouteConfiguration(CaptureHTTPRouteName(18081), loopback, "mesh", redirectAll, []string{mesh})
}

// endpointOnly is a service of proxyPortTestTable with no generated Service.
const endpointOnly = "e.ns.mesh"

// TestMergeCaptureRouteTables_ProxyPortIsOneMoreDialledPort: the proxy port's
// table (until chart 3.0.2 served by a chain of its own) compiles into the one
// table as one more dialled port, and every Host class gets its answer: a
// declared port, no port, the proxy port and the L4 port, an undeclared port,
// the port twice, another name of the service the proxy port's table does
// not know, the short name with its caller's namespace, another one and none,
// a name the node knows nothing of (on demand) and a name that is not the
// mesh's (the passthrough with redirect-all, 404 scoped), with the
// service-wide filter in force exactly where it was. Its block follows every
// declared port's in each virtual host. A compiled table that loses one of
// its routes is seen.
func TestMergeCaptureRouteTables_ProxyPortIsOneMoreDialledPort(t *testing.T) {
	for _, redirectAll := range []bool{true, false} {
		perPort, other := mergeTestTablesMode(redirectAll)
		perPort[18081] = proxyPortTestTable(redirectAll)
		// A declared port number above the proxy port's: its block still
		// comes first.
		perPort[30080] = BuildCaptureRouteConfiguration(CaptureHTTPRouteName(30080), []*routev3.VirtualHost{
			BuildRefusalVirtualHost("a.ns.mesh|refused", []string{"a.ns.mesh", "a.ns.mesh:30080", "a.ns", "a.ns:30080"}, RefusalPortNotDeclared, "undeclared 30080\n"),
			BuildRefusalVirtualHost("a.ns.mesh|mismatch", []string{"a.ns.mesh:*", "a.ns:*"}, RefusalPortMismatch, "dialled 30080\n"),
		}, BuildCaptureCatchAllVirtualHost("mesh", 30080, false, redirectAll), nil)
		merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
		require.NoError(t, err, "redirect-all %v", redirectAll)

		var hosts []string
		for _, n := range []string{"a.ns.mesh", "A.NS.MESH", "a.ns", "a", "x.y.mesh", "example.com", endpointOnly, "E.NS.MESH", "e.ns", "e"} {
			for _, s := range []string{"", ":80", ":8080", ":9090", ":18081", ":18082", ":30080", ":7777", ":8080:8080"} {
				hosts = append(hosts, n+s)
			}
		}
		dialled := []string{"80", "8080", "9090", "18081", "30080", "7777", ""}
		callers := []string{"", "ns", "other"}
		require.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, dialled, callers), "redirect-all %v", redirectAll)

		// The proxy accepts the table only with unique virtual-host names
		// and each domain in one virtual host; the endpoint-only service's
		// groups come from the proxy port's domains alone.
		vhosts, domains := map[string]bool{}, map[string]string{}
		for _, vh := range merged.GetVirtualHosts() {
			assert.False(t, vhosts[vh.GetName()], "virtual host %s twice", vh.GetName())
			vhosts[vh.GetName()] = true
			for _, d := range vh.GetDomains() {
				prev, dup := domains[d]
				assert.False(t, dup, "domain %s in %s and %s", d, prev, vh.GetName())
				domains[d] = vh.GetName()
			}
		}
		assert.Equal(t, endpointOnly+":8080", domains[endpointOnly+":8080"], "a group named after a virtual host with no \"|\"")
		assert.Equal(t, endpointOnly, domains[endpointOnly])
		assert.Equal(t, endpointOnly, domains[endpointOnly+":*"])

		// The proxy port's routes follow every declared port's.
		for _, vh := range merged.GetVirtualHosts() {
			last, firstProxy := -1, -1
			for i, r := range vh.GetRoutes() {
				for _, fs := range r.GetMatch().GetFilterState() {
					switch fs.GetStringMatch().GetExact() {
					case "18081":
						if firstProxy < 0 {
							firstProxy = i
						}
					case "80", "8080", "9090", "30080":
						last = i
					}
				}
			}
			if firstProxy >= 0 {
				assert.Greater(t, firstProxy, last, "virtual host %s: the proxy port's block is last of the dialled ports'", vh.GetName())
			}
		}

		// Seen red: one route of the proxy port's block dropped.
		bad := proto.Clone(merged).(*routev3.RouteConfiguration)
		dropped := false
		for _, vh := range bad.GetVirtualHosts() {
			for i, r := range vh.GetRoutes() {
				if !dropped && len(r.GetMatch().GetFilterState()) > 0 && r.GetMatch().GetFilterState()[0].GetStringMatch().GetExact() == "18081" {
					vh.Routes = append(vh.Routes[:i:i], vh.Routes[i+1:]...)
					dropped = true
					break
				}
			}
		}
		require.True(t, dropped)
		assert.NotEmpty(t, mergeMismatches(t, bad, perPort, other, hosts, dialled, callers), "a lost route of the proxy port is seen")
	}
}

// TestMergeCaptureRouteTables_ShortNameKeepsItsCallerMatcher: a short name's
// virtual host answers by the caller's namespace; compiled, the dialled port
// selects which of those matchers runs.
func TestMergeCaptureRouteTables_ShortNameKeepsItsCallerMatcher(t *testing.T) {
	perPort, other := mergeTestTables()
	for p, rc := range perPort {
		arm := []*routev3.Route{BuildRefusalVirtualHost("", nil, "arm-"+strconv.Itoa(int(p)), "x\n").GetRoutes()[0]}
		rc.VirtualHosts = append([]*routev3.VirtualHost{shortNameVirtualHost("a", "", map[string]shortArm{"ns": {routes: arm}}, nil)}, rc.VirtualHosts...)
	}
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	for _, d := range []string{"80", "8080", "9090"} {
		first := func(fs map[string]string) xdseval.Outcome {
			req := xdseval.Request{Host: "a", FilterState: fs}
			a, err := xdseval.Resolve(merged, req)
			require.NoError(t, err)
			r, err := xdseval.FirstRoute(a.Routes, req)
			require.NoError(t, err)
			return xdseval.OutcomeOf(r)
		}
		assert.Equal(t, "arm-"+d, first(map[string]string{DialledPortFilterStateKey: d, CallerNamespaceFilterStateKey: "ns"}).Reason)
		assert.Equal(t, RefusalCallerUnknown, first(map[string]string{DialledPortFilterStateKey: d}).Reason, "no caller namespace")
	}
}

// TestRefuseCaptureMeshNames: the table published when the per-port tables do
// not compile refuses every name any of them claims, whatever was dialled,
// and leaves every other Host to the floor of a port no service declares.
func TestRefuseCaptureMeshNames(t *testing.T) {
	perPort, other := mergeTestTables()
	rc := RefuseCaptureMeshNames(CaptureHTTPChainName, perPort, other)
	for _, host := range []string{"a.ns.mesh", "a.ns.mesh:8080", "A.NS:9090", "a.ns:1"} {
		for _, d := range []string{"", "8080", "7777"} {
			req := xdseval.Request{Host: host, FilterState: map[string]string{DialledPortFilterStateKey: d}}
			a, err := xdseval.Resolve(rc, req)
			require.NoError(t, err)
			r, err := xdseval.FirstRoute(a.Routes, req)
			require.NoError(t, err)
			o := xdseval.OutcomeOf(r)
			assert.Equal(t, uint32(421), o.Status, "%s dialled %q", host, d)
			assert.Equal(t, RefusalPortNotDeclared, o.Reason)
		}
	}
	req := xdseval.Request{Host: "example.com"}
	a, err := xdseval.Resolve(rc, req)
	require.NoError(t, err)
	r, err := xdseval.FirstRoute(a.Routes, req)
	require.NoError(t, err)
	assert.Equal(t, PassthroughClusterName, xdseval.OutcomeOf(r).Cluster, "a Host that is no mesh name leaves as it would")
}

// TestCaptureChainMatcher_ReportsADuplicateAddress: two capture services that
// claim one address are reported (the cache logs and counts it), and the first
// keeps the address.
func TestCaptureChainMatcher_ReportsADuplicateAddress(t *testing.T) {
	var got []string
	OnDuplicateCaptureAddress(func(listener, address, kept, dropped string) {
		got = append(got, listener+" "+address+" "+kept+" "+dropped)
	})
	t.Cleanup(func() { OnDuplicateCaptureAddress(func(string, string, string, string) {}) })
	perPortListener(t, []CaptureTCPService{
		{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", TCPPorts: []uint32{9000}},
		{ClusterName: "tcp:b.t.aether.internal", ClusterIP: "10.96.0.1", TCPPorts: []uint32{9001}},
	}, true)
	assert.Equal(t, []string{"capture_default_p1 10.96.0.1 tcp:a.t.aether.internal tcp:b.t.aether.internal"}, got)
}
