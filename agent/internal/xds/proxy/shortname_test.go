package proxy

import (
	"fmt"
	"testing"

	meshconst "aethermesh.dev/common/constants/mesh"

	"aethermesh.dev/common/extensionfilter"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func callerNamespaceOf(t *testing.T, kvs [][2]string) (string, bool) {
	t.Helper()
	for _, kv := range kvs {
		if kv[0] == CallerNamespaceFilterStateKey {
			return kv[1], true
		}
	}
	return "", false
}

// TestCallerNamespaceIsStoredOnTheHTTPChainsOfAPod: a short name's virtual
// host reads the caller's namespace from filter state, so BOTH of a pod's
// HTTP chains must store it, with the pod's namespace as a literal: the
// loopback (proxy port) chain and every capture HTTP chain. A chain that does
// not is a chain on which every short name is refused. The raw-TCP chains
// route nothing by name and do not carry it.
func TestCallerNamespaceIsStoredOnTheHTTPChainsOfAPod(t *testing.T) {
	pod := sourceTestPod()
	id := SourceIdentityForPod(pod, testTrustDomain)

	ns, ok := callerNamespaceOf(t, allFilterStateValues(t, buildDefaultOutboundHTTPFilterChain(pod, id, testTrustDomain, false, nil)))
	require.True(t, ok, "the loopback HTTP chain does not store the caller's namespace")
	assert.Equal(t, pod.GetNamespace(), ns)

	svc := CaptureTCPService{ClusterName: "tcp:svc-a." + testTrustDomain, ClusterIP: "10.96.1.10", LegacyTCPPort: 9000}
	l, err := GenerateCaptureListener(pod, id, 18001, testTrustDomain, false, []CaptureTCPService{svc}, true, nil)
	require.NoError(t, err)
	httpChains, tcpChains := 0, 0
	for _, fc := range l.GetFilterChains() {
		ns, ok := callerNamespaceOf(t, allFilterStateValues(t, fc))
		if len(fc.GetName()) >= 8 && fc.GetName()[:8] == "cap_http" {
			httpChains++
			require.Truef(t, ok, "capture chain %s does not store the caller's namespace", fc.GetName())
			assert.Equal(t, pod.GetNamespace(), ns, fc.GetName())
			continue
		}
		tcpChains++
		assert.Falsef(t, ok, "raw-TCP chain %s stores the caller's namespace, which nothing on it reads", fc.GetName())
	}
	assert.Equal(t, 2, httpChains, "cap_http and cap_http_18081")
	assert.Positive(t, tcpChains)

	// The value is the namespace itself, set by the agent: not a header, not
	// anything a request carries.
	f := BuildCallerNamespaceFilterState("team-a")
	assert.Contains(t, f.String(), `inline_string:"team-a"`)
}

// TestShortNameVirtualHosts_Arms: built from a table's own virtual hosts, one
// virtual host per (short name, suffix), one arm per namespace, each arm the
// routes of what the full name of that namespace gets for the same suffix
// (exactly, else its ":*" virtual host), with that virtual host's
// typed_per_filter_config in force on every route; and a no-match arm that
// refuses.
func TestShortNameVirtualHosts_Arms(t *testing.T) {
	const mesh = "aether.internal"
	// "f" is the service-wide filter, a header_mutation; one route of port
	// 9090 names the same filter itself (an ExtensionRef of the same kind).
	const f = extensionfilter.HeaderMutationFilterName
	stamp := func(key string) *anypb.Any {
		a, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
			ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{Key: key, Value: "ran"},
			}}}},
		}})
		require.NoError(t, err)
		return a
	}
	stamps := func(a *anypb.Any) []string {
		var m header_mutationv3.HeaderMutationPerRoute
		require.NoError(t, a.UnmarshalTo(&m))
		var out []string
		for _, r := range m.GetMutations().GetResponseMutations() {
			out = append(out, r.GetAppend().GetHeader().GetKey())
		}
		return out
	}
	cfg, own := stamp("x-service-wide"), stamp("x-route-own")
	route := func(cluster string) *routev3.Route {
		return &routev3.Route{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}}},
		}
	}
	withOwn := route("echo.a.aether.internal:9090")
	withOwn.TypedPerFilterConfig = map[string]*anypb.Any{f: own}
	vhosts := []*routev3.VirtualHost{
		{Name: "echo.a|8080", Domains: []string{"echo.a.svc.cluster.local:8080", "echo.a.aether.internal:8080", "echo.a.aether.internal"}, Routes: []*routev3.Route{route("echo.a.aether.internal:8080")}, TypedPerFilterConfig: map[string]*anypb.Any{f: cfg}},
		{Name: "echo.a|9090", Domains: []string{"echo.a.aether.internal:9090"}, Routes: []*routev3.Route{withOwn, route("echo.a.aether.internal:9090")}, TypedPerFilterConfig: map[string]*anypb.Any{f: cfg}},
		BuildRefusalVirtualHost("echo.a|undeclared", []string{"echo.a.aether.internal:*"}, RefusalPortNotDeclared, "x"),
		{Name: "echo.b|8080", Domains: []string{"echo.b.aether.internal:8080"}, Routes: []*routev3.Route{route("echo.b.aether.internal:8080")}},
		BuildRefusalVirtualHost("echo.b|undeclared", []string{"echo.b.aether.internal:*"}, RefusalPortNotDeclared, "x"),
		{Name: "solo.c|8080", Domains: []string{"solo.c.aether.internal:8080"}, Routes: []*routev3.Route{route("solo.c.aether.internal:8080")}},
		{Name: "on_demand_catch_all", Domains: []string{"*"}, Routes: []*routev3.Route{route("x")}},
		{Name: "not-a-mesh-name", Domains: []string{"echo.a.b.c.aether.internal:8080", "echo.aether.internal", "echo:8080"}, Routes: []*routev3.Route{route("y")}},
	}
	before := len(vhosts[0].GetRoutes()[0].GetTypedPerFilterConfig())

	short := ShortNameVirtualHosts(vhosts, mesh, nil, nil, nil)
	byDomain := map[string]*routev3.VirtualHost{}
	for _, vh := range short {
		require.Len(t, vh.GetDomains(), 1)
		require.NotNil(t, vh.GetMatcher())
		assert.Empty(t, vh.GetRoutes())
		assert.Empty(t, vh.GetTypedPerFilterConfig(), "a shared virtual host enables no filter itself")
		// The outer matcher asks whether the caller's namespace is on the
		// connection; the inner one, reached when it is, picks by its value.
		fields := vh.GetMatcher().GetMatcherList().GetMatchers()
		require.Len(t, fields, 1)
		var in network_inputsv3.FilterStateInput
		require.NoError(t, fields[0].GetPredicate().GetSinglePredicate().GetInput().GetTypedConfig().UnmarshalTo(&in))
		assert.Equal(t, CallerNamespaceFilterStateKey, in.GetKey())
		assert.Equal(t, ".+", fields[0].GetPredicate().GetSinglePredicate().GetValueMatch().GetSafeRegex().GetRegex())
		require.NoError(t, fields[0].GetOnMatch().GetMatcher().GetMatcherTree().GetInput().GetTypedConfig().UnmarshalTo(&in))
		assert.Equal(t, CallerNamespaceFilterStateKey, in.GetKey())
		byDomain[vh.GetDomains()[0]] = vh
	}
	assert.ElementsMatch(t, []string{"echo", "echo:8080", "echo:9090", "echo:*", "solo:8080"}, keysOfVhosts(byDomain),
		"one per (short name, suffix) some mesh authority is claimed with; nothing from a name that is no <svc>.<ns>.<mesh domain>")

	arm := func(domain, ns string) []*routev3.Route {
		t.Helper()
		vh := byDomain[domain]
		require.NotNil(t, vh, domain)
		return shortArmRoutes(t, vh.GetMatcher(), ns)
	}
	// Namespace a: its own port virtual hosts, with the filter on every route.
	a8080 := arm("echo:8080", "a")
	require.Len(t, a8080, 1)
	assert.Equal(t, "echo.a.aether.internal:8080", a8080[0].GetRoute().GetCluster())
	assert.Equal(t, []string{"x-service-wide"}, stamps(a8080[0].GetTypedPerFilterConfig()[f]))
	a9090 := arm("echo:9090", "a")
	require.Len(t, a9090, 2)
	// The route that names the filter itself: BOTH levels are in force, the
	// service's first, as under the service's own virtual host. With the
	// route's entry alone the request ran without the service-wide filter.
	assert.Equal(t, []string{"x-service-wide", "x-route-own"}, stamps(a9090[0].GetTypedPerFilterConfig()[f]),
		"a header_mutation applies every level: the arm's route must carry both")
	assert.Equal(t, []string{"x-service-wide"}, stamps(a9090[1].GetTypedPerFilterConfig()[f]))
	assert.Equal(t, "echo.a.aether.internal:8080", arm("echo", "a")[0].GetRoute().GetCluster(), "the port-less authority, where a claims it")
	// Namespace b: 8080 its own; 9090 falls to ITS ":*" virtual host, the
	// refusal, not to a's 9090; port-less it claims nowhere, so no arm.
	assert.Equal(t, "echo.b.aether.internal:8080", arm("echo:8080", "b")[0].GetRoute().GetCluster())
	assert.Empty(t, arm("echo:8080", "b")[0].GetTypedPerFilterConfig(), "b's service has no filter")
	assert.Equal(t, uint32(421), arm("echo:9090", "b")[0].GetDirectResponse().GetStatus())
	assert.Equal(t, RefusalPortNotDeclared, arm("echo:9090", "b")[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
	// A caller whose namespace has no service of the name (another
	// namespace; b for the port-less name, which b claims nowhere): the name
	// is not a mesh name for it, and it gets what the table gives any Host
	// that is none. No floor was given here, so that is the 404.
	for _, tc := range [][2]string{{"echo:8080", "c"}, {"echo", "b"}, {"solo:8080", "a"}} {
		r := arm(tc[0], tc[1])
		require.Len(t, r, 1, "%v", tc)
		assert.Equal(t, uint32(404), r[0].GetDirectResponse().GetStatus(), "%v", tc)
		assert.Empty(t, r[0].GetResponseHeadersToAdd(), "%v: not a refusal by the mesh", tc)
		assert.Nil(t, r[0].GetRoute(), "%v: the arm delivers", tc)
	}
	// A connection that carries no caller namespace is another matter: it is
	// refused, and says so.
	r := arm("echo:8080", "")
	require.Len(t, r, 1)
	assert.Equal(t, uint32(421), r[0].GetDirectResponse().GetStatus())
	assert.Equal(t, RefusalCallerUnknown, r[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
	// The input is not written to.
	assert.Len(t, vhosts[0].GetRoutes()[0].GetTypedPerFilterConfig(), before)
}

func keysOfVhosts(m map[string]*routev3.VirtualHost) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// shortArmRoutes returns the routes a short name's matcher selects for ns:
// the refusal of a connection with no namespace (""), the namespace's arm, or
// the table's answer for a name that is not the mesh's.
func shortArmRoutes(t *testing.T, m *xdsmatcherv3.Matcher, ns string) []*routev3.Route {
	t.Helper()
	on := m.GetOnNoMatch()
	if ns != "" {
		fields := m.GetMatcherList().GetMatchers()
		require.Len(t, fields, 1)
		byCaller := fields[0].GetOnMatch().GetMatcher()
		require.NotNil(t, byCaller)
		on = byCaller.GetOnNoMatch()
		if arm, ok := byCaller.GetMatcherTree().GetExactMatchMap().GetMap()[ns]; ok {
			on = arm
		}
	}
	require.NotNil(t, on)
	var list routev3.RouteList
	require.NoError(t, on.GetAction().GetTypedConfig().UnmarshalTo(&list))
	return list.GetRoutes()
}

// TestShortNameArm_FilterLevelsPerFilterKind: for EVERY allow-listed filter,
// a route that names the same filter as the service's virtual host carries, in
// the short name's arm, what extensionfilter.CombineLevels says the two levels
// amount to: never the route's entry alone unless that filter's most specific
// level wins. A filter the arm cannot combine is not delivered.
func TestShortNameArm_FilterLevelsPerFilterKind(t *testing.T) {
	route := func() *routev3.Route {
		return &routev3.Route{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "echo.a.aether.internal:8080"}}},
		}
	}
	stamp := func(key string) *anypb.Any {
		a, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
			ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: key}}}}},
		}})
		require.NoError(t, err)
		return a
	}
	authz := func(ctx map[string]string) *anypb.Any {
		a, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{ContextExtensions: ctx}}})
		require.NoError(t, err)
		return a
	}
	other, err := anypb.New(&corev3.Metadata{})
	require.NoError(t, err)
	// Per allow-listed filter: the two levels. The opaque configs of the
	// most-specific-wins filters only have to differ.
	configs := map[string][2]*anypb.Any{
		extensionfilter.HeaderMutationFilterName:   {stamp("x-service-wide"), stamp("x-route-own")},
		extensionfilter.ExtAuthzFilterName:         {authz(map[string]string{"tenant": "a"}), authz(map[string]string{"action": "write"})},
		extensionfilter.RBACFilterName:             {stamp("stands-for-the-service's"), stamp("stands-for-the-route's")},
		extensionfilter.HeaderToMetadataFilterName: {stamp("stands-for-the-service's"), stamp("stands-for-the-route's")},
	}
	for _, name := range []string{extensionfilter.HeaderMutationFilterName, extensionfilter.ExtAuthzFilterName, extensionfilter.RBACFilterName, extensionfilter.HeaderToMetadataFilterName} {
		require.Truef(t, extensionfilter.Allowed(name), "%s", name)
	}
	for name, levels := range configs {
		t.Run(name, func(t *testing.T) {
			r := route()
			r.TypedPerFilterConfig = map[string]*anypb.Any{name: levels[1]}
			vh := &routev3.VirtualHost{Name: "echo.a|8080", Domains: []string{"echo.a.aether.internal:8080"}, Routes: []*routev3.Route{r, route()}, TypedPerFilterConfig: map[string]*anypb.Any{name: levels[0]}}
			short := ShortNameVirtualHosts([]*routev3.VirtualHost{vh}, "aether.internal", nil, nil, nil)
			require.Len(t, short, 1)
			arm := shortArmRoutes(t, short[0].GetMatcher(), "a")
			require.Len(t, arm, 2)

			want, err := extensionfilter.CombineLevels(name, levels[0], levels[1])
			require.NoError(t, err)
			assert.True(t, proto.Equal(want, arm[0].GetTypedPerFilterConfig()[name]), "the route that names the filter itself: %v", arm[0].GetTypedPerFilterConfig()[name])
			assert.True(t, proto.Equal(levels[0], arm[1].GetTypedPerFilterConfig()[name]), "a route that does not: the service's")
			sem, _ := extensionfilter.LevelSemanticsOf(name)
			if sem != extensionfilter.MostSpecificLevelWins {
				assert.False(t, proto.Equal(levels[1], arm[0].GetTypedPerFilterConfig()[name]), "the route's entry alone: the service-wide level is in force nowhere")
			}
			assert.NotNil(t, arm[0].GetRoute(), "still a route")
		})
	}

	// A filter whose levels cannot be combined (not allow-listed here; a
	// config that does not decode behaves the same): the route is not
	// delivered. The route that does not name the filter is unaffected.
	r := route()
	r.TypedPerFilterConfig = map[string]*anypb.Any{"envoy.filters.http.unknown": other}
	vh := &routev3.VirtualHost{Name: "echo.a|8080", Domains: []string{"echo.a.aether.internal:8080"}, Routes: []*routev3.Route{r, route()}, TypedPerFilterConfig: map[string]*anypb.Any{"envoy.filters.http.unknown": other}}
	arm := shortArmRoutes(t, ShortNameVirtualHosts([]*routev3.VirtualHost{vh}, "aether.internal", nil, nil, nil)[0].GetMatcher(), "a")
	require.Len(t, arm, 2)
	assert.Nil(t, arm[0].GetRoute(), "delivered although the two levels could not be combined")
	assert.Equal(t, uint32(500), arm[0].GetDirectResponse().GetStatus())
	assert.Equal(t, RefusalFilterLevels, arm[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
	assert.NotNil(t, arm[1].GetRoute())
}

// TestShortName_NotAMeshNameForTheCallerGetsTheTablesFloor: a short name whose
// service does not exist in the caller's namespace is not a mesh name for that
// caller, whether or not another namespace has one. It gets the routes of the
// table's own floor, a copy: the passthrough on a capture table with
// redirect-all, 404 on the loopback table. And a connection with no caller
// namespace is refused in every table.
func TestShortName_NotAMeshNameForTheCallerGetsTheTablesFloor(t *testing.T) {
	route := &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "api.b.aether.internal:8080"}}},
	}
	vhosts := func() []*routev3.VirtualHost {
		return []*routev3.VirtualHost{{Name: "api.b|8080", Domains: []string{"api.b.aether.internal:8080"}, Routes: []*routev3.Route{route}}}
	}
	short := func(rc *routev3.RouteConfiguration) *xdsmatcherv3.Matcher {
		t.Helper()
		for _, vh := range rc.GetVirtualHosts() {
			if len(vh.GetDomains()) == 1 && vh.GetDomains()[0] == "api:8080" {
				return vh.GetMatcher()
			}
		}
		t.Fatalf("%s has no virtual host for api:8080", rc.GetName())
		return nil
	}
	last := func(rs []*routev3.Route) *routev3.Route { return rs[len(rs)-1] }
	for _, tc := range []struct {
		name        string
		table       *routev3.RouteConfiguration
		passthrough bool
	}{
		{"cap_http_8080, redirect-all", BuildCaptureRouteConfiguration("cap_http_8080", vhosts(), BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, true), &ShortNames{MeshDomain: "aether.internal"}), true},
		{"cap_http_8080, scoped", BuildCaptureRouteConfiguration("cap_http_8080", vhosts(), BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, false), &ShortNames{MeshDomain: "aether.internal"}), false},
		{"out_http", BuildOutboundRouteConfiguration(vhosts(), "aether.internal", nil), false},
		{"cap_http_18081, redirect-all", BuildProxyPortCaptureRouteConfiguration("cap_http_18081", vhosts(), "aether.internal", true, nil), true},
		{"cap_http_18081, scoped", BuildProxyPortCaptureRouteConfiguration("cap_http_18081", vhosts(), "aether.internal", false, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := short(tc.table)
			floor := tc.table.GetVirtualHosts()[len(tc.table.GetVirtualHosts())-1]
			require.Equal(t, []string{"*"}, floor.GetDomains())

			// From b: its own service.
			assert.Equal(t, "api.b.aether.internal:8080", shortArmRoutes(t, m, "b")[0].GetRoute().GetCluster())

			// From a, which has no mesh "api": the floor's routes, all of
			// them, in order, and copies.
			got := shortArmRoutes(t, m, "a")
			require.Len(t, got, len(floor.GetRoutes()))
			for i := range got {
				assert.True(t, proto.Equal(floor.GetRoutes()[i], got[i]), "route %d of the not-a-mesh-name arm is not the floor's", i)
			}
			if tc.passthrough {
				assert.Equal(t, PassthroughClusterName, last(got).GetRoute().GetCluster(), "leaves to the address that was dialled")
			} else {
				assert.Equal(t, uint32(404), last(got).GetDirectResponse().GetStatus())
			}
			for _, r := range got {
				assert.NotEqual(t, "api.b.aether.internal:8080", r.GetRoute().GetCluster(), "another namespace's service answers")
			}

			// No caller namespace on the connection: refused, here too.
			unknown := shortArmRoutes(t, m, "")
			require.Len(t, unknown, 1)
			assert.Equal(t, uint32(421), unknown[0].GetDirectResponse().GetStatus())
			assert.Equal(t, RefusalCallerUnknown, unknown[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
		})
	}
}

// TestLoopbackShortNameFloor: what the short name of a service the node knows
// by name only gets on the loopback table, per authority suffix. The mesh's
// own two ports are a port of no service there either: refused by name, never
// a route to a cluster named for them (which no agent builds, so the request
// would wait out the on-demand timeout).
func TestLoopbackShortNameFloor(t *testing.T) {
	const fqdn = "echo.demo.aether.internal"
	refusal := func(routes []*routev3.Route) string {
		require.Len(t, routes, 1)
		require.NotNil(t, routes[0].GetDirectResponse(), "not a refusal: %v", routes[0])
		assert.Equal(t, uint32(421), routes[0].GetDirectResponse().GetStatus())
		return routes[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue()
	}
	for _, port := range []uint32{meshconst.ProxyOutboundPort, meshconst.ProxyL4OutboundPort} {
		routes := LoopbackShortNameFloor(fqdn, fmt.Sprintf(":%d", port))
		assert.Equalf(t, RefusalPortNotDeclared, refusal(routes), "port %d", port)
	}
	assert.Equal(t, RefusalPortRequired, refusal(LoopbackShortNameFloor(fqdn, "")))
	assert.Equal(t, RefusalShortNameCold, refusal(LoopbackShortNameFloor(fqdn, ":*")))
	// A real port: parked for the cluster the FULL name spells.
	routes := LoopbackShortNameFloor(fqdn, ":8080")
	require.NotEmpty(t, routes)
	assert.Equal(t, fqdn+":8080", routes[len(routes)-1].GetRoute().GetCluster())
}

// TestShortName_AColdNameBesideAnotherNamespacesServiceStaysItsOwn: a service
// the node knows by name only (namespace "a") and a service of the SAME name
// in another namespace ("b") that the node holds, on port 8080. The short
// domain "echo:8080" exists because b's service claims that suffix, so the
// caller in "a" is not refused "short-name-cold" there: its request is parked
// for a cluster. That cluster is its OWN namespace's, "echo.a.<mesh>:8080",
// named by the route. It can never be b's: a short name is the caller's
// namespace's and nothing else.
func TestShortName_AColdNameBesideAnotherNamespacesServiceStaysItsOwn(t *testing.T) {
	const mesh = "aether.internal"
	held := &routev3.VirtualHost{Name: "echo.b|8080", Domains: []string{"echo.b." + mesh + ":8080"}, Routes: []*routev3.Route{{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "echo.b." + mesh + ":8080"}}},
	}}}
	short := ShortNameVirtualHosts([]*routev3.VirtualHost{held}, mesh, []string{"echo.a." + mesh, "echo.b." + mesh}, LoopbackShortNameFloor, nil)
	byDomain := map[string]*routev3.VirtualHost{}
	for _, vh := range short {
		for _, d := range vh.GetDomains() {
			byDomain[d] = vh
		}
	}
	vh := byDomain["echo:8080"]
	require.NotNil(t, vh, "the short domain of the suffix b's service claims: %v", keysOfVhosts(byDomain))

	clusters := func(ns string) []string {
		var out []string
		for _, r := range shortArmRoutes(t, vh.GetMatcher(), ns) {
			if c := r.GetRoute().GetCluster(); c != "" {
				out = append(out, c)
			}
			assert.Emptyf(t, r.GetRoute().GetClusterHeader(), "caller in %q: a cluster named by the request's authority", ns)
		}
		return out
	}
	assert.Equal(t, []string{"echo.a." + mesh + ":8080"}, clusters("a"), "the cold service's caller is sent to another namespace's service, or nowhere")
	assert.Equal(t, []string{"echo.b." + mesh + ":8080"}, clusters("b"))

	// On a port no namespace's service claims, the cold name's short form is
	// refused at once: there is no cluster a short authority could name.
	wild := byDomain["echo:*"]
	require.NotNil(t, wild, "%v", keysOfVhosts(byDomain))
	routes := shortArmRoutes(t, wild.GetMatcher(), "a")
	require.Len(t, routes, 1)
	assert.Equal(t, RefusalShortNameCold, routes[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
}
