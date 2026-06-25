package cache

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	ls := c.Listeners()
	require.Len(t, ls, 2)
	names := map[string]*listenerv3.Listener{}
	for _, r := range ls {
		l := r.(*listenerv3.Listener)
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

	listeners := c.Listeners()
	require.Len(t, listeners, 1)
	l, ok := listeners[0].(*listenerv3.Listener)
	require.True(t, ok)
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
		{Routes: []Route{{Prefix: "/", Service: "svc-3"}}}, // no hosts -> inert, never scoped
	})

	deps := c.DependencySet()
	assert.Len(t, deps, 2)
	assert.Contains(t, deps, "svc-1")
	assert.Contains(t, deps, "svc-2")
	assert.NotContains(t, deps, "svc-3", "a hostless virtual host exposes nothing and must not scope its service")
}

// TestVirtualHostVhosts checks host->cluster resolution: external hosts become
// the vhost domains and a non-default port targets the per-port cluster. A vhost
// without hosts is NOT routable — the mesh FQDN is never an edge entrypoint.
func TestVirtualHostVhosts(t *testing.T) {
	c := newTestCache("edge-1")

	// A per-port cluster must exist for the explicit-port backend to resolve to it.
	c.clusterMu.Lock()
	c.clusters["svc-2.aether.internal:9090"] = clusterEntry{service: "svc-2", sni: "9090"}
	c.clusterMu.Unlock()

	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"api.example.com", "api2.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-1"}}},
		{Routes: []Route{{Prefix: "/", Service: "svc-3"}}}, // no hosts -> NOT routable (no vhost)
		{Hosts: []string{"grpc.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-2", Port: 9090}}},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 2)

	assert.Equal(t, "api.example.com", vhosts[0].GetName())
	assert.Equal(t, []string{"api.example.com", "api2.example.com"}, vhosts[0].GetDomains())
	assert.Equal(t, "svc-1.aether.internal", vhosts[0].GetRoutes()[0].GetRoute().GetCluster())

	assert.Equal(t, "grpc.example.com", vhosts[1].GetName())
	assert.Equal(t, []string{"grpc.example.com"}, vhosts[1].GetDomains())
	assert.Equal(t, "svc-2.aether.internal:9090", vhosts[1].GetRoutes()[0].GetRoute().GetCluster())

	// No vhost exposes a mesh FQDN as a routable domain.
	for _, vh := range vhosts {
		for _, d := range vh.GetDomains() {
			assert.NotContains(t, d, ".aether.internal", "the mesh FQDN must not be routable from the edge")
		}
	}
}

// TestVirtualHostPathRoutes verifies one virtual host fans different paths to
// different services, preserving CR order (first match wins) and prefix vs exact.
func TestVirtualHostPathRoutes(t *testing.T) {
	c := newTestCache("edge-1")
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

	assert.Equal(t, "/users", routes[0].GetMatch().GetPrefix())
	assert.Equal(t, "svc-1.aether.internal", routes[0].GetRoute().GetCluster())
	assert.Equal(t, "/healthz", routes[1].GetMatch().GetPath())
	assert.Equal(t, "svc-2.aether.internal", routes[1].GetRoute().GetCluster())
	assert.Equal(t, "/", routes[2].GetMatch().GetPrefix())
	assert.Equal(t, "svc-3.aether.internal", routes[2].GetRoute().GetCluster())
}

// TestVirtualHostVhostsDedupDomains verifies a host claimed by an earlier vhost
// is dropped from later ones (Envoy NACKs duplicate domains) — the runtime
// backstop to the controller's duplicate-FQDN webhook, keep-first by input order.
func TestVirtualHostVhostsDedupDomains(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetVirtualHosts([]VirtualHost{
		{Hosts: []string{"a.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-1"}}},
		{Hosts: []string{"a.example.com", "b.example.com"}, Routes: []Route{{Prefix: "/", Service: "svc-2"}}},
	})

	vhosts := c.virtualHostVhosts()
	require.Len(t, vhosts, 2)
	assert.Equal(t, []string{"a.example.com"}, vhosts[0].GetDomains())
	assert.Equal(t, "svc-1.aether.internal", vhosts[0].GetRoutes()[0].GetRoute().GetCluster())
	assert.Equal(t, []string{"b.example.com"}, vhosts[1].GetDomains(), "the duplicate host is dropped from the later vhost")
	assert.Equal(t, "svc-2.aether.internal", vhosts[1].GetRoutes()[0].GetRoute().GetCluster())
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

		ls := c.Listeners()
		require.Len(t, ls, 1)
		l := ls[0].(*listenerv3.Listener)
		assert.Equal(t, proxy.EdgeListenerName, l.GetName(), "HTTP listener serves routes, not a redirect")
		assert.Equal(t, uint32(80), l.GetAddress().GetSocketAddress().GetPortValue())
	})

	t.Run("redirect when opt-in annotation set", func(t *testing.T) {
		c := newTestCache("edge-1")
		c.SetEdgeMode(80)
		c.SetEdgeTLSMode(443)
		c.SetEdgeHTTPRedirect(true)

		ls := c.Listeners()
		names := map[string]*listenerv3.Listener{}
		for _, r := range ls {
			l := r.(*listenerv3.Listener)
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

		ls := c.Listeners()
		require.Len(t, ls, 2, "HTTPS listener + HTTP routing listener")
		// The two listeners MUST have DISTINCT names (edge_https on 443, edge_http on
		// 80) — a shared name collides in the snapshot/LDS and drops :443 (regression).
		byName := map[string]uint32{}
		for _, r := range ls {
			l := r.(*listenerv3.Listener)
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

	ls := c.Listeners()
	names := map[string]bool{}
	for _, r := range ls {
		l := r.(*listenerv3.Listener)
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

	ls := c.Listeners()
	require.Len(t, ls, 1)
	l := ls[0].(*listenerv3.Listener)
	assert.Equal(t, proxy.EdgeListenerName, l.GetName(), "Phase 1 fallback: listener must be the shared edge_http")
	assert.Equal(t, uint32(80), l.GetAddress().GetSocketAddress().GetPortValue())
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

	ls := c.Listeners()
	require.Len(t, ls, 1)
	l := ls[0].(*listenerv3.Listener)
	assert.Equal(t, "edge_gw_ns-tls_secure-gw_18110", l.GetName())
	// TLS transport socket must be wired (not nil).
	require.NotNil(t, l.GetFilterChains()[0].GetTransportSocket(), "per-Gateway HTTPS listener must terminate TLS")
}
