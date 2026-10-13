package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findVHost returns the first virtual host whose domain set contains want.
func findVHost(vhosts []*routev3.VirtualHost, want string) *routev3.VirtualHost {
	for _, vh := range vhosts {
		for _, d := range vh.GetDomains() {
			if d == want {
				return vh
			}
		}
	}
	return nil
}

// captureTable returns the virtual hosts of the capture route table of dialled
// port port (0 for cap_http_other), without the table's catch-all.
func captureTable(c *SnapshotCache, port uint32) []*routev3.VirtualHost {
	return c.captureTableVhosts(c.captureTableInputs(), port)
}

// TestCaptureVhosts_RouteTargetRealPort verifies that a GAMMA route TARGET with no
// SA-backed mesh Service of its own (proposal 023) emits a cap_http vhost whose
// domains include the REAL Service port (M2) — not just the mesh :18081 spelling —
// so a client dialing echo:8080 host-matches and the path-based GAMMA routing fires.
func TestCaptureVhosts_RouteTargetRealPort(t *testing.T) {
	c := newTestCache("node-1")

	// "team-a/echo" is a pure route target: /v2 -> echo-v2, / -> echo-v1.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {
			{
				Matches:  []proxy.GammaMatch{{Prefix: "/v2"}},
				Backends: []proxy.GammaBackend{{Service: "team-a/echo-v2", Cluster: "echo-v2.team-a.aether.internal", Weight: 1}},
			},
			{
				Matches:  []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 1}},
			},
		},
	})
	// M2: the route target is addressed on its real Service port 8080.
	c.SetRouteTargetPorts(map[string][]uint32{"team-a/echo": {8080}})

	// Proposal 043: the dialled port selects the table. The real port's
	// spellings are in the table of port 8080, with the port-less ones (a
	// port-less Host on a dialled port means that port).
	vhosts := captureTable(c, 8080)

	// The cluster.local authority at the real port must host-match.
	vh := findVHost(vhosts, "echo.team-a.svc.cluster.local:8080")
	require.NotNil(t, vh, "route target must carry a :realPort cluster.local domain (M2); got vhosts=%v", domainsOf(vhosts))

	domains := vh.GetDomains()
	assert.Contains(t, domains, "echo.team-a.svc.cluster.local")
	assert.Contains(t, domains, "echo.team-a.aether.internal")
	assert.Contains(t, domains, "echo.team-a.svc.cluster.local:8080")
	assert.Contains(t, domains, "echo.team-a.aether.internal:8080")
	// 18081 is the proxy's port, not a port of the target: no table of a
	// dialled port holds a virtual host that spells it. (Until chart 2.7.0
	// "<name>:18081" and the port-less name on a dial to 80 were served with
	// the same rules, as "the port the pods declare".)
	for _, port := range []uint32{80, 8080, 9090} {
		for _, d := range domainsOf(captureTable(c, port)) {
			assert.NotContainsf(t, d, ":18081", "table %d", port)
		}
	}
	// And a table of a port the target is not addressed on has no routing
	// virtual host for it: 9090, and 80, which the target does not declare.
	for _, port := range []uint32{80, 9090} {
		_, routed := routedCluster(c, port, "echo.team-a.svc.cluster.local")
		assert.Falsef(t, routed, "table %d routes a name on a port it is not addressed on", port)
	}

	// The GAMMA rules must be present and path-ordered by specificity: the most
	// specific match (/v2, a segment-boundary prefix) is first and routes to echo-v2;
	// the "/" rule routes to echo-v1. (Gateway API mandates specificity ordering, and
	// "/v2" lowers to Envoy path_separated_prefix, not a raw Prefix.)
	routes := vh.GetRoutes()
	require.GreaterOrEqual(t, len(routes), 2, "both GAMMA rules projected onto the route target vhost")
	assert.Equal(t, "/v2", routes[0].GetMatch().GetPathSeparatedPrefix(),
		"the most specific match (/v2) is evaluated first")
	assert.Equal(t, "echo-v2.team-a.aether.internal", routes[0].GetRoute().GetCluster(),
		"/v2 routes to the echo-v2 backend cluster")
	// The "/" GAMMA rule routes to echo-v1 (distinct from the trailing default route,
	// which falls back to the route-target mesh cluster).
	var sawV1 bool
	for _, rt := range routes {
		if rt.GetRoute().GetCluster() == "echo-v1.team-a.aether.internal" {
			sawV1 = true
		}
	}
	assert.True(t, sawV1, "the '/' rule routes to echo-v1")
}

// TestCaptureVhosts_SABackedRouteTargetShortNames reproduces the MESH-HTTP
// MeshFrontend scenario: an HTTPRoute (with a ResponseHeaderModifier filter)
// whose parentRef is a Service that IS its own SA-backed mesh Service and its own
// backend (the "echo-v2" route target — a real backend, not a versioned-fanout
// pseudo-target). The client dials the route target by its BARE short name
// ("echo-v2", port 80 implied) so the captured :authority is "echo-v2". The
// cap_http vhost that carries the GAMMA rules must host-match every short-name +
// real-port spelling, exactly like a pure route target — otherwise the dial misses
// the vhost, falls through to passthrough, and the GAMMA header mutation is never
// applied (the X-Header-Set assertion fails).
func TestCaptureVhosts_SABackedRouteTargetShortNames(t *testing.T) {
	c := newTestCache("node-1")

	// echo-v2 is BOTH an SA-backed mesh authority (it has its own pods/Service)
	// AND the route target the HTTPRoute attaches to. The reconciler projects its
	// cluster.local FQDN into captureAuthorities.
	c.SetCaptureAuthorities(map[string]string{
		"gateway-conformance-mesh/echo-v2": "echo-v2.gateway-conformance-mesh.svc.cluster.local",
	})
	// The GAMMA rule on the echo-v2 route target (a single self-backend rule with a
	// header mutation — the ResponseHeaderModifier in MeshFrontend).
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"gateway-conformance-mesh/echo-v2": {
			{
				Backends: []proxy.GammaBackend{{
					Service: "gateway-conformance-mesh/echo-v2",
					Cluster: "echo-v2.gateway-conformance-mesh.aether.internal",
					Weight:  1,
				}},
			},
		},
	})
	// The route target is addressed on its real Service port 80 (echo Service :80).
	c.SetRouteTargetPorts(map[string][]uint32{"gateway-conformance-mesh/echo-v2": {80}})

	vhosts := captureTable(c, 80)

	// The client dials the bare short name (same-namespace) with no port -> :authority
	// "echo-v2". With a single owner the bare spelling must be emitted, AND the
	// real-port spelling for clients that present "echo-v2:80".
	require.NotNil(t, findVHost(vhosts, "echo-v2"),
		"SA-backed route target must host-match its bare short name; got vhosts=%v", domainsOf(vhosts))

	vh := findVHost(vhosts, "echo-v2.gateway-conformance-mesh.svc.cluster.local")
	require.NotNil(t, vh)
	domains := vh.GetDomains()
	assert.Contains(t, domains, "echo-v2")
	assert.Contains(t, domains, "echo-v2.gateway-conformance-mesh")
	assert.Contains(t, domains, "echo-v2.gateway-conformance-mesh.svc")
	assert.Contains(t, domains, "echo-v2:80")
	assert.Contains(t, domains, "echo-v2.gateway-conformance-mesh.svc.cluster.local:80")

	// The GAMMA rule (header mutation) must be projected onto the vhost — the route
	// carries a backend cluster (so the mutation lowers to ResponseHeadersToAdd on
	// the matched route, not the passthrough default).
	routes := vh.GetRoutes()
	require.GreaterOrEqual(t, len(routes), 1)
	var sawBackend bool
	for _, rt := range routes {
		if rt.GetRoute().GetCluster() == "echo-v2.gateway-conformance-mesh.aether.internal" {
			sawBackend = true
		}
	}
	assert.True(t, sawBackend, "the route target's GAMMA rule routes to its backend cluster")
}

// TestCaptureVhosts_RouteTargetNoPort verifies a route target whose parentRef
// declares no port: the rule applies on every port of the parent Service, and
// the ports are the Service's own (its port map, which the reconciler reads
// for every Service a route names). No spurious :0 domain, and no port the
// Service does not have: a dial to port 80 of a Service without one is not
// read as "the port the pods declare".
func TestCaptureVhosts_RouteTargetNoPort(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 1}}}},
	})
	// No SetRouteTargetPorts (or empty): the parentRef names no port.

	// Nothing says the name has a port 80: nothing routes there.
	_, routed := routedCluster(c, 80, "echo.team-a.svc.cluster.local")
	assert.False(t, routed, "no port map, no declared port: a dial to 80 is not the port the pods declare")

	// The Service maps 80 and 8080: each has the rule, in its own table.
	c.SetServicePortMaps(map[string]map[uint32]uint32{"team-a/echo": {80: 8080, 8080: 8080}})
	for _, port := range []uint32{80, 8080} {
		vhosts := captureTable(c, port)
		vh := findVHost(vhosts, "echo.team-a.svc.cluster.local")
		require.NotNil(t, vh, "table %d", port)
		assert.Equal(t, "echo-v1.team-a.aether.internal", vh.GetRoutes()[0].GetRoute().GetCluster(), "table %d: the rule", port)
		for _, d := range domainsOf(vhosts) {
			assert.NotContains(t, d, ":0", "no spurious :0 domain when the parentRef omits a port")
		}
	}
	_, routed = routedCluster(c, 9090, "echo.team-a.svc.cluster.local")
	assert.False(t, routed, "a port the Service does not have")
}

// routedCluster returns the cluster the virtual host of table port that
// host-matches authority routes its last (default) route to, and false when no
// virtual host of the table claims the authority or the one that does is a
// refusal.
func routedCluster(c *SnapshotCache, port uint32, authority string) (string, bool) {
	vh := findVHost(captureTable(c, port), authority)
	if vh == nil || len(vh.GetRoutes()) == 0 {
		return "", false
	}
	last := vh.GetRoutes()[len(vh.GetRoutes())-1]
	if last.GetRoute() == nil {
		return "", false
	}
	return last.GetRoute().GetCluster(), true
}

// TestCaptureVhosts_DemandScoped locks the M4 invariant (proposal 022/004): the
// cap_http table is bounded by the node dependency set, NOT the cluster-wide set of
// mesh authorities. This is what keeps redirect-all (now the managed-pod default)
// from exploding cap_http to every mesh Service on every node — Envoy only carries
// routes/clusters for destinations this node actually depends on; everything else
// rides the catch-all (ODCDS cold path / passthrough). The cold path then pulls a
// newly-used service into scope on demand.
func TestCaptureVhosts_DemandScoped(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	// Two real mesh authorities exist cluster-wide (the mesh-Service reconciler feeds
	// the full set, independent of any one node's dependency set).
	c.SetCaptureAuthorities(map[string]string{
		"default/in-scope":  "in-scope.default.svc.cluster.local",
		"default/off-scope": "off-scope.default.svc.cluster.local",
	})
	// A local pod declares only in-scope as an upstream, so only it enters this
	// node's dependency set.
	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/in-scope"), "example.org"))

	// Neither service has a known port yet, so each name can only have its
	// stand-in virtual host: the mesh authority that spells the dialled port.
	// (The table of port 8080 here; any per-port table is scoped alike.)
	inScope := proxy.ServiceClusterName("default/in-scope", c.meshDomain) + ":8080"
	offScope := proxy.ServiceClusterName("default/off-scope", c.meshDomain) + ":8080"
	vhosts := captureTable(c, 8080)
	require.NotNil(t, findVHost(vhosts, inScope),
		"in-scope authority must build a cap_http vhost; got %v", domainsOf(vhosts))
	assert.Nil(t, findVHost(vhosts, offScope),
		"off-scope authority must be excluded from cap_http (demand-scoped), even though it is a known mesh authority")

	// Cold path: once off-scope is observed via ODCDS it joins the dependency set,
	// so capture scope follows demand and its vhost now builds.
	require.True(t, c.ObserveDependency(ctx, "default/off-scope"))
	vhosts = captureTable(c, 8080)
	assert.NotNil(t, findVHost(vhosts, offScope),
		"observed (ODCDS) authority must now build a cap_http vhost — scope follows demand")
}

// TestCaptureTables_StableAcrossGAMMAChurn is the regression guard for the
// MESH-HTTP RDS-reload convergence race. A mesh service's virtual hosts are
// derived from captureAuthorities (fed by the generated mesh Services,
// INDEPENDENT of HTTPRoute lifecycle), so an in-scope mesh authority keeps a
// virtual host for every one of its names even after its GAMMA rules are
// cleared (the delete half of a suite's apply/delete churn). A captured
// request to that authority therefore never falls to the ORIGINAL_DST
// passthrough → kube-proxy.
//
// Until proposal 043 this was the job of a "known-target" regex on the
// catch-all, which accepted any port and routed it to the default cluster
// (#1682). The per-port tables do it by construction.
func TestCaptureTables_StableAcrossGAMMAChurn(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)

	const (
		svc     = "gateway-conformance-mesh/echo-v2"
		cluster = "echo-v2.gateway-conformance-mesh.aether.internal"
	)
	// The mesh-Service reconciler projects the SA-backed authority and its
	// address, which puts the service in the node's dependency set whatever
	// routes exist. This is stable across HTTPRoute churn.
	c.SetCaptureAuthorities(map[string]string{
		svc: "echo-v2.gateway-conformance-mesh.svc.cluster.local",
	})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: svc, ClusterIP: "10.96.0.20"}})
	// The user's Service that owns the name maps port 80 to the pods' 8080.
	// That is what makes port 80 a port of "echo-v2" at all (a dial to 80 is
	// port 80 and nothing else), and it is a fact of the SERVICE: it must be
	// known whether or not a route names the Service, or the delete half of
	// the churn turns every request below into a refusal.
	c.SetServicePortMaps(map[string]map[uint32]uint32{svc: {80: 8080, 8080: 8080}})
	// A GAMMA rule is applied (the route exists).
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		svc: {{Backends: []proxy.GammaBackend{{Service: svc, Cluster: cluster, Weight: 1}}}},
	})

	// What a client that dials port 80 can present as its authority.
	dialSpellings := []string{
		"echo-v2",
		"echo-v2:80",
		"echo-v2.gateway-conformance-mesh",
		"echo-v2.gateway-conformance-mesh.svc",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local:80",
		"echo-v2.gateway-conformance-mesh.aether.internal",
	}

	// With the route applied, every spelling reaches the cluster.
	for _, a := range dialSpellings {
		got, ok := routedCluster(c, 80, a)
		require.Truef(t, ok, "with rules: %q must have a routing virtual host in the table of port 80", a)
		assert.Equalf(t, cluster, got, "with rules: %q -> %s", a, cluster)
	}

	// Now SIMULATE THE CHURN: the suite deletes the HTTPRoute, the gamma reconciler
	// reconciles with the route gone -> serviceRoutes for echo-v2 is cleared. The
	// mesh Service is untouched, so captureAuthorities still carries echo-v2.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{})

	// Every spelling STILL has its virtual host and still reaches the cluster,
	// so a captured request stays in the mesh, never the passthrough.
	for _, a := range dialSpellings {
		got, ok := routedCluster(c, 80, a)
		require.Truef(t, ok, "after churn: %q must keep its virtual host (no passthrough leak)", a)
		assert.Equalf(t, cluster, got, "after churn: %q -> %s", a, cluster)
	}

	// Negative control: a name that is not the service's is claimed by no
	// virtual host, so the test above cannot pass on a wildcard.
	_, ok := routedCluster(c, 80, "echo-v3.gateway-conformance-mesh.svc.cluster.local")
	assert.False(t, ok, "an authority that names no mesh service has no virtual host")
}

// TestCaptureTables_RouteOnlyTarget covers the versioned-fanout shape: a GAMMA
// route TARGET with no SA-backed mesh Service of its own (only in serviceRoutes).
// Its cluster.local authority is derived from the namespace-qualified key and it
// has a virtual host for every name, so the passthrough never shadows it.
func TestCaptureTables_RouteOnlyTarget(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)

	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/v1"}}, Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 1}}}},
	})
	// The Service the route is parented to has port 80 (the reconciler reads
	// the port map of every Service a route names).
	c.SetServicePortMaps(map[string]map[uint32]uint32{"team-a/echo": {80: 8080}})

	cluster, ok := routedCluster(c, 80, "echo.team-a.svc.cluster.local")
	require.True(t, ok, "route-only target must have a virtual host")
	assert.Equal(t, "echo.team-a.aether.internal", cluster)
	cluster, ok = routedCluster(c, 80, "echo")
	require.True(t, ok, "bare same-namespace dial must have a virtual host")
	assert.Equal(t, "echo.team-a.aether.internal", cluster)
	// A Host that names a port other than the one dialled is refused, where
	// the known-target regex routed any port to the default cluster (#1682).
	_, ok = routedCluster(c, 80, "echo.team-a.svc.cluster.local:8080")
	assert.False(t, ok, "no virtual host of table 80 routes an authority that names port 8080")
	mismatch := findVHost(captureTable(c, 80), "echo.team-a.svc.cluster.local:*")
	require.NotNil(t, mismatch, "the other-port spellings are claimed by the port-mismatch refusal")
	assert.Equal(t, uint32(421), mismatch.GetRoutes()[0].GetDirectResponse().GetStatus())
	assert.Equal(t, proxy.RefusalPortMismatch, mismatch.GetRoutes()[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
}

// TestCaptureTables_FallthroughFollowsRedirectAll: with scoped capture (no
// redirect-all) there is no passthrough cluster, so a table's catch-all ends in
// a hard 404; with redirect-all it ends in the ORIGINAL_DST passthrough. The
// service's own virtual hosts are the same in both.
func TestCaptureTables_FallthroughFollowsRedirectAll(t *testing.T) {
	for _, redirectAll := range []bool{false, true} {
		c := newTestCache("node-1")
		c.SetCaptureEnabled(true)
		c.SetCaptureRedirectAll(redirectAll)
		c.SetCaptureAuthorities(map[string]string{
			"team-a/echo": "echo.team-a.svc.cluster.local",
		})
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{
			"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo", Cluster: "echo.team-a.aether.internal", Weight: 1}}}},
		})
		c.SetServicePortMaps(map[string]map[uint32]uint32{"team-a/echo": {80: 8080}})
		var table *routev3.RouteConfiguration
		for _, r := range c.captureRouteTables([]uint32{80}) {
			if rc := r.(*routev3.RouteConfiguration); rc.GetName() == "cap_http_80" {
				table = rc
			}
		}
		require.NotNil(t, table)
		require.NotNil(t, findVHost(table.GetVirtualHosts(), "echo.team-a.svc.cluster.local"))
		catchAll := table.GetVirtualHosts()[len(table.GetVirtualHosts())-1]
		require.Equal(t, []string{"*"}, catchAll.GetDomains())
		last := catchAll.GetRoutes()[len(catchAll.GetRoutes())-1]
		if redirectAll {
			assert.Equal(t, proxy.PassthroughClusterName, last.GetRoute().GetCluster())
		} else {
			assert.Equal(t, uint32(404), last.GetDirectResponse().GetStatus(), "no passthrough -> hard 404")
		}
	}
}

func domainsOf(vhosts []*routev3.VirtualHost) []string {
	var out []string
	for _, vh := range vhosts {
		out = append(out, vh.GetDomains()...)
	}
	return out
}

// routeForPrefix returns the first route in the vhost whose match is the given
// segment-boundary prefix (path_separated_prefix) or plain prefix.
func routeForPrefix(vh *routev3.VirtualHost, prefix string) *routev3.Route {
	for _, r := range vh.GetRoutes() {
		m := r.GetMatch()
		if m.GetPathSeparatedPrefix() == prefix || m.GetPrefix() == prefix {
			return r
		}
	}
	return nil
}

// TestCaptureVhosts_RouteTargetGAMMAFeatures verifies that the capture (cap_http)
// path applies the SAME full GAMMA feature set as the outbound path for a route
// TARGET (proposal 023 shape: an "echo" target fanned out to echo-v1/echo-v2).
// This is the MESH-HTTP conformance gap: the capture vhost must emit
// request/response header modification, a request redirect, AND a weighted split —
// not a reduced action. It mirrors the exact conformance HTTPRoute shapes
// (MeshHTTPRouteRequestHeaderModifier / RedirectHostAndStatus / Weight).
func TestCaptureVhosts_RouteTargetGAMMAFeatures(t *testing.T) {
	const (
		v1Cluster = "echo-v1.gateway-conformance-mesh.aether.internal"
		v2Cluster = "echo-v2.gateway-conformance-mesh.aether.internal"
	)
	c := newTestCache("node-1")
	// A single route target "gateway-conformance-mesh/echo" carrying all three
	// conformance feature shapes as separate rules.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"gateway-conformance-mesh/echo": {
			{
				// RequestHeaderModifier: set + add + remove on request, set + remove on response.
				Matches:  []proxy.GammaMatch{{Prefix: "/set"}},
				Backends: []proxy.GammaBackend{{Service: "gateway-conformance-mesh/echo-v1", Cluster: v1Cluster, Weight: 1}},
				HeaderMutation: &proxy.GammaHeaderMutation{
					SetRequest:     []proxy.GammaHeaderKV{{Name: "X-Header-Set", Value: "set-overwrites-values"}},
					AddRequest:     []proxy.GammaHeaderKV{{Name: "X-Header-Add", Value: "add-appends-values"}},
					RemoveRequest:  []string{"X-Header-Remove"},
					SetResponse:    []proxy.GammaHeaderKV{{Name: "X-Resp-Set", Value: "v"}},
					RemoveResponse: []string{"X-Resp-Remove"},
				},
			},
			{
				// RequestRedirect: host + 301, NO backend cluster.
				Matches:  []proxy.GammaMatch{{Prefix: "/hostname-redirect"}},
				Redirect: &proxy.GammaRedirect{Hostname: "example.org", StatusCode: 301},
			},
			{
				// 70/30 weighted split on the default path.
				Matches: []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{
					{Service: "gateway-conformance-mesh/echo-v1", Cluster: v1Cluster, Weight: 70},
					{Service: "gateway-conformance-mesh/echo-v2", Cluster: v2Cluster, Weight: 30},
				},
			},
		},
	})
	c.SetRouteTargetPorts(map[string][]uint32{"gateway-conformance-mesh/echo": {80}})

	vhosts := captureTable(c, 80)

	// The route target must host-match the bare same-namespace dial "echo" (the
	// authority the conformance client presents) — else the request falls to the
	// redirect-all passthrough (kube-proxy bypass) and no GAMMA feature applies.
	vh := findVHost(vhosts, "echo")
	require.NotNil(t, vh, "route target must carry the bare 'echo' domain; got %v", domainsOf(vhosts))
	assert.Contains(t, vh.GetDomains(), "echo:80", "real Service port spelling (M2) must host-match too")

	// 1. Header modification: the /set route carries request + response header maps.
	setRoute := routeForPrefix(vh, "/set")
	require.NotNil(t, setRoute, "the /set rule must produce a capture route")
	assert.Equal(t, v1Cluster, setRoute.GetRoute().GetCluster(), "/set forwards to echo-v1")
	require.Len(t, setRoute.GetRequestHeadersToAdd(), 2, "set + add request headers emitted on the capture path")
	assert.Equal(t, "X-Header-Set", setRoute.GetRequestHeadersToAdd()[0].GetHeader().GetKey())
	assert.Equal(t, "X-Header-Add", setRoute.GetRequestHeadersToAdd()[1].GetHeader().GetKey())
	assert.Equal(t, []string{"X-Header-Remove", "x-aether-outcome"}, setRoute.GetRequestHeadersToRemove())
	require.Len(t, setRoute.GetResponseHeadersToAdd(), 1, "response set header emitted on the capture path")
	assert.Equal(t, "X-Resp-Set", setRoute.GetResponseHeadersToAdd()[0].GetHeader().GetKey())
	// The rule's own removal, then the outcome header (aether#1641).
	assert.Equal(t, []string{"X-Resp-Remove", "x-aether-outcome"}, setRoute.GetResponseHeadersToRemove())

	// 2. Redirect: the /hostname-redirect route emits a RedirectAction (NOT a forward).
	redirectRoute := routeForPrefix(vh, "/hostname-redirect")
	require.NotNil(t, redirectRoute, "the redirect rule must produce a capture route")
	rd := redirectRoute.GetRedirect()
	require.NotNil(t, rd, "redirect rule emits a Route_Redirect on the capture path, not a cluster forward")
	assert.Equal(t, "example.org", rd.GetHostRedirect())
	assert.Equal(t, routev3.RedirectAction_MOVED_PERMANENTLY, rd.GetResponseCode())
	assert.Nil(t, redirectRoute.GetRoute(), "a redirect route must not carry a RouteAction")

	// 3. Weight: the default "/" route emits a 70/30 weighted_clusters split.
	weightRoute := routeForPrefix(vh, "/")
	require.NotNil(t, weightRoute, "the weighted '/' rule must produce a capture route")
	wc := weightRoute.GetRoute().GetWeightedClusters().GetClusters()
	require.Len(t, wc, 2, "two weighted backends on the capture path")
	got := map[string]uint32{}
	for _, w := range wc {
		got[w.GetName()] = w.GetWeight().GetValue()
	}
	assert.Equal(t, uint32(70), got[v1Cluster], "echo-v1 weight 70 preserved on the capture path")
	assert.Equal(t, uint32(30), got[v2Cluster], "echo-v2 weight 30 preserved on the capture path")
}
