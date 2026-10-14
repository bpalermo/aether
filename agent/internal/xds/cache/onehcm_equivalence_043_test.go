package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/xdseval"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Gate (b) of the one-HCM capture listener: for every name, every dialled
// port (each HTTP port number of the mesh, one other, and none) and every Host
// spelling (port-less, ":<dialled>", another port, the port twice), the one
// published table, cap_http, gives the answer the per-port table of the
// dialled port gives. The per-port tables are what chart 3.0.0 published, and
// are still built from the same cache inputs: cap_http is compiled from them.
//
// Every test of this package that reads route tables through a043Tables or
// perPortView runs the same check on its own state (the walks run it on
// thousands); this one runs it on the shapes the change is most likely to get
// wrong, and shows the check fails when the compiled table is wrong.

// oneHCMFixtures builds caches whose capture tables carry every kind of
// virtual host: routed, refused (each reason), mismatch, cold (no endpoint
// held), mesh-only, short names, route rules, a service-wide filter, imported
// rules, a user Service's port map with a named targetPort that has not
// resolved, and TLSRoutes on hostile shapes.
func oneHCMFixtures(t *testing.T) map[string]*SnapshotCache {
	t.Helper()
	ctx := context.Background()
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	const filter = "envoy.filters.http.header_mutation"
	out := map[string]*SnapshotCache{}

	full := a043Cache(t, a043Fixture())
	full.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/multi":  {{Matches: []proxy.GammaMatch{{Prefix: "/x"}}, HeaderMutation: rtpMark("x")}, {ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("9090")}},
		"demo/eighty": {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: "multi.demo." + a043Mesh, Weight: 1, Port: 9090}}}},
	})
	full.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{"demo/httponly": {{Imported: true, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("imported")}}})
	full.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": {Name: filter, Config: cfg}, "demo/tcpfirst": {Name: filter, Config: cfg}})
	full.SetServicePortMaps(map[string]map[uint32]uint32{"demo/userowned": {80: 8080, 8000: 0}})
	full.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/userowned": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("user")}}})
	out["fixture with rules, a filter, imports and a user Service"] = full

	hostile := a043Cache(t, append(a043Fixture(), shpHostileFixture()...))
	hostile.SetTLSServiceRoutes(shpTLSRoutes("demo/w80", "demo/m81", "demo/tcp80", "demo/httponly", "demo/tmulti", "demo/l82"))
	out["hostile shapes with TLSRoutes"] = hostile

	cold := newTestCache("node-1")
	cold.SetCaptureEnabled(true)
	cold.SetCaptureRedirectAll(true)
	a043ProjectNamesOnly(cold, a043Fixture())
	cold.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": {Name: filter, Config: cfg}})
	cold.SetRouteTargetPorts(map[string][]uint32{"demo/multi": {8080, 9090}})
	cold.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("cold")}}})
	out["no endpoint held, names only"] = cold

	scoped := a043Cache(t, a043Fixture())
	scoped.SetCaptureRedirectAll(false)
	out["scoped capture"] = scoped

	for name, c := range out {
		require.NoError(t, c.generateSnapshot(ctx), name)
	}
	return out
}

func TestOneHCM_CompiledTableAnswersAsThePerPortTables(t *testing.T) {
	for name, c := range oneHCMFixtures(t) {
		t.Run(name, func(t *testing.T) {
			merged := a043Published(t, c)
			perPort, other := c.capturePerPortTables(c.captureTableInputs(), c.captureHTTPPorts())
			require.Greater(t, len(perPort), 2, "the fixture has HTTP port numbers")
			requireCompiledEquivalent(t, merged, perPort, other)

			// Not vacuous: the requests the check makes see routed,
			// refused, mismatched and short-name answers alike.
			seen := map[string]bool{}
			for _, rc := range perPort {
				for _, vh := range rc.GetVirtualHosts() {
					_, kind, _ := strings.Cut(vh.GetName(), "|")
					seen[strings.SplitN(kind, "|", 2)[0]] = true
				}
			}
			t.Logf("%s: %d per-port tables, kinds of virtual host %v, compiled into %d virtual hosts (%d bytes vs %d)",
				name, len(perPort), slices.Sorted(maps.Keys(seen)), len(merged.GetVirtualHosts()), proto.Size(merged), sizeOfTables(perPort, other))
			assert.True(t, seen["mismatch"], "no mismatch virtual host")
		})
	}
}

func sizeOfTables(perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration) int {
	n := proto.Size(other)
	for _, rc := range perPort {
		n += proto.Size(rc)
	}
	return n
}

// TestOneHCM_EquivalenceCheckSeesAWrongCompiledTable is gate (b) seen red:
// each mutation is a plausible defect of the compiler, and the check must
// report it.
func TestOneHCM_EquivalenceCheckSeesAWrongCompiledTable(t *testing.T) {
	c := oneHCMFixtures(t)["fixture with rules, a filter, imports and a user Service"]
	merged := a043Published(t, c)
	perPort, other := c.capturePerPortTables(c.captureTableInputs(), c.captureHTTPPorts())
	require.Empty(t, compiledMismatches(merged, perPort, other, 1))

	mutate := func(name string, f func(rc *routev3.RouteConfiguration) bool) {
		t.Run(name, func(t *testing.T) {
			bad := proto.Clone(merged).(*routev3.RouteConfiguration)
			require.True(t, f(bad), "the mutation found nothing to change")
			got := compiledMismatches(bad, perPort, other, 3)
			assert.NotEmpty(t, got, "the check did not see the defect")
			t.Logf("seen: %v", got)
		})
	}
	vhost := func(rc *routev3.RouteConfiguration, name string) *routev3.VirtualHost {
		for _, vh := range rc.GetVirtualHosts() {
			if vh.GetName() == name {
				return vh
			}
		}
		return nil
	}
	const multi = "multi.demo." + a043Mesh

	mutate("a dialled port loses its branch (answered as any other port)", func(rc *routev3.RouteConfiguration) bool {
		m := vhost(rc, multi).GetMatcher().GetMatcherTree().GetExactMatchMap().GetMap()
		_, ok := m["8080"]
		delete(m, "8080")
		return ok
	})
	mutate("two dialled ports' branches swapped", func(rc *routev3.RouteConfiguration) bool {
		m := vhost(rc, multi).GetMatcher().GetMatcherTree().GetExactMatchMap().GetMap()
		if m["8080"] == nil || m["9090"] == nil {
			return false
		}
		m["8080"], m["9090"] = m["9090"], m["8080"]
		return true
	})
	mutate("the Host checked by suffix (the prototype's check): <name>:8080:8080 is served", func(rc *routev3.RouteConfiguration) bool {
		changed := false
		walkStringMatchers(vhost(rc, multi).GetMatcher(), func(sm *xdsmatcherv3.StringMatcher) {
			if e := sm.GetExact(); strings.HasSuffix(e, ":8080") {
				sm.MatchPattern = &xdsmatcherv3.StringMatcher_Suffix{Suffix: ":8080"}
				changed = true
			}
		})
		return changed
	})
	mutate("the Host compared case-sensitively", func(rc *routev3.RouteConfiguration) bool {
		changed := false
		walkStringMatchers(vhost(rc, multi).GetMatcher(), func(sm *xdsmatcherv3.StringMatcher) {
			if sm.GetIgnoreCase() {
				sm.IgnoreCase = false
				changed = true
			}
		})
		return changed
	})
	mutate("no dialled port falls to a port's answer instead of the others'", func(rc *routev3.RouteConfiguration) bool {
		vh := vhost(rc, multi)
		vh.GetMatcher().OnNoMatch = vh.GetMatcher().GetMatcherTree().GetExactMatchMap().GetMap()["8080"]
		return vh.GetMatcher().GetOnNoMatch() != nil
	})
	mutate("the catch-all offers every dialled port the same on-demand spelling", func(rc *routev3.RouteConfiguration) bool {
		vh := vhost(rc, "on_demand_catch_all")
		m := vh.GetMatcher().GetMatcherTree().GetExactMatchMap().GetMap()
		if m["8080"] == nil {
			return false
		}
		for k := range m {
			m[k] = m["8080"]
		}
		return true
	})
}

func walkStringMatchers(m *xdsmatcherv3.Matcher, f func(*xdsmatcherv3.StringMatcher)) {
	if m == nil {
		return
	}
	var pred func(p *xdsmatcherv3.Matcher_MatcherList_Predicate)
	pred = func(p *xdsmatcherv3.Matcher_MatcherList_Predicate) {
		if sp := p.GetSinglePredicate(); sp != nil && sp.GetValueMatch() != nil {
			f(sp.GetValueMatch())
		}
		for _, q := range p.GetOrMatcher().GetPredicate() {
			pred(q)
		}
		for _, q := range p.GetAndMatcher().GetPredicate() {
			pred(q)
		}
		if p.GetNotMatcher() != nil {
			pred(p.GetNotMatcher())
		}
	}
	om := func(o *xdsmatcherv3.Matcher_OnMatch) { walkStringMatchers(o.GetMatcher(), f) }
	for _, fm := range m.GetMatcherList().GetMatchers() {
		pred(fm.GetPredicate())
		om(fm.GetOnMatch())
	}
	for _, o := range m.GetMatcherTree().GetExactMatchMap().GetMap() {
		om(o)
	}
	om(m.GetOnNoMatch())
}

// TestOneHCM_NoDialledPortFailsClosed is control 2 at the table: on a
// connection that carries no dialled port (a chain that does not set it), no
// Host that names a mesh service the node knows is delivered to a mesh
// cluster: it is refused, or leaves as a name that is not the mesh's would.
// Never served on a port it was not dialled on.
func TestOneHCM_NoDialledPortFailsClosed(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	merged := a043Published(t, c)
	table := xdseval.NewTable(merged)
	checked := 0
	for _, s := range services {
		mesh, kube := a043Names(s, a043BareIsUnique(services, s))
		for _, name := range append([]string{mesh}, kube...) {
			for _, suffix := range []string{"", ":80", ":8080", ":9090", ":9000", ":7070"} {
				a, err := table.Resolve(xdseval.Request{Host: name + suffix, FilterState: map[string]string{proxy.CallerNamespaceFilterStateKey: s.ns}})
				require.NoError(t, err)
				for _, r := range a.Routes {
					o := xdseval.OutcomeOf(r)
					assert.Falsef(t, o.Cluster != "" && o.Cluster != proxy.PassthroughClusterName || o.ClusterHeader != "" || len(o.Weighted) > 0,
						"Host %s with no dialled port reaches %+v", name+suffix, o)
				}
				if first, _ := xdseval.FirstRoute(a.Routes, xdseval.Request{Host: name + suffix}); first != nil && first.GetDirectResponse() != nil {
					assert.Equal(t, uint32(421), first.GetDirectResponse().GetStatus(), "Host %s", name+suffix)
				}
				checked++
			}
		}
	}
	require.Greater(t, checked, 50)
	// Seen red: the same request WITH the dialled port is delivered.
	a, err := table.Resolve(xdseval.Request{Host: "multi.demo." + a043Mesh + ":8080", FilterState: map[string]string{proxy.DialledPortFilterStateKey: "8080"}})
	require.NoError(t, err)
	first, err := xdseval.FirstRoute(a.Routes, xdseval.Request{Host: "multi.demo." + a043Mesh + ":8080"})
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.NotNil(t, first.GetRoute(), "with the dialled port 8080 the request is delivered: %v", first)
	_ = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	_ = fmt.Sprint
}
