package proxy

import (
	"regexp"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolveCatchAll mimics Envoy's first-match route selection over the catch-all
// vhost for a given :authority and path: it walks the routes in order and returns
// the cluster of the first whose path prefix and (if present) :authority safe_regex
// header matcher both match. A direct_response (404/200) route returns "" for the
// cluster and its status. This lets a unit test assert "a known target never
// resolves to the passthrough cluster" without standing up Envoy.
func resolveCatchAll(vh *routev3.VirtualHost, authority, path string) (cluster string, directStatus uint32) {
	for _, rt := range vh.GetRoutes() {
		m := rt.GetMatch()
		switch {
		case m.GetPath() != "":
			if m.GetPath() != path {
				continue
			}
		case m.GetPrefix() != "":
			if len(path) < len(m.GetPrefix()) || path[:len(m.GetPrefix())] != m.GetPrefix() {
				continue
			}
		}
		ok := true
		for _, h := range m.GetHeaders() {
			if h.GetName() != ":authority" {
				continue
			}
			re := h.GetStringMatch().GetSafeRegex().GetRegex()
			if re == "" || !regexp.MustCompile(re).MatchString(authority) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if dr := rt.GetDirectResponse(); dr != nil {
			return "", dr.GetStatus()
		}
		if ch := rt.GetRoute().GetClusterHeader(); ch != "" {
			// cluster_header(:authority) resolves to the authority itself.
			return authority, 0
		}
		return rt.GetRoute().GetCluster(), 0
	}
	return "", 0
}

// TestCatchAll_KnownTargetNeverShadowedByPassthrough is the regression guard for the
// MESH-HTTP RDS-reload convergence race: a captured request whose :authority is a
// known in-scope mesh service must resolve to that service's cluster — under EVERY
// dial spelling and on ANY port — never to the ORIGINAL_DST passthrough (which
// leaks to kube-proxy and silently drops the GAMMA features). The known-target
// routes are derived from the STABLE mesh-authority set, so this holds even while
// the service's dedicated cap_http vhost is absent mid-rebuild.
func TestCatchAll_KnownTargetNeverShadowedByPassthrough(t *testing.T) {
	// echo-v2 in gateway-conformance-mesh, all spellings, optional port.
	known := KnownTargetRoute{
		AuthorityRegex: "^(echo-v2|echo-v2\\.gateway-conformance-mesh|echo-v2\\.gateway-conformance-mesh\\.svc|echo-v2\\.gateway-conformance-mesh\\.svc\\.cluster\\.local)(:[0-9]+)?$",
		Cluster:        "echo-v2.gateway-conformance-mesh.aether.internal",
	}
	vh := buildOnDemandCatchAllVirtualHost("aether.internal", true, known)

	// Every spelling a client might dial, with and without a real Service port.
	for _, authority := range []string{
		"echo-v2",
		"echo-v2:80",
		"echo-v2.gateway-conformance-mesh",
		"echo-v2.gateway-conformance-mesh:80",
		"echo-v2.gateway-conformance-mesh.svc",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local:8080",
	} {
		cluster, _ := resolveCatchAll(vh, authority, "/set")
		assert.Equalf(t, known.Cluster, cluster,
			"known target %q must route to its cluster, not the passthrough", authority)
		assert.NotEqualf(t, PassthroughClusterName, cluster,
			"known target %q must never resolve to the passthrough", authority)
	}

	// A genuinely foreign authority still falls to the passthrough (redirect-all).
	cluster, _ := resolveCatchAll(vh, "example.com", "/")
	assert.Equal(t, PassthroughClusterName, cluster,
		"a non-mesh, unknown authority still passes through (plain egress)")

	// A mesh-shaped authority still resolves via ODCDS cluster_header(:authority).
	cluster, _ = resolveCatchAll(vh, "other.team-a.aether.internal", "/")
	assert.Equal(t, "other.team-a.aether.internal", cluster,
		"a mesh-shaped authority still resolves via ODCDS, not the passthrough")
}

// TestCatchAll_KnownTargetsOnlyInRedirectAll: without redirect-all there is no
// passthrough to shadow, so known-target routes must NOT be emitted (the catch-all
// keeps its hard-404 fallthrough and the minimal mesh-authority/liveness routes).
func TestCatchAll_KnownTargetsOnlyInRedirectAll(t *testing.T) {
	known := KnownTargetRoute{AuthorityRegex: "^echo$", Cluster: "echo.ns.aether.internal"}

	withPT := buildOnDemandCatchAllVirtualHost("aether.internal", true, known)
	require.Len(t, withPT.GetRoutes(), 4, "liveness + mesh-regex + 1 known-target + passthrough")
	assert.Equal(t, PassthroughClusterName,
		withPT.GetRoutes()[len(withPT.GetRoutes())-1].GetRoute().GetCluster())

	// redirectAll=false: knownTargets are ignored and the fallthrough is a 404.
	noPT := buildOnDemandCatchAllVirtualHost("aether.internal", false, known)
	require.Len(t, noPT.GetRoutes(), 3, "liveness + mesh-regex + 404 (no known-target, no passthrough)")
	assert.Equal(t, uint32(404),
		noPT.GetRoutes()[len(noPT.GetRoutes())-1].GetDirectResponse().GetStatus())
}

func TestBuildOutboundRouteConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		vhosts    []*routev3.VirtualHost
		expectLen int
	}{
		{
			name:      "no virtual hosts",
			vhosts:    nil,
			expectLen: 0,
		},
		{
			name: "single virtual host",
			vhosts: []*routev3.VirtualHost{
				{Name: "svc-a", Domains: []string{"svc-a"}},
			},
			expectLen: 1,
		},
		{
			name: "multiple virtual hosts",
			vhosts: []*routev3.VirtualHost{
				{Name: "svc-a", Domains: []string{"svc-a"}},
				{Name: "svc-b", Domains: []string{"svc-b"}},
			},
			expectLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routeConfig := BuildOutboundRouteConfiguration(tt.vhosts, "aether.internal")

			require.NotNil(t, routeConfig)
			assert.Equal(t, OutboundHTTPRouteName, routeConfig.GetName())
			// Service vhosts plus the universal on-demand catch-all, always last.
			require.Len(t, routeConfig.GetVirtualHosts(), tt.expectLen+1)
			catchAll := routeConfig.GetVirtualHosts()[tt.expectLen]
			assert.Equal(t, []string{"*"}, catchAll.GetDomains())
			// Route 1: liveness local-reply 200 on MeshLivePath (proposal 013).
			// Route 2: mesh-shaped authority (regex) -> ODCDS cluster_header.
			// Route 3: everything else -> instant 404.
			require.Len(t, catchAll.GetRoutes(), 3)
			assert.Equal(t, MeshLivePath, catchAll.GetRoutes()[0].GetMatch().GetPath())
			assert.Equal(t, uint32(200), catchAll.GetRoutes()[0].GetDirectResponse().GetStatus())
			assert.Equal(t, onDemandClusterHeader, catchAll.GetRoutes()[1].GetRoute().GetClusterHeader())
			assert.NotEmpty(t, catchAll.GetRoutes()[1].GetMatch().GetHeaders(), "mesh-authority regex gate")
			assert.Equal(t, uint32(404), catchAll.GetRoutes()[2].GetDirectResponse().GetStatus())
		})
	}
}

func TestBuildOutboundClusterVirtualHost(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
	}{
		{name: "standard cluster", clusterName: "my-service"},
		{name: "port cluster", clusterName: "my-service:9090"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fqdn := tt.clusterName + ".aether.internal"
			vhost := BuildOutboundClusterVirtualHost(fqdn, []string{fqdn})

			require.NotNil(t, vhost)
			assert.Equal(t, fqdn, vhost.GetName())
			assert.Equal(t, []string{fqdn}, vhost.GetDomains())

			require.Len(t, vhost.GetRoutes(), 1)
			route := vhost.GetRoutes()[0]
			assert.Equal(t, "/", route.GetMatch().GetPrefix())
			assert.Equal(t, fqdn, route.GetRoute().GetCluster())
		})
	}
}

// TestOutboundRetryPolicy: every client-side service route retries endpoint-churn
// failures (drain/warm-up windows) on a different host, with only
// non-idempotent-safe conditions.
func TestOutboundRetryPolicy(t *testing.T) {
	for name, vh := range map[string]*routev3.VirtualHost{
		"cluster vhost":   BuildOutboundClusterVirtualHost("svc-1.aether.internal", []string{"svc-1.aether.internal"}),
		"catch-all vhost": buildOnDemandCatchAllVirtualHost("aether.internal", false),
	} {
		// Find the routed (non-direct-response) route; the catch-all leads with a
		// liveness direct_response route (see TestEgressLivenessRoute).
		var rp *routev3.RetryPolicy
		for _, r := range vh.GetRoutes() {
			if r.GetRoute() != nil {
				rp = r.GetRoute().GetRetryPolicy()
				break
			}
		}
		require.NotNil(t, rp, name)
		assert.Equal(t, "connect-failure,refused-stream,reset-before-request,retriable-status-codes", rp.GetRetryOn(), name)
		assert.Equal(t, []uint32{503}, rp.GetRetriableStatusCodes(), name)
		assert.Equal(t, uint32(2), rp.GetNumRetries().GetValue(), name)
		require.Len(t, rp.GetRetryHostPredicate(), 1, name)
		assert.Equal(t, "envoy.retry_host_predicates.previous_hosts", rp.GetRetryHostPredicate()[0].GetName(), name)
	}
}

// TestEgressLivenessRoute: the outbound catch-all leads with a local-reply 200 on
// MeshLivePath (proposal 013 prober), matched by exact path before the
// authority-regex/404 routes, so it wins regardless of authority.
func TestEgressLivenessRoute(t *testing.T) {
	vh := buildOnDemandCatchAllVirtualHost("aether.internal", false)
	require.NotEmpty(t, vh.GetRoutes())
	live := vh.GetRoutes()[0]
	assert.Equal(t, MeshLivePath, live.GetMatch().GetPath(), "liveness must be an exact-path match")
	require.NotNil(t, live.GetDirectResponse(), "liveness must be a direct_response")
	assert.Equal(t, uint32(200), live.GetDirectResponse().GetStatus())
}
