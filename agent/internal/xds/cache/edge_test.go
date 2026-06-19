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
// listener on the https port (referencing the route's SDS cert) plus an
// HTTP->HTTPS redirect on the plain port; the certs ride the SecretType channel.
func TestEdgeTLSModeListeners(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	c.SetEdgeTLSMode(443)
	c.SetEdgeRoutes([]EdgeRoute{
		{Hosts: []string{"api.example.com"}, Service: "svc-1", TLSSecret: "kubernetes/api-tls"},
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

func TestSetEdgeRoutesDependencySet(t *testing.T) {
	c := newTestCache("edge-1")

	c.SetEdgeRoutes([]EdgeRoute{
		{Hosts: []string{"api.example.com"}, Service: "svc-1"},
		{Hosts: []string{"grpc.example.com"}, Service: "svc-2", Port: 9090},
		{Service: "svc-3"}, // no hosts -> inert, never pulled into scope
		{Service: ""},      // inert, ignored
	})

	deps := c.DependencySet()
	assert.Len(t, deps, 2)
	assert.Contains(t, deps, "svc-1")
	assert.Contains(t, deps, "svc-2")
	assert.NotContains(t, deps, "svc-3", "a hostless route exposes nothing and must not scope its service")
}

// TestEdgeRouteVhosts checks host->cluster resolution: external hosts map to the
// default cluster and a non-default port targets the per-port cluster. A route
// without hosts is NOT routable — the mesh FQDN is never an edge entrypoint.
func TestEdgeRouteVhosts(t *testing.T) {
	c := newTestCache("edge-1")

	// A per-port cluster must exist for the explicit-port route to resolve to it.
	c.clusterMu.Lock()
	c.clusters["svc-2.aether.internal:9090"] = clusterEntry{service: "svc-2", sni: "9090"}
	c.clusterMu.Unlock()

	c.SetEdgeRoutes([]EdgeRoute{
		{Hosts: []string{"api.example.com", "api2.example.com"}, Service: "svc-1"},
		{Service: "svc-3"}, // no hosts -> NOT routable (no vhost)
		{Hosts: []string{"grpc.example.com"}, Service: "svc-2", Port: 9090},
	})

	vhosts := c.edgeRouteVhosts()
	require.Len(t, vhosts, 2)

	assert.Equal(t, "svc-1.aether.internal", vhosts[0].GetName())
	assert.Equal(t, []string{"api.example.com", "api2.example.com"}, vhosts[0].GetDomains())

	assert.Equal(t, "svc-2.aether.internal:9090", vhosts[1].GetName())
	assert.Equal(t, []string{"grpc.example.com"}, vhosts[1].GetDomains())

	// No vhost exposes a mesh FQDN as a routable domain.
	for _, vh := range vhosts {
		for _, d := range vh.GetDomains() {
			assert.NotContains(t, d, ".aether.internal", "the mesh FQDN must not be routable from the edge")
		}
	}
}

// TestEdgeRouteVhostsMergeByCluster verifies multiple routes to the same service
// collapse into one vhost with the union of hostnames (Envoy NACKs duplicate
// vhost names/domains).
func TestEdgeRouteVhostsMergeByCluster(t *testing.T) {
	c := newTestCache("edge-1")
	c.SetEdgeRoutes([]EdgeRoute{
		{Hosts: []string{"a.example.com"}, Service: "svc-1"},
		{Hosts: []string{"b.example.com", "a.example.com"}, Service: "svc-1"},
	})

	vhosts := c.edgeRouteVhosts()
	require.Len(t, vhosts, 1)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, vhosts[0].GetDomains())
}

func TestPerDownstreamConnectionPool(t *testing.T) {
	node := newTestCache("node-1")
	assert.True(t, node.perDownstreamConnectionPool(), "node proxy pools per downstream")

	edge := newTestCache("edge-1")
	edge.SetEdgeMode(8080)
	assert.False(t, edge.perDownstreamConnectionPool(), "edge multiplexes on its single identity")
}
