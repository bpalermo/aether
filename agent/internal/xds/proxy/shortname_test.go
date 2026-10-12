package proxy

import (
	"testing"

	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	l, err := GenerateCaptureListener(pod, id, 18001, testTrustDomain, false, []CaptureTCPService{svc}, []uint32{80, 8080}, true, nil)
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
	assert.GreaterOrEqual(t, httpChains, 3, "cap_http_80, cap_http_8080, cap_http_other")
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
// typed_per_filter_config on every route; and a no-match arm that refuses.
func TestShortNameVirtualHosts_Arms(t *testing.T) {
	const mesh = "aether.internal"
	cfg, err := anypb.New(&corev3.Metadata{})
	require.NoError(t, err)
	own, err := anypb.New(&corev3.HeaderValue{Key: "own"})
	require.NoError(t, err)
	route := func(cluster string) *routev3.Route {
		return &routev3.Route{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}}},
		}
	}
	withOwn := route("echo.a.aether.internal:9090")
	withOwn.TypedPerFilterConfig = map[string]*anypb.Any{"f": own}
	vhosts := []*routev3.VirtualHost{
		{Name: "echo.a|8080", Domains: []string{"echo.a.svc.cluster.local:8080", "echo.a.aether.internal:8080", "echo.a.aether.internal"}, Routes: []*routev3.Route{route("echo.a.aether.internal:8080")}, TypedPerFilterConfig: map[string]*anypb.Any{"f": cfg}},
		{Name: "echo.a|9090", Domains: []string{"echo.a.aether.internal:9090"}, Routes: []*routev3.Route{withOwn, route("echo.a.aether.internal:9090")}, TypedPerFilterConfig: map[string]*anypb.Any{"f": cfg}},
		BuildRefusalVirtualHost("echo.a|undeclared", []string{"echo.a.aether.internal:*"}, RefusalPortNotDeclared, "x"),
		{Name: "echo.b|8080", Domains: []string{"echo.b.aether.internal:8080"}, Routes: []*routev3.Route{route("echo.b.aether.internal:8080")}},
		BuildRefusalVirtualHost("echo.b|undeclared", []string{"echo.b.aether.internal:*"}, RefusalPortNotDeclared, "x"),
		{Name: "solo.c|8080", Domains: []string{"solo.c.aether.internal:8080"}, Routes: []*routev3.Route{route("solo.c.aether.internal:8080")}},
		{Name: "on_demand_catch_all", Domains: []string{"*"}, Routes: []*routev3.Route{route("x")}},
		{Name: "not-a-mesh-name", Domains: []string{"echo.a.b.c.aether.internal:8080", "echo.aether.internal", "echo:8080"}, Routes: []*routev3.Route{route("y")}},
	}
	before := len(vhosts[0].GetRoutes()[0].GetTypedPerFilterConfig())

	short := ShortNameVirtualHosts(vhosts, mesh)
	byDomain := map[string]*routev3.VirtualHost{}
	for _, vh := range short {
		require.Len(t, vh.GetDomains(), 1)
		require.NotNil(t, vh.GetMatcher())
		assert.Empty(t, vh.GetRoutes())
		assert.Empty(t, vh.GetTypedPerFilterConfig(), "a shared virtual host enables no filter itself")
		var in network_inputsv3.FilterStateInput
		require.NoError(t, vh.GetMatcher().GetMatcherTree().GetInput().GetTypedConfig().UnmarshalTo(&in))
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
	assert.Equal(t, cfg.GetTypeUrl(), a8080[0].GetTypedPerFilterConfig()["f"].GetTypeUrl())
	a9090 := arm("echo:9090", "a")
	require.Len(t, a9090, 2)
	assert.Equal(t, own.GetTypeUrl(), a9090[0].GetTypedPerFilterConfig()["f"].GetTypeUrl(), "a route's own entry for the filter wins")
	assert.Equal(t, cfg.GetTypeUrl(), a9090[1].GetTypedPerFilterConfig()["f"].GetTypeUrl())
	assert.Equal(t, "echo.a.aether.internal:8080", arm("echo", "a")[0].GetRoute().GetCluster(), "the port-less authority, where a claims it")
	// Namespace b: 8080 its own; 9090 falls to ITS ":*" virtual host, the
	// refusal, not to a's 9090; port-less it claims nowhere, so no arm.
	assert.Equal(t, "echo.b.aether.internal:8080", arm("echo:8080", "b")[0].GetRoute().GetCluster())
	assert.Empty(t, arm("echo:8080", "b")[0].GetTypedPerFilterConfig(), "b's service has no filter")
	assert.Equal(t, uint32(421), arm("echo:9090", "b")[0].GetDirectResponse().GetStatus())
	assert.Equal(t, RefusalPortNotDeclared, arm("echo:9090", "b")[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
	// No arm: another namespace, no namespace, and b for the port-less name.
	for _, tc := range [][2]string{{"echo:8080", "c"}, {"echo:8080", ""}, {"echo", "b"}, {"solo:8080", "a"}} {
		r := arm(tc[0], tc[1])
		require.Len(t, r, 1, "%v", tc)
		assert.Equal(t, uint32(421), r[0].GetDirectResponse().GetStatus(), "%v", tc)
		assert.Equal(t, RefusalNameNotInNamespace, r[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue(), "%v", tc)
		assert.Nil(t, r[0].GetRoute(), "%v: the no-match arm delivers", tc)
	}
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

// shortArmRoutes returns the routes a short name's matcher selects for ns.
func shortArmRoutes(t *testing.T, m *xdsmatcherv3.Matcher, ns string) []*routev3.Route {
	t.Helper()
	on := m.GetOnNoMatch()
	if arm, ok := m.GetMatcherTree().GetExactMatchMap().GetMap()[ns]; ok && ns != "" {
		on = arm
	}
	require.NotNil(t, on)
	var list routev3.RouteList
	require.NoError(t, on.GetAction().GetTypedConfig().UnmarshalTo(&list))
	return list.GetRoutes()
}
