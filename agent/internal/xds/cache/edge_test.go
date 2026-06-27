package cache

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stripReadiness asserts the dedicated always-bound readiness listener is present
// (on the default readiness port) and returns the remaining PUBLIC edge listeners,
// so the public-listener assertions below are unaffected by it.
func stripReadiness(t *testing.T, ls []types.Resource) []*listenerv3.Listener {
	t.Helper()
	var public []*listenerv3.Listener
	foundReadiness := false
	for _, r := range ls {
		l := r.(*listenerv3.Listener)
		if l.GetName() == proxy.EdgeReadinessListenerName {
			foundReadiness = true
			assert.Equal(t, uint32(proxy.DefaultEdgeReadinessPort), l.GetAddress().GetSocketAddress().GetPortValue())
			continue
		}
		public = append(public, l)
	}
	require.True(t, foundReadiness, "the dedicated readiness listener must always be present")
	return public
}

// TestEdgeTLSModeListeners verifies that with TLS enabled AND the per-Gateway
// HTTP redirect opt-in set, the cache serves a TLS listener on the https port
// (referencing the vhost's SDS cert) plus an HTTP->HTTPS redirect on the plain
// port; the certs ride the SecretType channel.
func TestEdgeTLSModeListeners(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	c.SetEdgeTLSMode(443)
	c.SetEdgeHTTPRedirect(true) // opt-in: this Gateway redirects HTTP→HTTPS
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-1"}}, TLSSecret: "kubernetes/api-tls"},
	})
	require.NoError(t, c.SetEdgeTLSSecrets(context.Background(), map[string]EdgeTLSCert{
		"kubernetes/api-tls": {Cert: []byte("CERT"), Key: []byte("KEY")},
	}))

	ls := stripReadiness(t, c.Listeners())
	require.Len(t, ls, 2)
	names := map[string]*listenerv3.Listener{}
	for _, l := range ls {
		names[l.GetName()] = l
	}
	tls := names[proxy.EdgeHTTPSListenerName]
	redirect := names[proxy.EdgeRedirectListenerName]
	require.NotNil(t, tls)
	require.NotNil(t, redirect)
	assert.Equal(t, uint32(443), tls.GetAddress().GetSocketAddress().GetPortValue())
	assert.Equal(t, uint32(80), redirect.GetAddress().GetSocketAddress().GetPortValue())
	require.NotNil(t, tls.GetFilterChains()[0].GetTransportSocket(), "https listener terminates TLS")

	// The cert rides the snapshot SecretType channel under its SDS name.
	c.secretMu.RLock()
	sec := c.secrets["kubernetes/api-tls"]
	c.secretMu.RUnlock()
	require.NotNil(t, sec)
	assert.Equal(t, []byte("CERT"), sec.GetTlsCertificate().GetCertificateChain().GetInlineBytes())
}

func TestSetStaticDependencies(t *testing.T) {
	c := newTestCache("edge-1")

	c.SetStaticDependencies([]string{"svc-1", "svc-2", ""})
	deps := c.DependencySet()
	assert.Len(t, deps, 2)
	assert.Contains(t, deps, "svc-1")
	assert.Contains(t, deps, "svc-2")

	// Replacing the exposed set drops the old members.
	c.SetStaticDependencies([]string{"svc-3"})
	deps = c.DependencySet()
	assert.Len(t, deps, 1)
	assert.Contains(t, deps, "svc-3")
}

// TestEdgeModeListeners verifies edge mode serves the single public-facing
// listener (not the node proxy's per-pod inbound/outbound + health gateway).
func TestEdgeModeListeners(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(8080)

	listeners := stripReadiness(t, c.Listeners())
	require.Len(t, listeners, 1)
	l := listeners[0]
	assert.Equal(t, proxy.EdgeListenerName, l.GetName())
	assert.Equal(t, uint32(8080), l.GetAddress().GetSocketAddress().GetPortValue())
}

// TestSetVirtualHostsDependencySet verifies the dependency set is the union of
// every routable virtual host's backend services. A hostless vhost is inert.
func TestSetVirtualHostsDependencySet(t *testing.T) {
	c := newTestCache("edge-1")

	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com"}, Routes: []Route{
			{Prefix: "/a", Service: "svc-1"},
			{Prefix: "/b", Service: "svc-2", Port: 9090},
		}},
		{Routes: []Route{{Prefix: "/", Service: "svc-3"}}}, // no hosts -> catch-all "*", IS routable + scoped
	})

	deps := c.DependencySet()
	assert.Len(t, deps, 3)
	assert.Contains(t, deps, "svc-1")
	assert.Contains(t, deps, "svc-2")
	assert.Contains(t, deps, "svc-3", "a hostname-less route matches all hosts (catch-all) and must scope its service")
}

// TestVirtualHostVhosts checks host->cluster resolution: external hosts become
// the vhost domains, a non-default port targets the per-port cluster, and a route
// WITHOUT hosts becomes the catch-all "*" vhost (Gateway API: a hostname-less route
// matches all hosts on its listener).
//
// With the hostname-merge design, each domain in a VirtualHost.Hosts list gets its
// own Envoy vhost entry (one vhost per distinct hostname). A VirtualHost with two
// hostnames emits two Envoy vhosts that share the same routes — this is equivalent
// to the old single-vhost-with-multiple-domains output and passes Envoy validation
// (no duplicate domains across vhosts).
func TestVirtualHostVhosts(t *testing.T) {
	c := newTestCache("edge-1")

	// Register services as mesh-registered so edgeClusterNameLocked uses the mesh path.
	// A per-port cluster must also exist for the explicit-port backend to resolve to it.
	c.clusterMu.Lock()
	c.clusters["svc-1"] = clusterEntry{service: "svc-1"}
	c.clusters["svc-3"] = clusterEntry{service: "svc-3"}
	c.clusters["svc-2"] = clusterEntry{service: "svc-2"}
	c.clusters["svc-2.aether.internal:9090"] = clusterEntry{service: "svc-2", sni: "9090"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com", "api2.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-1"}}},
		{Routes: []Route{{Prefix: "/", Service: "svc-3"}}}, // no hosts -> catch-all "*"
		{Hosts: []string{"grpc.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-2", Port: 9090}}},
	})

	vhosts := c.virtualHostVhosts()
	// 4 = api.example.com + api2.example.com + grpc.example.com + catch-all "*".
	// (One Envoy vhost per distinct hostname; "api.example.com" and "api2.example.com"
	// are siblings from the same VirtualHost and each gets its own Envoy vhost.)
	require.Len(t, vhosts, 4)

	byName := map[string]*routev3.VirtualHost{}
	for _, vh := range vhosts {
		byName[vh.GetName()] = vh
	}

	require.NotNil(t, byName["api.example.com"])
	assert.Equal(t, []string{"api.example.com"}, byName["api.example.com"].GetDomains())
	assert.Equal(t, "svc-1.aether.internal", byName["api.example.com"].GetRoutes()[0].GetRoute().GetCluster())

	require.NotNil(t, byName["api2.example.com"], "api2.example.com gets its own Envoy vhost (one per domain)")
	assert.Equal(t, []string{"api2.example.com"}, byName["api2.example.com"].GetDomains())
	assert.Equal(t, "svc-1.aether.internal", byName["api2.example.com"].GetRoutes()[0].GetRoute().GetCluster())

	require.NotNil(t, byName["grpc.example.com"])
	assert.Equal(t, []string{"grpc.example.com"}, byName["grpc.example.com"].GetDomains())
	assert.Equal(t, "svc-2.aether.internal:9090", byName["grpc.example.com"].GetRoutes()[0].GetRoute().GetCluster())

	// The hostname-less route is served by the catch-all "*" vhost.
	require.NotNil(t, byName["*"], "a hostname-less route must produce the catch-all * vhost")
	assert.Equal(t, []string{"*"}, byName["*"].GetDomains())
	assert.Equal(t, "svc-3.aether.internal", byName["*"].GetRoutes()[0].GetRoute().GetCluster())
}

// TestVirtualHostVhostsHostlessMerge verifies that MULTIPLE hostname-less routes
// merge into a SINGLE catch-all "*" vhost (Envoy NACKs duplicate domains, so there
// can be only one "*"), preserving their routes in order.
func TestVirtualHostVhostsHostlessMerge(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetVirtualHosts([]VirtualHost{
		{Routes: []Route{{Prefix: "/a", Service: "svc-a"}}},                              // no hosts
		{Hosts: []string{"x.example.com"}, Routes: []Route{{Prefix: "/", Service: "x"}}}, // hosted
		{Routes: []Route{{Prefix: "/b", Service: "svc-b"}}},                              // no hosts
	})

	vhosts := c.virtualHostVhosts()
	star := 0
	var starVH *routev3.VirtualHost
	for _, vh := range vhosts {
		if vh.GetName() == "*" {
			star++
			starVH = vh
		}
	}
	require.Equal(t, 1, star, "all hostname-less routes share ONE catch-all * vhost")
	require.NotNil(t, starVH)
	assert.Equal(t, []string{"*"}, starVH.GetDomains())
	require.Len(t, starVH.GetRoutes(), 2, "both hostname-less routes land in the * vhost")
	// FIX 1: non-"/" prefixes use path_separated_prefix.
	assert.Equal(t, "/a", starVH.GetRoutes()[0].GetMatch().GetPathSeparatedPrefix())
	assert.Equal(t, "/b", starVH.GetRoutes()[1].GetMatch().GetPathSeparatedPrefix())
}

// TestVirtualHostPathRoutes verifies one virtual host fans different paths to
// different services, ordered by Gateway API path specificity: Exact >
// longer PathPrefix > shorter PathPrefix (most-specific first so Envoy's
// first-match-wins logic applies correctly).
func TestVirtualHostPathRoutes(t *testing.T) {
	c := newTestCache("edge-1")

	// Register services as mesh so edgeClusterNameLocked resolves to mesh names.
	c.clusterMu.Lock()
	c.clusters["svc-1"] = clusterEntry{service: "svc-1"}
	c.clusters["svc-2"] = clusterEntry{service: "svc-2"}
	c.clusters["svc-3"] = clusterEntry{service: "svc-3"}
	c.clusterMu.Unlock()

	// Input order: /users (prefix, len 6), /healthz (exact), / (prefix, len 1).
	// Expected output order: /healthz (exact) → /users (prefix len 6) → / (prefix len 1).
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com"}, Routes: []Route{
			{Prefix: "/users", Service: "svc-1"},
			{Exact: "/healthz", Service: "svc-2"},
			{Prefix: "/", Service: "svc-3"},
		}},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 1)
	routes := vhosts[0].GetRoutes()
	require.Len(t, routes, 3)

	// Exact match first (highest Gateway API precedence).
	assert.Equal(t, "/healthz", routes[0].GetMatch().GetPath())
	assert.Equal(t, "svc-2.aether.internal", routes[0].GetRoute().GetCluster())
	// Longer prefix second: FIX 1 — non-"/" prefix uses path_separated_prefix.
	assert.Equal(t, "/users", routes[1].GetMatch().GetPathSeparatedPrefix())
	assert.Equal(t, "svc-1.aether.internal", routes[1].GetRoute().GetCluster())
	// Catch-all prefix last: "/" stays as plain prefix.
	assert.Equal(t, "/", routes[2].GetMatch().GetPrefix())
	assert.Equal(t, "svc-3.aether.internal", routes[2].GetRoute().GetCluster())
}

// TestVirtualHostVhostsMergeSharedDomains verifies that a host appearing in
// multiple VirtualHosts is MERGED into one Envoy vhost (routes from all
// contributors combined) rather than kept-first-drop. Envoy NACKs duplicate
// domains, so the merge guarantees exactly one domain entry in the output.
// This replaces the old keep-first behavior now that Gateway API allows multiple
// HTTPRoutes to share a hostname on one Gateway.
func TestVirtualHostVhostsMergeSharedDomains(t *testing.T) {
	c := newTestCache("edge-1")

	// Register services as mesh so edgeClusterNameLocked resolves to mesh names.
	c.clusterMu.Lock()
	c.clusters["svc-1"] = clusterEntry{service: "svc-1"}
	c.clusters["svc-2"] = clusterEntry{service: "svc-2"}
	c.clusterMu.Unlock()

	// First vhost: only a.example.com → svc-1.
	// Second vhost: a.example.com AND b.example.com → svc-2.
	// Expected: a.example.com gets routes from BOTH vhosts merged; b.example.com
	// gets only svc-2.
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"a.example.com"}, Routes: []Route{{Prefix: "/a1", Service: "svc-1"}}},
		{Hosts: []string{"a.example.com", "b.example.com"}, Routes: []Route{{Prefix: "/a2", Service: "svc-2"}}},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 2, "one vhost per distinct hostname")

	byDomain := map[string]*routev3.VirtualHost{}
	for _, vh := range vhosts {
		byDomain[vh.GetName()] = vh
	}

	// a.example.com: domains listed exactly once; routes from both contributors.
	aVH := byDomain["a.example.com"]
	require.NotNil(t, aVH)
	assert.Equal(t, []string{"a.example.com"}, aVH.GetDomains(), "domain listed exactly once (no duplicates)")
	require.Len(t, aVH.GetRoutes(), 2, "routes from both VirtualHosts are present")
	clusters := []string{
		aVH.GetRoutes()[0].GetRoute().GetCluster(),
		aVH.GetRoutes()[1].GetRoute().GetCluster(),
	}
	assert.ElementsMatch(t, []string{"svc-1.aether.internal", "svc-2.aether.internal"}, clusters)

	// b.example.com: only the second vhost's route.
	bVH := byDomain["b.example.com"]
	require.NotNil(t, bVH)
	assert.Equal(t, []string{"b.example.com"}, bVH.GetDomains())
	require.Len(t, bVH.GetRoutes(), 1)
	assert.Equal(t, "svc-2.aether.internal", bVH.GetRoutes()[0].GetRoute().GetCluster())
}

func TestPerDownstreamConnectionPool(t *testing.T) {
	node := newTestCache("node-1")
	assert.True(t, node.perDownstreamConnectionPool(), "node proxy pools per downstream")

	edge := newTestCache("edge-1")
	edge.SetEdgeMode(8080)
	assert.False(t, edge.perDownstreamConnectionPool(), "edge multiplexes on its single identity")
}

// TestEdgeHTTPRedirectOptIn verifies that without the opt-in the HTTP listener
// serves routes directly (no redirect listener), and with the opt-in it emits
// the redirect listener and NO plain HTTP routing listener.
func TestEdgeHTTPRedirectOptIn(t *testing.T) {
	t.Run("no redirect by default", func(t *testing.T) {
		c := newTestCache("edge-1")
		c.SetEdgeMode(80)
		// No SetEdgeHTTPRedirect call → default off.

		ls := stripReadiness(t, c.Listeners())
		require.Len(t, ls, 1)
		l := ls[0]
		assert.Equal(t, proxy.EdgeListenerName, l.GetName(), "HTTP listener serves routes, not a redirect")
		assert.Equal(t, uint32(80), l.GetAddress().GetSocketAddress().GetPortValue())
	})

	t.Run("redirect when opt-in annotation set", func(t *testing.T) {
		c := newTestCache("edge-1")
		c.SetEdgeMode(80)
		c.SetEdgeTLSMode(443)
		c.SetEdgeHTTPRedirect(true)

		ls := stripReadiness(t, c.Listeners())
		names := map[string]*listenerv3.Listener{}
		for _, l := range ls {
			names[l.GetName()] = l
		}
		// The HTTPS routing listener must be present (distinct name from the :80 listener).
		require.NotNil(t, names[proxy.EdgeHTTPSListenerName], "HTTPS routing listener must be present")
		assert.Equal(t, uint32(443), names[proxy.EdgeHTTPSListenerName].GetAddress().GetSocketAddress().GetPortValue())
		// The redirect listener replaces the plain HTTP routing listener.
		require.NotNil(t, names[proxy.EdgeRedirectListenerName], "redirect listener must be present when opt-in is set")
		assert.Equal(t, uint32(80), names[proxy.EdgeRedirectListenerName].GetAddress().GetSocketAddress().GetPortValue())
		// There must be no second plain HTTP routing listener (would conflict with the redirect on port 80).
		assert.Len(t, ls, 2, "exactly TLS listener + redirect listener, no duplicate HTTP listener")
	})

	t.Run("TLS mode without redirect opt-in serves HTTP directly", func(t *testing.T) {
		c := newTestCache("edge-1")
		c.SetEdgeMode(80)
		c.SetEdgeTLSMode(443)
		// TLS enabled but redirect annotation not set → HTTP listener still serves routes.

		ls := stripReadiness(t, c.Listeners())
		require.Len(t, ls, 2, "HTTPS listener + HTTP routing listener")
		// The two listeners MUST have DISTINCT names (edge_https on 443, edge_http on
		// 80) — a shared name collides in the snapshot/LDS and drops :443 (regression).
		byName := map[string]uint32{}
		for _, l := range ls {
			byName[l.GetName()] = l.GetAddress().GetSocketAddress().GetPortValue()
			assert.NotEqual(t, proxy.EdgeRedirectListenerName, l.GetName(), "redirect listener must NOT be present without opt-in")
		}
		assert.Len(t, byName, 2, "the two listeners must have distinct names (no collision)")
		assert.Equal(t, uint32(443), byName[proxy.EdgeHTTPSListenerName], "HTTPS listener named edge_https on 443")
		assert.Equal(t, uint32(80), byName[proxy.EdgeListenerName], "plain HTTP routing listener named edge_http on 80")
	})
}

// TestPerGatewayListenerNamesAllDistinct is a regression guard for #332: every
// per-Gateway listener must have a UNIQUE name. When all listeners shared
// "edge_http", LDS silently dropped the :443 listener, taking down the HTTPS side.
func TestPerGatewayListenerNamesAllDistinct(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80) // enable edge mode

	// Simulate 3 Gateways each with HTTP + HTTPS listeners (6 listeners total).
	gateways := []EdgeGatewayEntry{
		{
			Namespace: "aether-ingress",
			Name:      "edge",
			Listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 80, InternalPort: 18100, HTTPRedirect: false},
				{ExternalPort: 443, InternalPort: 18101, TLSSecretNames: []string{"kubernetes/prod-tls"}},
			},
			VirtualHosts: []VirtualHost{
				{Hosts: []string{"api.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-1"}}},
			},
		},
		{
			Namespace: "conformance",
			Name:      "same-namespace",
			Listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 80, InternalPort: 18200, HTTPRedirect: false},
			},
			VirtualHosts: []VirtualHost{
				{Hosts: []string{"conformance.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-2"}}},
			},
		},
		{
			Namespace: "conformance",
			Name:      "backend-namespaces",
			Listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 80, InternalPort: 18300, HTTPRedirect: false},
			},
			VirtualHosts: []VirtualHost{
				{Hosts: []string{"backend.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-3"}}},
			},
		},
	}
	c.SetEdgeGateways(gateways)

	ls := stripReadiness(t, c.Listeners())
	names := map[string]bool{}
	for _, l := range ls {
		name := l.GetName()
		assert.False(t, names[name], "listener name %q must be unique (duplicate = LDS drop, regression for #332)", name)
		names[name] = true
	}

	// Must have exactly one listener per (Gateway, listener) pair.
	assert.Len(t, names, 4, "3 HTTP + 1 HTTPS = 4 distinct listeners")
}

// TestPerGatewayListenerFallbackToPhase1 verifies that when SetEdgeGateways(nil)
// is called, the cache falls back to the shared Phase 1 listener (edge_http).
func TestPerGatewayListenerFallbackToPhase1(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	// Start with Phase 2 gateways, then reset to nil.
	c.SetEdgeGateways([]EdgeGatewayEntry{
		{
			Namespace: "aether-ingress",
			Name:      "edge",
			Listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 80, InternalPort: 18100},
			},
		},
	})
	c.SetEdgeGateways(nil) // reset to Phase 1

	ls := stripReadiness(t, c.Listeners())
	require.Len(t, ls, 1)
	l := ls[0]
	assert.Equal(t, proxy.EdgeListenerName, l.GetName(), "Phase 1 fallback: listener must be the shared edge_http")
	assert.Equal(t, uint32(80), l.GetAddress().GetSocketAddress().GetPortValue())
}

// TestEdgeReadinessListenerAlwaysPresent is the regression guard for the Phase 2
// readiness wedge: the dedicated readiness listener (edge_readiness on the
// readiness port) MUST be emitted in BOTH Phase 1 (shared listeners) and Phase 2
// (per-Gateway listeners on internal ports, where nothing binds :443), so the
// kubelet readiness probe always has a stable target.
func TestEdgeReadinessListenerAlwaysPresent(t *testing.T) {
	readiness := func(ls []types.Resource) *listenerv3.Listener {
		for _, r := range ls {
			if l := r.(*listenerv3.Listener); l.GetName() == proxy.EdgeReadinessListenerName {
				return l
			}
		}
		return nil
	}

	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	c.SetEdgeTLSMode(443)

	// Phase 1 (shared listeners).
	rl := readiness(c.Listeners())
	require.NotNil(t, rl, "readiness listener must be present in Phase 1")
	assert.Equal(t, uint32(proxy.DefaultEdgeReadinessPort), rl.GetAddress().GetSocketAddress().GetPortValue())

	// Phase 2 (per-Gateway listeners on internal ports — nothing binds :443).
	c.SetEdgeGateways([]EdgeGatewayEntry{
		{Namespace: "aether-ingress", Name: "edge", Listeners: []EdgeGatewayListenerEntry{{ExternalPort: 443, InternalPort: 18100}}},
	})
	rl2 := readiness(c.Listeners())
	require.NotNil(t, rl2, "readiness listener must ALSO be present in Phase 2 (the probe target when :443 is unbound)")
	assert.Equal(t, uint32(proxy.DefaultEdgeReadinessPort), rl2.GetAddress().GetSocketAddress().GetPortValue())
}

// TestPerGatewayRouteConfigIsolation verifies that each Gateway gets its own route
// config with its own virtual hosts (no cross-Gateway leakage).
func TestPerGatewayRouteConfigIsolation(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	c.SetEdgeGateways([]EdgeGatewayEntry{
		{
			Namespace: "ns-a",
			Name:      "gw-a",
			Listeners: []EdgeGatewayListenerEntry{{ExternalPort: 80, InternalPort: 18100}},
			VirtualHosts: []VirtualHost{
				{Hosts: []string{"alpha.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-alpha"}}},
			},
		},
		{
			Namespace: "ns-b",
			Name:      "gw-b",
			Listeners: []EdgeGatewayListenerEntry{{ExternalPort: 80, InternalPort: 18200}},
			VirtualHosts: []VirtualHost{
				{Hosts: []string{"beta.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-beta"}}},
			},
		},
	})

	rcs := c.edgeGatewayRouteConfigs()
	require.Len(t, rcs, 2)

	rcNames := map[string]bool{}
	for _, r := range rcs {
		rc, ok := r.(*routev3.RouteConfiguration)
		require.True(t, ok)
		rcNames[rc.GetName()] = true
		// Each route config must only reference its own domains.
		if rc.GetName() == "edge_rt_ns-a_gw-a" {
			var domains []string
			for _, vh := range rc.GetVirtualHosts() {
				domains = append(domains, vh.GetDomains()...)
			}
			assert.Contains(t, domains, "alpha.example.com")
			assert.NotContains(t, domains, "beta.example.com")
		} else if rc.GetName() == "edge_rt_ns-b_gw-b" {
			var domains []string
			for _, vh := range rc.GetVirtualHosts() {
				domains = append(domains, vh.GetDomains()...)
			}
			assert.Contains(t, domains, "beta.example.com")
			assert.NotContains(t, domains, "alpha.example.com")
		}
	}
	assert.Contains(t, rcNames, "edge_rt_ns-a_gw-a")
	assert.Contains(t, rcNames, "edge_rt_ns-b_gw-b")
}

// TestPerGatewayCertWired verifies that an HTTPS per-Gateway listener references
// the TLS secret names that were passed in the EdgeGatewayListenerEntry, and
// that the listener has a transport socket (TLS termination), not plaintext.
func TestPerGatewayCertWired(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	c.SetEdgeGateways([]EdgeGatewayEntry{
		{
			Namespace: "ns-tls",
			Name:      "secure-gw",
			Listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 443, InternalPort: 18110, TLSSecretNames: []string{"kubernetes/my-cert"}},
			},
			VirtualHosts: []VirtualHost{
				{Hosts: []string{"secure.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-secure"}}},
			},
		},
	})

	ls := stripReadiness(t, c.Listeners())
	require.Len(t, ls, 1)
	l := ls[0]
	assert.Equal(t, "edge_gw_ns-tls_secure-gw_18110", l.GetName())
	// TLS transport socket must be wired (not nil).
	require.NotNil(t, l.GetFilterChains()[0].GetTransportSocket(), "per-Gateway HTTPS listener must terminate TLS")
}

// TestPerGatewayHTTPSListenerRoutesMatchHTTP is the regression guard for
// HTTPRouteRedirectPortAndScheme (https-listener-on-443 subtests returning 404).
//
// When a Gateway has an HTTPS listener (TLSSecretNames set) AND a catch-all
// virtual host (Hosts: nil/empty, i.e. the "*" vhost from effectiveHostnames
// returning nil for a no-hostname route on a no-hostname HTTPS listener), the
// per-Gateway route config must include the redirect routes in the "*" catch-all
// vhost — not only in named-hostname vhosts. Without this, a request like
// "Host: example.org" falls through to the 404 catch-all and returns 404 instead
// of the expected redirect.
//
// This test verifies that:
//  1. The HTTPS per-Gateway listener uses RDS (not inline route config) and
//     references the same route config name as the HTTP case.
//  2. The per-Gateway route config built from a catch-all VirtualHost (empty Hosts)
//     carrying redirect routes exposes those routes in the "*" vhost domain — so
//     any Host header matches and gets the redirect, not the 404.
func TestPerGatewayHTTPSListenerRoutesMatchHTTP(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	// Simulate the effective hostname computation result for a route with no
	// hostnames attached to a Gateway with a no-hostname HTTPS listener: Hosts=nil
	// (true catch-all). The redirect route must survive into the route config.
	entry := EdgeGatewayEntry{
		Namespace: "ns-https",
		Name:      "tls-gw",
		Listeners: []EdgeGatewayListenerEntry{
			// HTTPS listener: TLSSecretNames set, ExternalPort 443.
			{ExternalPort: 443, InternalPort: 18300, TLSSecretNames: []string{"ns-https/tls-cert"}},
		},
		// VirtualHost with empty Hosts (catch-all): produced when
		// effectiveHostnames returns nil for no-hostname route on no-hostname HTTPS listener.
		VirtualHosts: []VirtualHost{
			{
				Hosts: nil, // catch-all — must produce "*" vhost domain in Envoy
				Routes: []Route{
					{
						Prefix:   "/scheme-nil-and-port-nil",
						Redirect: &proxy.GammaRedirect{Hostname: "example.org"},
					},
					{
						Prefix:   "/scheme-http-and-port-80",
						Redirect: &proxy.GammaRedirect{Scheme: "http", Port: 80, Hostname: "example.org"},
					},
				},
			},
		},
	}
	c.SetEdgeGateways([]EdgeGatewayEntry{entry})

	// 1. Listener check: the HTTPS listener must use RDS (not inline route config)
	//    and reference edge_rt_ns-https_tls-gw.
	ls := stripReadiness(t, c.Listeners())
	require.Len(t, ls, 1)
	l := ls[0]
	assert.Equal(t, "edge_gw_ns-https_tls-gw_18300", l.GetName())
	require.NotNil(t, l.GetFilterChains()[0].GetTransportSocket(), "HTTPS listener must terminate TLS")

	hcm := extractHCM(t, l.GetFilterChains()[0])
	rds := hcm.GetRds()
	require.NotNil(t, rds, "per-Gateway HTTPS listener must use RDS (not inline route config)")
	assert.Equal(t, proxy.EdgeGatewayRouteName("ns-https", "tls-gw"), rds.GetRouteConfigName(),
		"HTTPS listener RDS must reference the per-Gateway route config")

	// 2. Route config check: the "*" catch-all vhost must contain the redirect routes,
	//    not only a 404. This is the root cause of the 404 regression:
	//    effectiveHostnames was incorrectly returning ["second-example.org", "*.wildcard.org"]
	//    instead of nil for a no-hostname route on a no-hostname HTTPS listener.
	rcs := c.edgeGatewayRouteConfigs()
	require.Len(t, rcs, 1)
	rc, ok := rcs[0].(*routev3.RouteConfiguration)
	require.True(t, ok)
	assert.Equal(t, proxy.EdgeGatewayRouteName("ns-https", "tls-gw"), rc.GetName())
	assert.True(t, rc.GetIgnorePortInHostMatching(), "IgnorePortInHostMatching must be set")

	// The catch-all vhost ("*") must exist and carry the redirect routes.
	var catchAllVH *routev3.VirtualHost
	for _, vh := range rc.GetVirtualHosts() {
		if len(vh.GetDomains()) == 1 && vh.GetDomains()[0] == "*" {
			catchAllVH = vh
			break
		}
	}
	require.NotNil(t, catchAllVH, "route config must have a '*' catch-all virtual host (from empty Hosts)")

	// The "*" vhost must contain the redirect routes before the terminal 404.
	routes := catchAllVH.GetRoutes()
	require.GreaterOrEqual(t, len(routes), 3, "catch-all vhost must have the 2 redirect routes + the 404 route")

	// Verify at least one redirect action (not a direct-response 404) is present.
	var hasRedirect bool
	for _, r := range routes {
		if r.GetRedirect() != nil {
			hasRedirect = true
			break
		}
	}
	assert.True(t, hasRedirect,
		"'*' catch-all vhost must contain redirect route actions, not only the 404 terminal route")
}

// extractHCM extracts the HttpConnectionManager from a filter chain's first
// network filter. Fails the test if the filter is absent or the wrong type.
func extractHCM(t *testing.T, fc *listenerv3.FilterChain) *hcmv3.HttpConnectionManager {
	t.Helper()
	require.NotEmpty(t, fc.GetFilters(), "filter chain must have at least one filter")
	f := fc.GetFilters()[0]
	hcm := &hcmv3.HttpConnectionManager{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(hcm), "first filter must be HttpConnectionManager")
	return hcm
}

// TestVirtualHostVhosts_WeightedSplit verifies end-to-end that a route with
// multiple weighted Backends produces a weighted_clusters Envoy route action.
// Each backend independently resolves via edgeClusterNameLocked (mesh vs cleartext).
func TestVirtualHostVhosts_WeightedSplit(t *testing.T) {
	c := newTestCache("edge-1")

	// Register svc-a in the mesh registry; svc-b is a plain k8s Service (non-mesh).
	c.clusterMu.Lock()
	c.clusters["default/svc-a"] = clusterEntry{service: "default/svc-a"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{
			Hosts: []string{"split.example.com"},
			Routes: []Route{{
				Prefix: "/split",
				// Weighted backends: svc-a (mesh, port 0 = default cluster) weight=3,
				// svc-b (k8s cleartext, port 9090) weight=1.
				Backends: []RouteBackend{
					{Service: "svc-a", BackendNamespace: "default", Port: 0, Weight: 3},
					{Service: "svc-b", BackendNamespace: "conformance-ns", Port: 9090, Weight: 1},
				},
				Service:          "svc-a", // legacy field: first backend
				Port:             0,
				BackendNamespace: "default",
			}},
		},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 1)
	routes := vhosts[0].GetRoutes()
	require.Len(t, routes, 1)

	ra := routes[0].GetRoute()
	require.NotNil(t, ra, "must have a RouteAction (not a redirect)")

	wc := ra.GetWeightedClusters()
	require.NotNil(t, wc, "multiple backends must produce weighted_clusters")
	assert.Equal(t, uint32(4), wc.GetTotalWeight().GetValue(), "total_weight = 3+1 = 4")

	require.Len(t, wc.GetClusters(), 2)
	// First backend: svc-a is in the registry → mesh default cluster.
	assert.Equal(t, "svc-a.default.aether.internal", wc.GetClusters()[0].GetName())
	assert.Equal(t, uint32(3), wc.GetClusters()[0].GetWeight().GetValue())
	// Second backend: svc-b is NOT in the registry → edge_k8s cleartext cluster.
	assert.Equal(t, "edge_k8s_conformance-ns_svc-b_9090", wc.GetClusters()[1].GetName())
	assert.Equal(t, uint32(1), wc.GetClusters()[1].GetWeight().GetValue())
}

// TestVirtualHostVhosts_SingleBackendNoWeightedClusters verifies that a route with
// a single Backends entry uses the plain single-cluster RouteAction (not weighted_clusters).
func TestVirtualHostVhosts_SingleBackendNoWeightedClusters(t *testing.T) {
	c := newTestCache("edge-1")
	// Register svc-1 in mesh (default port path — no explicit per-port cluster needed).
	c.clusterMu.Lock()
	c.clusters["default/svc-1"] = clusterEntry{service: "default/svc-1"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{
			Hosts: []string{"api.example.com"},
			Routes: []Route{{
				Prefix: "/",
				Backends: []RouteBackend{
					// Port 0 → default mesh cluster (svc-1.default.aether.internal).
					{Service: "svc-1", BackendNamespace: "default", Port: 0, Weight: 1},
				},
				Service:          "svc-1",
				Port:             0,
				BackendNamespace: "default",
			}},
		},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 1)
	routes := vhosts[0].GetRoutes()
	require.Len(t, routes, 1)

	ra := routes[0].GetRoute()
	require.NotNil(t, ra)
	assert.Equal(t, "svc-1.default.aether.internal", ra.GetCluster(), "single backend must use plain cluster")
	assert.Nil(t, ra.GetWeightedClusters(), "single backend must NOT use weighted_clusters")
}

// TestEdgeK8sBackendClusters_WeightedBackends verifies that edgeK8sBackendClusters
// emits a cleartext cluster for every non-mesh weighted backend (iterating Backends),
// deduplicating by (namespace, service, port) as with single-backend routes.
func TestEdgeK8sBackendClusters_WeightedBackends(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	// mesh-svc in registry; plain-svc-a and plain-svc-b are non-mesh.
	c.clusterMu.Lock()
	c.clusters["default/mesh-svc"] = clusterEntry{service: "default/mesh-svc"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{
			Hosts: []string{"split.example.com"},
			Routes: []Route{{
				Prefix: "/",
				Backends: []RouteBackend{
					{Service: "mesh-svc", BackendNamespace: "default", Port: 8080, Weight: 1}, // mesh → skipped
					{Service: "plain-svc-a", BackendNamespace: "ns-a", Port: 9000, Weight: 2}, // non-mesh → cluster
					{Service: "plain-svc-b", BackendNamespace: "ns-b", Port: 8080, Weight: 1}, // non-mesh → cluster
					// Duplicate of plain-svc-a/9000 — must be deduped.
					{Service: "plain-svc-a", BackendNamespace: "ns-a", Port: 9000, Weight: 1},
				},
				Service:          "mesh-svc",
				BackendNamespace: "default",
			}},
		},
	})

	clusters := c.edgeK8sBackendClusters()
	// mesh-svc → skipped; plain-svc-a deduplicated to 1; plain-svc-b → 1. Total = 2.
	require.Len(t, clusters, 2, "mesh cluster skipped; duplicates deduped; 2 cleartext clusters")

	names := make(map[string]bool)
	for _, r := range clusters {
		cl, ok := r.(*clusterv3.Cluster)
		require.True(t, ok)
		names[cl.GetName()] = true
	}
	assert.True(t, names["edge_k8s_ns-a_plain-svc-a_9000"], "non-mesh svc-a cluster must be emitted")
	assert.True(t, names["edge_k8s_ns-b_plain-svc-b_8080"], "non-mesh svc-b cluster must be emitted")
}

// TestEdgeClusterNameLocked_MeshVsNonMesh verifies that edgeClusterNameLocked
// returns the mesh cluster name when the service IS in the registry, and the
// edge_k8s_… cleartext name when it is NOT.
func TestEdgeClusterNameLocked_MeshVsNonMesh(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	// Register "mesh-svc" as a registry service.
	c.clusterMu.Lock()
	c.clusters["default/mesh-svc"] = clusterEntry{service: "default/mesh-svc"}
	c.clusterMu.Unlock()

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	// Mesh path: service in registry → mesh FQDN.
	name := c.edgeClusterNameLocked("mesh-svc", "default", 0)
	assert.Equal(t, "mesh-svc.default.aether.internal", name, "registry service resolves to mesh cluster")

	// Non-mesh path: service NOT in registry → edge_k8s_… cleartext name.
	name2 := c.edgeClusterNameLocked("plain-svc", "conformance-ns", 8080)
	assert.Equal(t, "edge_k8s_conformance-ns_plain-svc_8080", name2, "non-registry service resolves to cleartext k8s cluster")

	// Non-mesh with port 0 defaults to :80.
	name3 := c.edgeClusterNameLocked("plain-svc", "conformance-ns", 0)
	assert.Equal(t, "edge_k8s_conformance-ns_plain-svc_80", name3, "non-registry service with port 0 defaults to :80")
}

// TestEdgeK8sBackendClusters_NonMeshOnly verifies that edgeK8sBackendClusters
// emits one STRICT_DNS cleartext cluster per unique (namespace, service, port)
// that is NOT in the registry, and skips any service that IS registry-registered.
func TestEdgeK8sBackendClusters_NonMeshOnly(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	// Register "mesh-svc" as a registry service; "plain-svc" is NOT registered.
	c.clusterMu.Lock()
	c.clusters["default/mesh-svc"] = clusterEntry{service: "default/mesh-svc"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{
			Hosts: []string{"api.example.com"},
			Routes: []Route{
				{Prefix: "/mesh", Service: "mesh-svc", Port: 8080, BackendNamespace: "default"},
				{Prefix: "/plain", Service: "plain-svc", Port: 9000, BackendNamespace: "conformance-ns"},
			},
		},
		{
			Hosts: []string{"other.example.com"},
			Routes: []Route{
				// Same (namespace, service, port) as above — must be deduped to one cluster.
				{Prefix: "/", Service: "plain-svc", Port: 9000, BackendNamespace: "conformance-ns"},
			},
		},
	})

	clusters := c.edgeK8sBackendClusters()
	// Only one cleartext cluster: plain-svc/9000 (mesh-svc is in registry → no cleartext cluster).
	require.Len(t, clusters, 1, "exactly one non-mesh backend cluster (mesh-svc skipped; duplicate plain-svc/9000 deduped)")

	cl, ok := clusters[0].(*clusterv3.Cluster)
	require.True(t, ok)
	assert.Equal(t, "edge_k8s_conformance-ns_plain-svc_9000", cl.GetName())
	assert.Equal(t, clusterv3.Cluster_STRICT_DNS, cl.GetType())
	assert.Nil(t, cl.GetTransportSocket(), "cleartext cluster must have NO transport socket")
	ep := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "plain-svc.conformance-ns.svc.cluster.local", ep.GetAddress())
	assert.Equal(t, uint32(9000), ep.GetPortValue())
}

// TestEdgeK8sBackendClusters_HeadlessDialPort verifies that a non-mesh backend with a
// DialPort (set for a headless Service, whose FQDN resolves to pod IPs) produces a
// STRICT_DNS cluster that DIALS the DialPort while keeping the cluster name keyed by
// the service Port. This is the HTTPRouteServiceTypes conformance fix: dialing the
// service port (8080) against pod IPs is refused; the pods listen on targetPort 3000.
func TestEdgeK8sBackendClusters_HeadlessDialPort(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	c.SetVirtualHosts([]VirtualHost{
		{
			Hosts: []string{"api.example.com"},
			Routes: []Route{
				// Headless backend: service port 8080, but pods listen on 3000.
				{Prefix: "/headless", Service: "headless-svc", Port: 8080, DialPort: 3000, BackendNamespace: "ns"},
			},
		},
	})

	clusters := c.edgeK8sBackendClusters()
	require.Len(t, clusters, 1)
	cl, ok := clusters[0].(*clusterv3.Cluster)
	require.True(t, ok)
	// Cluster name stays keyed by the SERVICE port so route→cluster names match.
	assert.Equal(t, "edge_k8s_ns_headless-svc_8080", cl.GetName())
	ep := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "headless-svc.ns.svc.cluster.local", ep.GetAddress())
	// The endpoint DIALS the targetPort (3000), not the service port (8080).
	assert.Equal(t, uint32(3000), ep.GetPortValue(), "headless cluster must dial the targetPort")
}

// TestVirtualHostVhostsSameHostMerge verifies that two VirtualHosts sharing a
// hostname have their routes MERGED into ONE Envoy vhost — not keep-first-drop.
// This is the Gateway API HTTPRouteMatchingAcrossRoutes behavior: multiple
// HTTPRoutes sharing a hostname on one Gateway must all contribute their routes.
func TestVirtualHostVhostsSameHostMerge(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-a"] = clusterEntry{service: "svc-a"}
	c.clusters["svc-b"] = clusterEntry{service: "svc-b"}
	c.clusterMu.Unlock()

	// Two VirtualHosts sharing "shared.example.com" — different services on different prefixes.
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"shared.example.com"}, Routes: []Route{{Prefix: "/api", Service: "svc-a"}}},
		{Hosts: []string{"shared.example.com"}, Routes: []Route{{Prefix: "/web", Service: "svc-b"}}},
	})

	vhosts := c.virtualHostVhosts()
	// Must produce exactly ONE vhost for shared.example.com (not two, not one with the other dropped).
	var sharedVH []*routev3.VirtualHost
	for _, vh := range vhosts {
		if vh.GetName() == "shared.example.com" {
			sharedVH = append(sharedVH, vh)
		}
	}
	require.Len(t, sharedVH, 1, "two VirtualHosts sharing a hostname must merge into ONE Envoy vhost")
	routes := sharedVH[0].GetRoutes()
	require.Len(t, routes, 2, "both routes from both VirtualHosts must be present")
	// FIX 1: non-"/" prefixes use path_separated_prefix.
	prefixes := []string{routes[0].GetMatch().GetPathSeparatedPrefix(), routes[1].GetMatch().GetPathSeparatedPrefix()}
	assert.ElementsMatch(t, []string{"/api", "/web"}, prefixes, "both routes present after merge")
}

// TestVirtualHostVhostsSameHostMerge_Specificity verifies that merged routes are
// ordered by Gateway API path specificity: Exact > longer prefix > shorter prefix.
func TestVirtualHostVhostsSameHostMerge_Specificity(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-a"] = clusterEntry{service: "svc-a"}
	c.clusters["svc-b"] = clusterEntry{service: "svc-b"}
	c.clusters["svc-c"] = clusterEntry{service: "svc-c"}
	c.clusterMu.Unlock()

	// Three VirtualHosts sharing "api.example.com", routes at /, /v2, /v2/exact.
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-a"}}},
		{Hosts: []string{"api.example.com"}, Routes: []Route{{Prefix: "/v2", Service: "svc-b"}}},
		{Hosts: []string{"api.example.com"}, Routes: []Route{{Exact: "/v2/exact", Service: "svc-c"}}},
	})

	vhosts := c.virtualHostVhosts()
	var merged *routev3.VirtualHost
	for _, vh := range vhosts {
		if vh.GetName() == "api.example.com" {
			merged = vh
		}
	}
	require.NotNil(t, merged, "merged vhost must be present")
	routes := merged.GetRoutes()
	require.Len(t, routes, 3)
	// Exact first.
	assert.Equal(t, "/v2/exact", routes[0].GetMatch().GetPath(), "exact match must be first")
	// Longer prefix second: FIX 1 — non-"/" prefix uses path_separated_prefix.
	assert.Equal(t, "/v2", routes[1].GetMatch().GetPathSeparatedPrefix(), "longer prefix must be second")
	// Catch-all prefix last: "/" stays as plain prefix.
	assert.Equal(t, "/", routes[2].GetMatch().GetPrefix(), "catch-all prefix must be last")
}

// TestVirtualHostVhostsSameHostMerge_DomainsNotDuplicated verifies that the merged
// Envoy vhost only has the domain listed ONCE, not once per contributing VirtualHost
// (Envoy NACKs duplicate domains).
func TestVirtualHostVhostsSameHostMerge_DomainsNotDuplicated(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-a"] = clusterEntry{service: "svc-a"}
	c.clusters["svc-b"] = clusterEntry{service: "svc-b"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"shared.example.com"}, Routes: []Route{{Prefix: "/a", Service: "svc-a"}}},
		{Hosts: []string{"shared.example.com"}, Routes: []Route{{Prefix: "/b", Service: "svc-b"}}},
	})

	vhosts := c.virtualHostVhosts()
	for _, vh := range vhosts {
		if vh.GetName() == "shared.example.com" {
			domains := vh.GetDomains()
			assert.Equal(t, []string{"shared.example.com"}, domains, "domain must appear exactly ONCE (Envoy NACKs duplicates)")
			return
		}
	}
	t.Fatal("shared.example.com vhost not found")
}

// TestBuildEdgeK8sCluster verifies the cluster builder produces a STRICT_DNS
// cleartext cluster with the right FQDN and no transport socket.
func TestBuildEdgeK8sCluster_Shape(t *testing.T) {
	cl := proxy.BuildEdgeK8sCluster("edge_k8s_ns_svc_8080", "svc.ns.svc.cluster.local", 8080)

	assert.Equal(t, "edge_k8s_ns_svc_8080", cl.GetName())
	assert.Equal(t, clusterv3.Cluster_STRICT_DNS, cl.GetType())
	assert.Nil(t, cl.GetTransportSocket(), "k8s cleartext cluster must have NO transport socket")
	la := cl.GetLoadAssignment()
	require.NotNil(t, la)
	ep := la.GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "svc.ns.svc.cluster.local", ep.GetAddress())
	assert.Equal(t, uint32(8080), ep.GetPortValue())
}

// TestSortRoutesBySpecificity verifies Gateway API path-precedence ordering:
// Exact beats any prefix; longer prefix beats shorter prefix; ties preserve
// input order (stable sort).
func TestSortRoutesBySpecificity(t *testing.T) {
	// Input order: / (shortest prefix), /v2 (prefix), /v2/exact (exact).
	// Expected after sort: /v2/exact (exact), /v2 (longer prefix), / (shorter prefix).
	routes := []Route{
		{Prefix: "/", Service: "svc-root"},
		{Prefix: "/v2", Service: "svc-v2"},
		{Exact: "/v2/exact", Service: "svc-exact"},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 3)
	assert.Equal(t, "/v2/exact", routes[0].Exact, "exact match must come first")
	assert.Equal(t, "svc-exact", routes[0].Service)
	assert.Equal(t, "/v2", routes[1].Prefix, "longer prefix before shorter")
	assert.Equal(t, "svc-v2", routes[1].Service)
	assert.Equal(t, "/", routes[2].Prefix, "catch-all prefix last")
	assert.Equal(t, "svc-root", routes[2].Service)
}

// TestSortRoutesBySpecificity_StableOnTie verifies that routes with the same
// path specificity preserve their input order (stable sort = tie-break order).
func TestSortRoutesBySpecificity_StableOnTie(t *testing.T) {
	// Three routes all with prefix "/api" — same length, same type, different services.
	// Input order must be preserved.
	routes := []Route{
		{Prefix: "/api", Service: "svc-a"},
		{Prefix: "/api", Service: "svc-b"},
		{Prefix: "/api", Service: "svc-c"},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 3)
	assert.Equal(t, "svc-a", routes[0].Service, "first equal-specificity route stays first")
	assert.Equal(t, "svc-b", routes[1].Service)
	assert.Equal(t, "svc-c", routes[2].Service)
}

// TestVirtualHostPathRoutes_SpecificitySort verifies that routes on a named vhost
// are re-ordered by path specificity regardless of their input order:
// Exact > longer prefix > shorter prefix; the 404 stays last.
func TestVirtualHostPathRoutes_SpecificitySort(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-root"] = clusterEntry{service: "svc-root"}
	c.clusters["svc-v2"] = clusterEntry{service: "svc-v2"}
	c.clusters["svc-exact"] = clusterEntry{service: "svc-exact"}
	c.clusterMu.Unlock()

	// Input order: / (shortest) → /v2 (prefix) → /v2/exact (exact).
	// Envoy must see them as: exact → /v2 → / (most-specific first).
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com"}, Routes: []Route{
			{Prefix: "/", Service: "svc-root"},
			{Prefix: "/v2", Service: "svc-v2"},
			{Exact: "/v2/exact", Service: "svc-exact"},
		}},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 1)
	routes := vhosts[0].GetRoutes()
	require.Len(t, routes, 3, "three application routes (no 404 on named vhosts — 404 is on the catch-all only)")
	assert.NotEmpty(t, routes[0].GetMatch().GetPath(), "first route must be exact")
	assert.Equal(t, "/v2/exact", routes[0].GetMatch().GetPath())
	assert.Equal(t, "/v2", routes[1].GetMatch().GetPathSeparatedPrefix(), "longer prefix second")
	assert.Equal(t, "/", routes[2].GetMatch().GetPrefix(), "catch-all prefix last")
}

// TestCatchAllVhostSpecificitySort verifies that routes merged into the "*" catch-all
// vhost (from multiple hostname-less HTTPRoutes) are ordered by path specificity.
// The 404 fallback is appended by appendEdgeCatchAll404 (called inside
// BuildEdgeRouteConfiguration), so this test uses the full route config to verify
// the 404 stays last.
func TestCatchAllVhostSpecificitySort(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-root"] = clusterEntry{service: "svc-root"}
	c.clusters["svc-v2"] = clusterEntry{service: "svc-v2"}
	c.clusters["svc-exact"] = clusterEntry{service: "svc-exact"}
	c.clusterMu.Unlock()

	// Three hostname-less vhosts (different HTTPRoutes) in order: /, /v2, /v2/exact.
	// They merge into the single "*" vhost and must be sorted most-specific-first.
	c.SetVirtualHosts([]VirtualHost{
		{Routes: []Route{{Prefix: "/", Service: "svc-root"}}},
		{Routes: []Route{{Prefix: "/v2", Service: "svc-v2"}}},
		{Routes: []Route{{Exact: "/v2/exact", Service: "svc-exact"}}},
	})

	// virtualHostVhosts returns vhosts without the 404 (that is added by
	// appendEdgeCatchAll404 inside BuildEdgeRouteConfiguration).
	vhosts := c.virtualHostVhosts()
	var starVH *routev3.VirtualHost
	for _, vh := range vhosts {
		if vh.GetName() == "*" {
			starVH = vh
			break
		}
	}
	require.NotNil(t, starVH, "catch-all * vhost must be present")

	// Verify path-specificity order (no 404 at this stage).
	routes := starVH.GetRoutes()
	require.Len(t, routes, 3, "three application routes before 404 is appended")
	assert.NotEmpty(t, routes[0].GetMatch().GetPath(), "first route must be exact match")
	assert.Equal(t, "/v2/exact", routes[0].GetMatch().GetPath())
	assert.Equal(t, "/v2", routes[1].GetMatch().GetPathSeparatedPrefix(), "longer prefix second")
	assert.Equal(t, "/", routes[2].GetMatch().GetPrefix(), "catch-all prefix third")

	// Build the full route config to verify the 404 is appended last.
	rc := proxy.BuildEdgeRouteConfiguration(vhosts)
	var starFull *routev3.VirtualHost
	for _, vh := range rc.GetVirtualHosts() {
		if vh.GetName() == "*" {
			starFull = vh
			break
		}
	}
	require.NotNil(t, starFull, "* vhost must exist in full route config")
	allRoutes := starFull.GetRoutes()
	require.Len(t, allRoutes, 4, "3 application routes + 1 catch-all 404")
	last := allRoutes[len(allRoutes)-1]
	require.NotNil(t, last.GetDirectResponse(), "last route must be the 404 fallback")
	assert.Equal(t, uint32(404), last.GetDirectResponse().GetStatus())
}

// --- Header/method/query sort dimension tests ---

// TestSortRoutesBySpecificity_HeaderCountRanksHigher verifies that two routes with
// identical paths are ordered so the one with MORE header matchers sorts first.
func TestSortRoutesBySpecificity_HeaderCountRanksHigher(t *testing.T) {
	routes := []Route{
		{Prefix: "/api", Service: "svc-fewer", Headers: []proxy.RouteHeaderMatch{{Name: "h1", Value: "v1"}}},
		{Prefix: "/api", Service: "svc-more", Headers: []proxy.RouteHeaderMatch{{Name: "h1", Value: "v1"}, {Name: "h2", Value: "v2"}}},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 2)
	assert.Equal(t, "svc-more", routes[0].Service, "route with 2 headers must sort before route with 1")
	assert.Equal(t, "svc-fewer", routes[1].Service)
}

// TestSortRoutesBySpecificity_MethodRanksAboveNoMethod verifies that a route with a
// method matcher ranks higher than an identical route without one.
func TestSortRoutesBySpecificity_MethodRanksAboveNoMethod(t *testing.T) {
	routes := []Route{
		{Prefix: "/form", Service: "svc-any"},
		{Prefix: "/form", Service: "svc-post", Method: "POST"},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 2)
	assert.Equal(t, "svc-post", routes[0].Service, "route with method must sort before route without")
	assert.Equal(t, "svc-any", routes[1].Service)
}

// TestSortRoutesBySpecificity_QueryCountRanksHigher verifies that a route with more
// query-param matchers sorts before one with fewer (path and headers equal).
func TestSortRoutesBySpecificity_QueryCountRanksHigher(t *testing.T) {
	routes := []Route{
		{Prefix: "/q", Service: "svc-one", QueryParams: []proxy.RouteQueryParamMatch{{Name: "a", Value: "1"}}},
		{Prefix: "/q", Service: "svc-two", QueryParams: []proxy.RouteQueryParamMatch{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 2)
	assert.Equal(t, "svc-two", routes[0].Service, "route with 2 query params must sort before route with 1")
	assert.Equal(t, "svc-one", routes[1].Service)
}

// TestSortRoutesBySpecificity_FullDimensionOrder verifies the full priority per the
// Gateway API spec: path key > METHOD present > header count > query count. Method
// ranks ABOVE header count (a method-only rule outranks a header-only rule).
func TestSortRoutesBySpecificity_FullDimensionOrder(t *testing.T) {
	// All routes share prefix "/api" so path keys are equal.
	// Expected order: most-specific first.
	routes := []Route{
		// headers=0, method="", query=0 — least specific.
		{Prefix: "/api", Service: "svc-bare"},
		// headers=1, method="", query=0.
		{Prefix: "/api", Service: "svc-hdr", Headers: []proxy.RouteHeaderMatch{{Name: "x", Value: "1"}}},
		// headers=0, method="GET", query=0 — method outranks header-only.
		{Prefix: "/api", Service: "svc-method", Method: "GET"},
		// headers=1, method="POST", query=1 — most specific.
		{
			Prefix: "/api", Service: "svc-full",
			Headers:     []proxy.RouteHeaderMatch{{Name: "x", Value: "1"}},
			Method:      "POST",
			QueryParams: []proxy.RouteQueryParamMatch{{Name: "v", Value: "2"}},
		},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 4)
	assert.Equal(t, "svc-full", routes[0].Service, "method+headers+query = most specific")
	assert.Equal(t, "svc-method", routes[1].Service, "method match outranks header-only (spec: method > headers)")
	assert.Equal(t, "svc-hdr", routes[2].Service, "header only = third")
	assert.Equal(t, "svc-bare", routes[3].Service, "bare path = least specific")
}

// TestSortRoutesBySpecificity_MethodVsHeader_ConformanceProbe11 is the regression
// test for HTTPRouteMethodMatching probe 11 ("PATCH /" with header version=four).
// Two pathless rules match: one on method PATCH (→ backend-v2), one on header
// version=four (→ backend-v3). The Gateway API spec ranks Method ABOVE header count,
// so the method rule must win. The old layout (header count above method) sent the
// request to the header rule (backend-v3), failing conformance.
func TestSortRoutesBySpecificity_MethodVsHeader_ConformanceProbe11(t *testing.T) {
	routes := []Route{
		// Header-only rule (version=four → infra-backend-v3).
		{Prefix: "/", Service: "infra-backend-v3", Headers: []proxy.RouteHeaderMatch{{Name: "version", Value: "four"}}},
		// Method-only rule (PATCH → infra-backend-v2).
		{Prefix: "/", Service: "infra-backend-v2", Method: "PATCH"},
	}
	sortRoutesBySpecificity(routes)

	require.Len(t, routes, 2)
	assert.Equal(t, "infra-backend-v2", routes[0].Service,
		"a method match must outrank a header match (Gateway API precedence: method > headers)")
}

// TestDirectResponseRoute_AdmittedByBuildEdgeVhosts verifies that a
// DirectResponseStatus=500 route is admitted (not dropped as inadmissible) and
// appears in the Envoy vhost's route list as a direct_response action.
func TestDirectResponseRoute_AdmittedByBuildEdgeVhosts(t *testing.T) {
	c := newTestCache("edge-1")
	// No cluster registration needed; direct_response routes have no upstream.
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com"}, Routes: []Route{
			{Prefix: "/bad", DirectResponseStatus: 500},
		}},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 1)
	routes := vhosts[0].GetRoutes()
	require.Len(t, routes, 1)
	assert.NotNil(t, routes[0].GetDirectResponse(), "direct_response action must be present")
	assert.Equal(t, uint32(500), routes[0].GetDirectResponse().GetStatus())
}

// --- Wildcard hostname vhost routing (HTTPRouteListenerHostnameMatching /
// HTTPRouteHostnameIntersection conformance regression guards) ---

// TestBuildEdgeVhostsLocked_SpecificHostUnderWildcard is the conformance
// regression guard for HTTPRouteListenerHostnameMatching: when effectiveHostnames
// computes "baz.bar.com" for a *.bar.com listener ∩ baz.bar.com route,
// buildEdgeVhostsLocked must emit a vhost with domain "baz.bar.com" (not the
// catch-all "*"). This ensures requests to baz.bar.com route to the correct
// backend instead of the catch-all.
func TestBuildEdgeVhostsLocked_SpecificHostUnderWildcard(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-v3"] = clusterEntry{service: "svc-v3"}
	c.clusters["svc-v1"] = clusterEntry{service: "svc-v1"}
	c.clusterMu.Unlock()

	// effectiveHostnames("baz.bar.com", listener "*.bar.com") = "baz.bar.com".
	// The vhost is given the intersection result as its Hosts.
	c.SetVirtualHosts([]VirtualHost{
		// v3 backend: specific intersection host baz.bar.com (from *.bar.com ∩ baz.bar.com).
		{Hosts: []string{"baz.bar.com"}, Routes: []Route{{Prefix: "/", Service: "svc-v3"}}},
		// v1 catch-all: hostname-less route → "*" catch-all.
		{Routes: []Route{{Prefix: "/", Service: "svc-v1"}}},
	})

	vhosts := c.virtualHostVhosts()
	// Expected: one named vhost "baz.bar.com" + one catch-all "*".
	require.Len(t, vhosts, 2)

	byDomain := map[string]*routev3.VirtualHost{}
	for _, vh := range vhosts {
		byDomain[vh.GetName()] = vh
	}

	// baz.bar.com must be a named vhost, NOT the catch-all.
	bazVH := byDomain["baz.bar.com"]
	require.NotNil(t, bazVH, "baz.bar.com must get its own named vhost (intersection result)")
	assert.Equal(t, []string{"baz.bar.com"}, bazVH.GetDomains())
	assert.Equal(t, "svc-v3.aether.internal", bazVH.GetRoutes()[0].GetRoute().GetCluster())

	// The catch-all "*" holds the v1 backend.
	starVH := byDomain["*"]
	require.NotNil(t, starVH)
	assert.Equal(t, "svc-v1.aether.internal", starVH.GetRoutes()[0].GetRoute().GetCluster())
}

// TestBuildEdgeVhostsLocked_WildcardDomain verifies that when the effective
// hostname is a wildcard "*.bar.com" (from *.bar.com listener ∩ *.bar.com route),
// buildEdgeVhostsLocked emits a vhost with domain "*.bar.com" — NOT the catch-all
// "*". Envoy matches "*.bar.com" to any request whose Host has bar.com as suffix,
// so this is a real named vhost, not the catch-all.
func TestBuildEdgeVhostsLocked_WildcardDomain(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-backend"] = clusterEntry{service: "svc-backend"}
	c.clusterMu.Unlock()

	// effectiveHostnames("*.bar.com", listener "*.bar.com") = "*.bar.com".
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"*.bar.com"}, Routes: []Route{{Prefix: "/", Service: "svc-backend"}}},
	})

	vhosts := c.virtualHostVhosts()
	// Only one vhost: "*.bar.com". No catch-all "*" (there are no hostname-less routes).
	require.Len(t, vhosts, 1)
	vh := vhosts[0]
	assert.Equal(t, "*.bar.com", vh.GetName(), "wildcard hostname must produce named vhost, NOT catch-all *")
	assert.Equal(t, []string{"*.bar.com"}, vh.GetDomains())
	assert.Equal(t, "svc-backend.aether.internal", vh.GetRoutes()[0].GetRoute().GetCluster())
}

// TestBuildEdgeVhostsLocked_WildcardAndSpecificIsolated verifies the full
// HTTPRouteHostnameIntersection conformance shape: a *.bar.com listener has two
// routes — one with hostname baz.bar.com (intersection = baz.bar.com) and one
// with hostname *.bar.com (intersection = *.bar.com). Each must get its own
// named vhost; neither must land in the catch-all "*".
// This mirrors the data-plane state after effectiveHostnames runs correctly.
func TestBuildEdgeVhostsLocked_WildcardAndSpecificIsolated(t *testing.T) {
	c := newTestCache("edge-1")
	c.clusterMu.Lock()
	c.clusters["svc-specific"] = clusterEntry{service: "svc-specific"}
	c.clusters["svc-wildcard"] = clusterEntry{service: "svc-wildcard"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		// baz.bar.com route: intersection of *.bar.com listener ∩ baz.bar.com route.
		{Hosts: []string{"baz.bar.com"}, Routes: []Route{{Prefix: "/", Service: "svc-specific"}}},
		// *.bar.com route: intersection of *.bar.com listener ∩ *.bar.com route.
		{Hosts: []string{"*.bar.com"}, Routes: []Route{{Prefix: "/", Service: "svc-wildcard"}}},
	})

	vhosts := c.virtualHostVhosts()
	// Two named vhosts; no catch-all "*".
	require.Len(t, vhosts, 2)

	byName := map[string]*routev3.VirtualHost{}
	for _, vh := range vhosts {
		byName[vh.GetName()] = vh
	}

	bazVH := byName["baz.bar.com"]
	require.NotNil(t, bazVH, "baz.bar.com must be a named vhost")
	assert.Equal(t, "svc-specific.aether.internal", bazVH.GetRoutes()[0].GetRoute().GetCluster())

	wildVH := byName["*.bar.com"]
	require.NotNil(t, wildVH, "*.bar.com must be a named vhost (not catch-all *)")
	assert.Equal(t, []string{"*.bar.com"}, wildVH.GetDomains())
	assert.Equal(t, "svc-wildcard.aether.internal", wildVH.GetRoutes()[0].GetRoute().GetCluster())

	// Confirm no catch-all "*" was emitted.
	assert.Nil(t, byName["*"], "no hostname-less routes → no catch-all * vhost")
}

// TestPerGatewayRedirectPreservesExternalPort is the regression test for
// HTTPRouteRedirectPortAndScheme subtest "scheme-nil-and-port-nil" on a
// http-listener-on-8080 Gateway (the LONE remaining Extended conformance fail
// before this fix).
//
// Root cause: edgeGatewayRouteConfigs builds the per-Gateway route config without
// passing the listener's ExternalPort. A redirect route with neither Port nor Scheme
// (pure "preserve original port" semantics) ends up with GammaRedirect.Port=0 and
// GammaRedirect.Scheme="". gammaRedirectAction then leaves port_redirect=0, which
// makes Envoy use the listener's BOUND (internal) port — not the external port 8080
// the client connected to via the LB Service. The Location header becomes
// "http://host:18101/path" instead of "http://host:8080/path".
//
// Fix: gatewayExternalHTTPPort finds the external port of the non-redirect, non-TLS
// listener. buildEdgeVhostsLocked injects it into GammaRedirect.ListenerPort for
// redirect routes with Port=0 and Scheme="". gammaRedirectAction emits
// port_redirect=8080 for non-standard ports; for default ports (80, 443) it leaves
// port_redirect=0 so the port is correctly omitted from Location.
func TestPerGatewayRedirectPreservesExternalPort(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)

	// Gateway with HTTP listener on external port 8080, internal port 18101.
	// This is the per-Gateway addressing pattern: LB Service maps 8080 → 18101.
	entry := EdgeGatewayEntry{
		Namespace: "gateway-conformance-infra",
		Name:      "http-gateway-8080",
		Listeners: []EdgeGatewayListenerEntry{
			{ExternalPort: 8080, InternalPort: 18101, HTTPRedirect: false},
		},
		VirtualHosts: []VirtualHost{
			{
				// Catch-all: no hostname, matches all requests.
				Hosts: nil,
				Routes: []Route{
					{
						// scheme-nil-and-port-nil: no Scheme, no Port → must emit port_redirect=8080.
						Prefix:   "/scheme-nil-and-port-nil",
						Redirect: &proxy.GammaRedirect{StatusCode: 302},
					},
					{
						// scheme-nil-and-port-80: explicit Port=80 → port_redirect=80 always.
						Prefix:   "/scheme-nil-and-port-80",
						Redirect: &proxy.GammaRedirect{Port: 80, StatusCode: 302},
					},
					{
						// scheme-https-and-port-nil: Scheme=https → port_redirect=443 (scheme default).
						Prefix:   "/scheme-https-and-port-nil",
						Redirect: &proxy.GammaRedirect{Scheme: "https", StatusCode: 302},
					},
				},
			},
		},
	}
	c.SetEdgeGateways([]EdgeGatewayEntry{entry})

	rcs := c.edgeGatewayRouteConfigs()
	require.Len(t, rcs, 1)
	rc, ok := rcs[0].(*routev3.RouteConfiguration)
	require.True(t, ok)
	assert.Equal(t, proxy.EdgeGatewayRouteName("gateway-conformance-infra", "http-gateway-8080"), rc.GetName())

	// Find the "*" catch-all vhost (from nil Hosts).
	var catchAll *routev3.VirtualHost
	for _, vh := range rc.GetVirtualHosts() {
		if len(vh.GetDomains()) == 1 && vh.GetDomains()[0] == "*" {
			catchAll = vh
			break
		}
	}
	require.NotNil(t, catchAll, "catch-all '*' vhost must be present for the hostname-less VirtualHost")

	// Index redirect routes by path prefix for easy lookup.
	redirectByPath := make(map[string]*routev3.RedirectAction)
	for _, r := range catchAll.GetRoutes() {
		rd := r.GetRedirect()
		if rd == nil {
			continue
		}
		// Extract the path matcher to key the result.
		path := r.GetMatch().GetPrefix()
		if path == "" {
			path = r.GetMatch().GetPathSeparatedPrefix()
		}
		redirectByPath[path] = rd
	}

	// scheme-nil-and-port-nil on 8080 listener → port_redirect MUST be 8080.
	// This is the conformance-failing case before the fix.
	nilNil := redirectByPath["/scheme-nil-and-port-nil"]
	require.NotNil(t, nilNil, "scheme-nil-and-port-nil route must be present")
	assert.Equal(t, uint32(8080), nilNil.GetPortRedirect(),
		"scheme-nil-and-port-nil on 8080 listener must set port_redirect=8080 so Location contains :8080")
	assert.Empty(t, nilNil.GetSchemeRedirect(), "no scheme change")

	// scheme-nil-and-port-80 → explicit port=80 is preserved as-is.
	nilPort80 := redirectByPath["/scheme-nil-and-port-80"]
	require.NotNil(t, nilPort80, "scheme-nil-and-port-80 route must be present")
	assert.Equal(t, uint32(80), nilPort80.GetPortRedirect(),
		"explicit port=80 must be preserved")

	// scheme-https-and-port-nil → scheme change uses scheme default (443).
	httpsNil := redirectByPath["/scheme-https-and-port-nil"]
	require.NotNil(t, httpsNil, "scheme-https-and-port-nil route must be present")
	assert.Equal(t, uint32(443), httpsNil.GetPortRedirect(),
		"https scheme-only redirect must use port 443, not the listener port 8080")
	assert.Equal(t, "https", httpsNil.GetSchemeRedirect())
}

// TestGatewayExternalHTTPPort verifies gatewayExternalHTTPPort returns the correct
// port for various listener configurations.
func TestGatewayExternalHTTPPort(t *testing.T) {
	tests := []struct {
		name      string
		listeners []EdgeGatewayListenerEntry
		want      uint32
	}{
		{
			name:      "single HTTP listener on 8080",
			listeners: []EdgeGatewayListenerEntry{{ExternalPort: 8080, InternalPort: 18101}},
			want:      8080,
		},
		{
			name:      "default HTTP listener on 80",
			listeners: []EdgeGatewayListenerEntry{{ExternalPort: 80, InternalPort: 18100}},
			want:      80,
		},
		{
			name: "TLS listener only — no HTTP port",
			listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 443, InternalPort: 18102, TLSSecretNames: []string{"my/cert"}},
			},
			want: 0,
		},
		{
			name: "redirect listener only — no routable port",
			listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 80, InternalPort: 18100, HTTPRedirect: true},
			},
			want: 0,
		},
		{
			name: "redirect + TLS — no plain HTTP port",
			listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 80, InternalPort: 18100, HTTPRedirect: true},
				{ExternalPort: 443, InternalPort: 18101, TLSSecretNames: []string{"my/cert"}},
			},
			want: 0,
		},
		{
			name: "HTTP + TLS mixed — returns HTTP port",
			listeners: []EdgeGatewayListenerEntry{
				{ExternalPort: 8080, InternalPort: 18100},
				{ExternalPort: 8443, InternalPort: 18101, TLSSecretNames: []string{"my/cert"}},
			},
			want: 8080,
		},
		{
			name:      "empty listeners",
			listeners: nil,
			want:      0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gatewayExternalHTTPPort(tc.listeners)
			assert.Equal(t, tc.want, got)
		})
	}
}
