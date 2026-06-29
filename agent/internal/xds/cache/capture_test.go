package cache

import (
	"regexp"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
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

	vhosts := c.captureVhosts()

	// The cluster.local authority at the real port must host-match.
	vh := findVHost(vhosts, "echo.team-a.svc.cluster.local:8080")
	require.NotNil(t, vh, "route target must carry a :realPort cluster.local domain (M2); got vhosts=%v", domainsOf(vhosts))

	domains := vh.GetDomains()
	// Both the portless + mesh :18081 spellings (M1) AND the real-port spellings (M2).
	assert.Contains(t, domains, "echo.team-a.svc.cluster.local")
	assert.Contains(t, domains, "echo.team-a.svc.cluster.local:18081")
	assert.Contains(t, domains, "echo.team-a.aether.internal")
	assert.Contains(t, domains, "echo.team-a.aether.internal:18081")
	assert.Contains(t, domains, "echo.team-a.svc.cluster.local:8080")
	assert.Contains(t, domains, "echo.team-a.aether.internal:8080")

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

	vhosts := c.captureVhosts()

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
// declares no port falls back to the M1 behavior (portless + :18081 only) — no
// spurious :0 domain.
func TestCaptureVhosts_RouteTargetNoPort(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 1}}}},
	})
	// No SetRouteTargetPorts (or empty) — the M1 path.

	vhosts := c.captureVhosts()
	vh := findVHost(vhosts, "echo.team-a.svc.cluster.local")
	require.NotNil(t, vh)
	for _, d := range vh.GetDomains() {
		assert.NotContains(t, d, ":0", "no spurious :0 domain when the parentRef omits a port")
	}
}

// matchesAuthority reports whether any known-target route's authority regex matches
// the given dialed authority and, if so, the cluster it pins to.
func matchesAuthority(targets []proxy.KnownTargetRoute, authority string) (string, bool) {
	for _, kt := range targets {
		if regexp.MustCompile(kt.AuthorityRegex).MatchString(authority) {
			return kt.Cluster, true
		}
	}
	return "", false
}

// TestCaptureKnownTargets_StableAcrossGAMMAChurn is the regression guard for the
// MESH-HTTP RDS-reload convergence race. The known-target safety net is derived
// from captureAuthorities (fed by the generated mesh Services, INDEPENDENT of
// HTTPRoute lifecycle), so an in-scope mesh authority remains pinned to its cluster
// even after its GAMMA rules are cleared (the delete half of a suite's apply/delete
// churn). A captured request to that authority therefore never falls to the
// ORIGINAL_DST passthrough → kube-proxy while the dedicated vhost is mid-rebuild.
func TestCaptureKnownTargets_StableAcrossGAMMAChurn(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)

	const (
		svc     = "gateway-conformance-mesh/echo-v2"
		cluster = "echo-v2.gateway-conformance-mesh.aether.internal"
	)
	// The mesh-Service reconciler projects the SA-backed authority. This is stable
	// across HTTPRoute churn.
	c.SetCaptureAuthorities(map[string]string{
		svc: "echo-v2.gateway-conformance-mesh.svc.cluster.local",
	})
	// A GAMMA rule is applied (the route exists).
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		svc: {{Backends: []proxy.GammaBackend{{Service: svc, Cluster: cluster, Weight: 1}}}},
	})

	dialSpellings := []string{
		"echo-v2",
		"echo-v2:80",
		"echo-v2.gateway-conformance-mesh",
		"echo-v2.gateway-conformance-mesh.svc",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local:8080",
	}

	// With the route applied, every spelling pins to the cluster.
	withRules := c.captureKnownTargets()
	for _, a := range dialSpellings {
		got, ok := matchesAuthority(withRules, a)
		require.Truef(t, ok, "with rules: %q must be a known target", a)
		assert.Equalf(t, cluster, got, "with rules: %q -> %s", a, cluster)
	}

	// Now SIMULATE THE CHURN: the suite deletes the HTTPRoute, the gamma reconciler
	// reconciles with the route gone -> serviceRoutes for echo-v2 is cleared. The
	// dedicated cap_http vhost vanishes (this is the race window). The mesh Service
	// is untouched, so captureAuthorities still carries echo-v2.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{})

	// The dedicated vhost is indeed gone (proving the window is real)...
	assert.Nil(t, findVHost(c.captureVhosts(), "echo-v2"),
		"the dedicated cap_http vhost is absent while rules are cleared (the race window)")

	// ...but the known-target safety net STILL pins every spelling to the cluster,
	// so a captured request reaches the backend in-mesh, never the passthrough.
	afterChurn := c.captureKnownTargets()
	for _, a := range dialSpellings {
		got, ok := matchesAuthority(afterChurn, a)
		require.Truef(t, ok, "after churn: %q must remain a known target (no passthrough leak)", a)
		assert.Equalf(t, cluster, got, "after churn: %q -> %s", a, cluster)
	}
}

// TestCaptureKnownTargets_RouteOnlyTarget covers the versioned-fanout shape: a GAMMA
// route TARGET with no SA-backed mesh Service of its own (only in serviceRoutes).
// Its cluster.local authority is derived from the namespace-qualified key and it is
// pinned to its mesh cluster so the passthrough never shadows it.
func TestCaptureKnownTargets_RouteOnlyTarget(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)

	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 1}}}},
	})

	targets := c.captureKnownTargets()
	cluster, ok := matchesAuthority(targets, "echo.team-a.svc.cluster.local:8080")
	require.True(t, ok, "route-only target must be pinned")
	assert.Equal(t, "echo.team-a.aether.internal", cluster)
	cluster, ok = matchesAuthority(targets, "echo")
	require.True(t, ok, "bare same-namespace dial must be pinned")
	assert.Equal(t, "echo.team-a.aether.internal", cluster)
}

// TestCaptureKnownTargets_DisabledWithoutRedirectAll: with scoped capture (no
// redirect-all) there is no passthrough cluster to shadow a target, so the
// known-target set is empty and the catch-all keeps its hard 404.
func TestCaptureKnownTargets_DisabledWithoutRedirectAll(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	// redirect-all NOT enabled.
	c.SetCaptureAuthorities(map[string]string{
		"team-a/echo": "echo.team-a.svc.cluster.local",
	})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo", Cluster: "echo.team-a.aether.internal", Weight: 1}}}},
	})
	assert.Nil(t, c.captureKnownTargets(), "no passthrough -> no known-target safety net")
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

	vhosts := c.captureVhosts()

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
	assert.Equal(t, []string{"X-Header-Remove"}, setRoute.GetRequestHeadersToRemove())
	require.Len(t, setRoute.GetResponseHeadersToAdd(), 1, "response set header emitted on the capture path")
	assert.Equal(t, "X-Resp-Set", setRoute.GetResponseHeadersToAdd()[0].GetHeader().GetKey())
	assert.Equal(t, []string{"X-Resp-Remove"}, setRoute.GetResponseHeadersToRemove())

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
