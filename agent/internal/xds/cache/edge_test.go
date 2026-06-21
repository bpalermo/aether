package cache

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEdgeTLSModeListeners verifies that with TLS enabled the cache serves a TLS
// listener on the https port (referencing the vhost's SDS cert) plus an
// HTTP->HTTPS redirect on the plain port; the certs ride the SecretType channel.
func TestEdgeTLSModeListeners(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	c.SetEdgeTLSMode(443)
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
	tls := names[proxy.EdgeListenerName]
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
