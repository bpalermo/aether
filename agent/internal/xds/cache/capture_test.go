package cache

import (
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

func domainsOf(vhosts []*routev3.VirtualHost) []string {
	var out []string
	for _, vh := range vhosts {
		out = append(out, vh.GetDomains()...)
	}
	return out
}
