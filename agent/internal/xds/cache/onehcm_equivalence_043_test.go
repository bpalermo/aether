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
	"aethermesh.dev/common/extensionfilter"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
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
	stamp := func(key string) *anypb.Any {
		a, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
			RequestMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Remove{Remove: key}}},
		}})
		require.NoError(t, err)
		return a
	}
	cfg := stamp("x-service")
	const filter = "envoy.filters.http.header_mutation"
	out := map[string]*SnapshotCache{}

	full := a043Cache(t, a043Fixture())
	full.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/multi": {
			// A rule's ExtensionRef of the service-wide filter's kind: the
			// proxy combines the two levels.
			{Matches: []proxy.GammaMatch{{Prefix: "/x"}}, HeaderMutation: rtpMark("x"), ExtensionFilters: []proxy.ExtensionFilter{{Name: filter, Config: stamp("x-rule")}}},
			{ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("9090")},
		},
		"demo/eighty":    {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: "multi.demo." + a043Mesh, Weight: 1, Port: 9090}}}},
		"demo/userowned": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("user")}},
	})
	full.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{"demo/httponly": {{Imported: true, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("imported")}}})
	full.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": {Name: filter, Config: cfg}, "demo/tcpfirst": {Name: filter, Config: cfg}})
	full.SetServicePortMaps(map[string]map[uint32]uint32{"demo/userowned": {80: 8080, 8000: 0}})
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
			requireCompiledEquivalentAllPaths(t, merged, perPort, other)

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
	require.Empty(t, compiledMismatches(merged, perPort, other, 1, true))

	mutate := func(name string, f func(rc *routev3.RouteConfiguration) bool) {
		t.Run(name, func(t *testing.T) {
			bad := proto.Clone(merged).(*routev3.RouteConfiguration)
			require.True(t, f(bad), "the mutation found nothing to change")
			got := compiledMismatches(bad, perPort, other, 3, true)
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
	dialled := func(r *routev3.Route) string {
		for _, fs := range r.GetMatch().GetFilterState() {
			if fs.GetKey() == proxy.DialledPortFilterStateKey {
				return fs.GetStringMatch().GetExact()
			}
		}
		return ""
	}
	setDialled := func(r *routev3.Route, port string) {
		for _, fs := range r.GetMatch().GetFilterState() {
			if fs.GetKey() == proxy.DialledPortFilterStateKey {
				fs.GetStringMatch().MatchPattern = &typematcherv3.StringMatcher_Exact{Exact: port}
			}
		}
	}

	mutate("a dialled port loses its routes (answered as any other port)", func(rc *routev3.RouteConfiguration) bool {
		vh := vhost(rc, multi)
		before := len(vh.GetRoutes())
		vh.Routes = slices.DeleteFunc(vh.Routes, func(r *routev3.Route) bool { return dialled(r) == "8080" })
		return len(vh.GetRoutes()) < before
	})
	mutate("two dialled ports' routes swapped", func(rc *routev3.RouteConfiguration) bool {
		n := 0
		for _, r := range vhost(rc, multi).GetRoutes() {
			switch dialled(r) {
			case "8080":
				setDialled(r, "9090")
				n++
			case "9090":
				setDialled(r, "8080")
				n++
			}
		}
		return n > 0
	})
	mutate("a route of port 9090 shadows port 8080", func(rc *routev3.RouteConfiguration) bool {
		vh := vhost(rc, multi)
		for _, r := range vh.GetRoutes() {
			// The rule only port 9090 has (the "/x" rule is on both ports,
			// alike: one shadowing the other would change nothing).
			if dialled(r) == "9090" && r.GetRoute() != nil && slices.ContainsFunc(r.GetResponseHeadersToAdd(), func(h *corev3.HeaderValueOption) bool { return h.GetHeader().GetValue() == "9090" }) {
				shadow := proto.Clone(r).(*routev3.Route)
				setDialled(shadow, "8080")
				vh.Routes = append([]*routev3.Route{shadow}, vh.Routes...)
				return true
			}
		}
		return false
	})
	mutate("the dialled-port predicate dropped from one route", func(rc *routev3.RouteConfiguration) bool {
		// 8080's fallback route: without the predicate it takes 9090's
		// requests, which port 9090's own rule answers otherwise.
		var last *routev3.Route
		for _, r := range vhost(rc, multi).GetRoutes() {
			if dialled(r) == "8080" && r.GetRoute() != nil {
				last = r
			}
		}
		if last == nil {
			return false
		}
		last.Match.FilterState = nil
		return true
	})
	mutate("the Host checked by suffix (the prototype's check): <name>:8080:8080 is served", func(rc *routev3.RouteConfiguration) bool {
		changed := false
		for _, r := range vhost(rc, multi).GetRoutes() {
			for _, h := range r.GetMatch().GetHeaders() {
				if h.GetName() == ":authority" && strings.Contains(h.GetStringMatch().GetSafeRegex().GetRegex(), "8080") {
					h.GetStringMatch().GetSafeRegex().Regex = "(^[^:]+$)|(:8080$)"
					changed = true
				}
			}
		}
		return changed
	})
	mutate("the Host condition dropped from a port's routes", func(rc *routev3.RouteConfiguration) bool {
		changed := false
		for _, r := range vhost(rc, multi).GetRoutes() {
			if dialled(r) == "8080" && len(r.GetMatch().GetHeaders()) > 0 {
				r.Match.Headers = slices.DeleteFunc(r.Match.Headers, func(h *routev3.HeaderMatcher) bool { return h.GetName() == ":authority" })
				changed = true
			}
		}
		return changed
	})
	// The service-wide filter: a virtual-host config, which each refusal
	// route of the same virtual host disables.
	disables := func(a *anypb.Any) bool {
		fc := &routev3.FilterConfig{}
		return a.MessageIs(fc) && a.UnmarshalTo(fc) == nil && fc.GetDisabled()
	}
	mutate("the service-wide filter folded into each route (the levels as one config)", func(rc *routev3.RouteConfiguration) bool {
		changed := false
		for _, vh := range rc.GetVirtualHosts() {
			for name, cfg := range vh.GetTypedPerFilterConfig() {
				for _, r := range vh.GetRoutes() {
					if own := r.GetTypedPerFilterConfig()[name]; own != nil && disables(own) {
						delete(r.TypedPerFilterConfig, name)
						continue
					}
					folded, err := extensionfilter.CombineLevels(name, cfg, r.GetTypedPerFilterConfig()[name])
					require.NoError(t, err)
					if r.TypedPerFilterConfig == nil {
						r.TypedPerFilterConfig = map[string]*anypb.Any{}
					}
					r.TypedPerFilterConfig[name] = folded
				}
				changed = true
			}
			vh.TypedPerFilterConfig = nil
		}
		return changed
	})
	mutate("a refusal runs the service-wide filter (its disable dropped)", func(rc *routev3.RouteConfiguration) bool {
		changed := false
		for _, vh := range rc.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				for name, a := range r.GetTypedPerFilterConfig() {
					if disables(a) {
						delete(r.TypedPerFilterConfig, name)
						changed = true
					}
				}
			}
		}
		return changed
	})
	mutate("the catch-all offers every dialled port the same on-demand spelling", func(rc *routev3.RouteConfiguration) bool {
		var want string
		routes := vhost(rc, "on_demand_catch_all").GetRoutes()
		for _, r := range routes {
			if dialled(r) == "8080" && r.GetRoute().GetClusterHeader() != "" {
				want = r.GetMatch().GetHeaders()[0].GetStringMatch().GetSafeRegex().GetRegex()
			}
		}
		changed := false
		for _, r := range routes {
			if r.GetRoute().GetClusterHeader() != "" && want != "" {
				r.GetMatch().GetHeaders()[0].GetStringMatch().GetSafeRegex().Regex = want
				changed = changed || dialled(r) != "8080"
			}
		}
		return changed
	})
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
				req := xdseval.Request{Host: name + suffix, FilterState: map[string]string{proxy.CallerNamespaceFilterStateKey: s.ns}}
				a, err := table.Resolve(req)
				require.NoError(t, err)
				for _, path := range []string{"/", "/x", "/v2"} {
					req.Path = path
					first, err := xdseval.FirstRoute(a.Routes, req)
					require.NoError(t, err)
					o := xdseval.OutcomeOf(first)
					assert.Falsef(t, o.Cluster != "" && o.Cluster != proxy.PassthroughClusterName || o.ClusterHeader != "" || len(o.Weighted) > 0,
						"Host %s with no dialled port reaches %+v", name+suffix, o)
					if first.GetDirectResponse() != nil && first.GetMatch().GetPath() != proxy.MeshLivePath {
						assert.Equal(t, uint32(421), o.Status, "Host %s", name+suffix)
					}
				}
				checked++
			}
		}
	}
	require.Greater(t, checked, 50)
	// Seen red: the same request WITH the dialled port is delivered.
	a, err := table.Resolve(xdseval.Request{Host: "multi.demo." + a043Mesh + ":8080", FilterState: map[string]string{proxy.DialledPortFilterStateKey: "8080"}})
	require.NoError(t, err)
	first, err := xdseval.FirstRoute(a.Routes, xdseval.Request{Host: "multi.demo." + a043Mesh + ":8080", FilterState: map[string]string{proxy.DialledPortFilterStateKey: "8080"}})
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.NotNil(t, first.GetRoute(), "with the dialled port 8080 the request is delivered: %v", first)
	_ = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	_ = fmt.Sprint
}
