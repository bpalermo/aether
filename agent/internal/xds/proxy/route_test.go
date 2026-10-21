package proxy

import (
	"regexp"
	"strings"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	previousprioritiesv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/priority/previous_priorities/v3"
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

// resolveTable resolves (authority, path) against a whole route table the way
// Envoy selects a virtual host: an exact domain first, then the longest prefix
// wildcard ("name:*"), then "*". It returns the selected virtual host's name
// with what resolveCatchAll finds in it.
func resolveTable(rc *routev3.RouteConfiguration, authority, path string) (vhost, cluster string, status uint32) {
	var exact, wildcard, star *routev3.VirtualHost
	wildcardLen := -1
	for _, vh := range rc.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			switch {
			case d == "*":
				star = vh
			case strings.HasSuffix(d, "*"):
				if p := strings.TrimSuffix(d, "*"); strings.HasPrefix(authority, p) && len(p) > wildcardLen {
					wildcard, wildcardLen = vh, len(p)
				}
			case d == authority:
				exact = vh
			}
		}
	}
	for _, vh := range []*routev3.VirtualHost{exact, wildcard, star} {
		if vh != nil {
			cluster, status = resolveCatchAll(vh, authority, path)
			return vh.GetName(), cluster, status
		}
	}
	return "", "", 0
}

// TestCatchAll_KnownTargetNeverShadowedByPassthrough is the regression guard for the
// MESH-HTTP RDS-reload convergence race: a captured request whose :authority is a
// known in-scope mesh service must resolve to that service's cluster — under EVERY
// dial spelling — never to the ORIGINAL_DST passthrough (which leaks to
// kube-proxy and silently drops the GAMMA features).
//
// Until proposal 043 the guarantee was a "known target" regex route on the
// catch-all that accepted the service's names ON ANY PORT and sent them to its
// default cluster. That route was #1682: "<svc>.<ns>:9090" reached the port the
// default cluster addresses and answered 200. It is gone. The guarantee is now
// held by the table itself: the table of the dialled port carries a virtual
// host with every spelling of the service (port-less and with that port), and
// a refusal for the service's names with any OTHER port. So the catch-all only
// ever sees names that are no mesh service, and what it must NOT do is asserted
// here too: route a Kubernetes name, with any port, to a mesh cluster.
func TestCatchAll_KnownTargetNeverShadowedByPassthrough(t *testing.T) {
	const cluster = "echo-v2.gateway-conformance-mesh.aether.internal"
	names := []string{
		"echo-v2",
		"echo-v2.gateway-conformance-mesh",
		"echo-v2.gateway-conformance-mesh.svc",
		"echo-v2.gateway-conformance-mesh.svc.cluster.local",
		cluster,
	}
	var routed, mismatch []string
	for _, n := range names {
		routed = append(routed, n, n+":80")
		mismatch = append(mismatch, n+":*")
	}
	// The table of dialled port 80, redirect-all.
	table := BuildCaptureRouteConfiguration(CaptureHTTPRouteName(80), []*routev3.VirtualHost{
		BuildServiceVirtualHost(cluster+"|80", cluster, routed, nil),
		BuildRefusalVirtualHost(cluster+"|mismatch", mismatch, RefusalPortMismatch, "refused\n"),
	}, BuildCaptureCatchAllVirtualHost("aether.internal", 80, true, true), nil)

	// Every spelling a client might dial on port 80, port-less or spelled out.
	for _, authority := range routed {
		vh, got, _ := resolveTable(table, authority, "/set")
		assert.Equalf(t, cluster, got, "known target %q must route to its cluster, not the passthrough (vhost %s)", authority, vh)
		assert.NotEqualf(t, PassthroughClusterName, got, "known target %q must never resolve to the passthrough", authority)
	}

	// The same names with a port that was NOT dialled: refused, and in
	// particular not delivered to the cluster of the port that was (#1682).
	for _, n := range names {
		vh, got, status := resolveTable(table, n+":8080", "/set")
		assert.Equalf(t, uint32(421), status, "%s:8080 in the table of port 80 (vhost %s)", n, vh)
		assert.Emptyf(t, got, "%s:8080 must reach no cluster", n)
	}

	// A genuinely foreign authority still falls to the passthrough (redirect-all).
	_, got, _ := resolveTable(table, "example.com", "/")
	assert.Equal(t, PassthroughClusterName, got,
		"a non-mesh, unknown authority still passes through (plain egress)")

	// A mesh-shaped authority this node has no virtual host for still resolves
	// via ODCDS cluster_header(:authority), in a legacy table port-less too.
	_, got, _ = resolveTable(table, "other.team-a.aether.internal", "/")
	assert.Equal(t, "other.team-a.aether.internal", got,
		"a mesh-shaped authority still resolves via ODCDS, not the passthrough")

	// The catch-all on its own, with the service's virtual hosts gone (the
	// mid-rebuild window the known-target routes were for): a Kubernetes name
	// is passed through, with or without a port. It is NEVER sent to a mesh
	// cluster by the catch-all, on any port; that was the defect.
	catchAll := BuildCaptureCatchAllVirtualHost("aether.internal", 80, true, true)
	for _, authority := range []string{"echo-v2", "echo-v2:80", "echo-v2.gateway-conformance-mesh:9090", "echo-v2.gateway-conformance-mesh.svc.cluster.local:8080"} {
		got, _ := resolveCatchAll(catchAll, authority, "/set")
		assert.Equalf(t, PassthroughClusterName, got, "the catch-all names no mesh cluster for %q", authority)
	}
}

// TestCatchAll_KnownTargetsOnlyInRedirectAll: the catch-all's shape in the two
// capture modes. It carries no per-service route in either (the known-target
// routes it had in redirect-all are gone with proposal 043): liveness, the
// on-demand route, and the fallthrough, which is the passthrough with
// redirect-all and a hard 404 without.
func TestCatchAll_KnownTargetsOnlyInRedirectAll(t *testing.T) {
	withPT := BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, true)
	require.Len(t, withPT.GetRoutes(), 3, "liveness + mesh-regex + passthrough")
	assert.Equal(t, PassthroughClusterName,
		withPT.GetRoutes()[len(withPT.GetRoutes())-1].GetRoute().GetCluster())

	// redirectAll=false: the fallthrough is a 404.
	noPT := BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, false)
	require.Len(t, noPT.GetRoutes(), 3, "liveness + mesh-regex + 404 (no passthrough)")
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
			routeConfig := BuildOutboundRouteConfiguration(tt.vhosts, "aether.internal", nil)

			require.NotNil(t, routeConfig)
			assert.Equal(t, OutboundHTTPRouteName, routeConfig.GetName())
			// Service vhosts plus the universal on-demand catch-all, always last.
			require.Len(t, routeConfig.GetVirtualHosts(), tt.expectLen+1)
			catchAll := routeConfig.GetVirtualHosts()[tt.expectLen]
			assert.Equal(t, []string{"*"}, catchAll.GetDomains())
			// Route 1: liveness local-reply 200 on MeshLivePath (proposal 013).
			// Route 2: mesh-shaped authority (regex) -> ODCDS cluster_header.
			// Route 3: everything else -> instant 404.
			require.Len(t, catchAll.GetRoutes(), 4)
			assert.Equal(t, MeshLivePath, catchAll.GetRoutes()[0].GetMatch().GetPath())
			assert.Equal(t, uint32(200), catchAll.GetRoutes()[0].GetDirectResponse().GetStatus())
			assert.Equal(t, uint32(421), catchAll.GetRoutes()[1].GetDirectResponse().GetStatus(), "the mesh's own two ports, before the on-demand route")
			assert.NotEmpty(t, catchAll.GetRoutes()[1].GetMatch().GetHeaders())
			assert.Equal(t, onDemandClusterHeader, catchAll.GetRoutes()[2].GetRoute().GetClusterHeader())
			assert.NotEmpty(t, catchAll.GetRoutes()[2].GetMatch().GetHeaders(), "mesh-authority regex gate")
			assert.Equal(t, uint32(404), catchAll.GetRoutes()[3].GetDirectResponse().GetStatus())
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
		"catch-all vhost": buildOnDemandCatchAllVirtualHost(MeshAuthorityRegex("aether.internal", "(:[0-9]+)?"), false),
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
		// The response-driven condition is the destination's outcome header and
		// no status code (aether#1641; outcome_test.go has the rule itself).
		assert.Equal(t, "connect-failure,refused-stream,reset-before-request,retriable-headers", rp.GetRetryOn(), name)
		assert.Empty(t, rp.GetRetriableStatusCodes(), name)
		require.Len(t, rp.GetRetriableHeaders(), 1, name)
		assert.Equal(t, "x-aether-outcome", rp.GetRetriableHeaders()[0].GetName(), name)
		assert.Equal(t, uint32(2), rp.GetNumRetries().GetValue(), name)
		require.Len(t, rp.GetRetryHostPredicate(), 1, name)
		assert.Equal(t, "envoy.retry_host_predicates.previous_hosts", rp.GetRetryHostPredicate()[0].GetName(), name)
		requireRetryPriority(t, name, rp)
	}
}

// requireRetryPriority fails unless a retry under rp leaves every priority
// already attempted: previous_priorities with update_frequency 1. Without it
// a source whose priority 0 is one dying endpoint sends every retry back to
// that endpoint, previous_hosts having no other host there to pick.
func requireRetryPriority(t *testing.T, where string, rp *routev3.RetryPolicy) {
	t.Helper()
	pr := rp.GetRetryPriority()
	require.NotNilf(t, pr, "%s: no retry_priority: a retry stays at the priority that failed", where)
	assert.Equalf(t, "envoy.retry_priorities.previous_priorities", pr.GetName(), where)
	var cfg previousprioritiesv3.PreviousPrioritiesConfig
	require.NotNilf(t, pr.GetTypedConfig(), "%s: previous_priorities needs its typed config (update_frequency 0 is rejected)", where)
	require.NoErrorf(t, pr.GetTypedConfig().UnmarshalTo(&cfg), where)
	assert.Equalf(t, int32(1), cfg.GetUpdateFrequency(),
		"%s: the FIRST retry must already leave the priority that failed", where)
}

// TestEveryRetryPolicyLeavesAttemptedPriorities: both policies the generators
// hand out, the mesh one and the cleartext edge one, carry the retry
// priority, and each call returns its own copy.
func TestEveryRetryPolicyLeavesAttemptedPriorities(t *testing.T) {
	requireRetryPriority(t, "outboundRetryPolicy", outboundRetryPolicy())
	requireRetryPriority(t, "nonMeshRetryPolicy", nonMeshRetryPolicy())

	a, b := outboundRetryPolicy(), outboundRetryPolicy()
	a.GetRetryPriority().Name = "changed"
	assert.Equal(t, retryPriorityName, b.GetRetryPriority().GetName(), "two routes must not share one retry priority")
}

// TestEgressLivenessRoute: the outbound catch-all leads with a local-reply 200 on
// MeshLivePath (proposal 013 prober), matched by exact path before the
// authority-regex/404 routes, so it wins regardless of authority.
func TestEgressLivenessRoute(t *testing.T) {
	vh := buildOnDemandCatchAllVirtualHost(MeshAuthorityRegex("aether.internal", "(:[0-9]+)?"), false)
	require.NotEmpty(t, vh.GetRoutes())
	live := vh.GetRoutes()[0]
	assert.Equal(t, MeshLivePath, live.GetMatch().GetPath(), "liveness must be an exact-path match")
	require.NotNil(t, live.GetDirectResponse(), "liveness must be a direct_response")
	assert.Equal(t, uint32(200), live.GetDirectResponse().GetStatus())
}
