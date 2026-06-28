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
