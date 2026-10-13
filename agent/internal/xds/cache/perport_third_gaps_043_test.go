package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPerPort_DefaultClusterHoldsAPodThatNamesThePortInPortProtocolsOnly: a
// pod serves a port when its row lists it OR names it in port_protocols. Two
// pods of one service: one declares 8080 (its endpoint.aether.io/port), the
// other declares 9090 and serves 8080 too, which its row says in
// port_protocols alone. Both are members of the cluster of port 8080. A
// membership test that read the listed ports only left the second pod out of
// the default cluster, and its capacity with it.
func TestPerPort_DefaultClusterHoldsAPodThatNamesThePortInPortProtocolsOnly(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	a := makeEndpoint("10.0.6.1", "cluster-1", "node-2", 8080)
	a.PortProtocols = map[uint32]registryv1.PortProtocol{8080: h}
	b := makeEndpoint("10.0.6.2", "cluster-1", "node-3", 9090)
	b.PortProtocols = map[uint32]registryv1.PortProtocol{9090: h, 8080: h}
	c2 := makeEndpoint("10.0.6.3", "cluster-1", "node-3", 8080)
	c2.PortProtocols = map[uint32]registryv1.PortProtocol{8080: h}

	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	declareDeps(c, "ns/svc")
	reg := &mockRegistry{listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		return map[string][]*registryv1.ServiceEndpoint{"ns/svc": {a, b, c2}}, nil
	}}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	require.Equal(t, uint32(8080), c.servicePorts["ns/svc"].legacyPort, "fixture: two of three pods declare 8080")
	def, ok := c.clusters["ns/svc"]
	require.True(t, ok, "the default cluster")
	assert.Contains(t, def.endpoints, "10.0.6.1")
	assert.Contains(t, def.endpoints, "10.0.6.3")
	assert.Contains(t, def.endpoints, "10.0.6.2", "the pod that names 8080 in port_protocols alone serves it, and is not in its cluster")
}

// TestPerPort_EntryVirtualHostsAreRebuiltOncePerRuleGeneration: the entries'
// loopback virtual hosts are rebuilt when a rule input changed, and NOT on
// every snapshot build after it. The generation they were built at is
// recorded; a build that forgot to record it rebuilt every virtual host of the
// node on every snapshot for good (and re-marshalled and re-hashed each).
func TestPerPort_EntryVirtualHostsAreRebuiltOncePerRuleGeneration(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	vhostOf := func() any {
		c.clusterMu.RLock()
		defer c.clusterMu.RUnlock()
		e, ok := c.clusters["demo/multi"]
		require.True(t, ok)
		require.NotNil(t, e.vhost)
		return e.vhost
	}
	require.NoError(t, c.generateSnapshot(ctx))
	before := vhostOf()

	// A rule event: the next build rebuilds the virtual host, once.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{Matches: []proxy.GammaMatch{{Prefix: "/x"}}, HeaderMutation: rtpMark("x")}}})
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt := vhostOf()
	require.NotSame(t, before, rebuilt, "a rule event must rebuild the entry's virtual host")

	// No event: the builds that follow keep the very object.
	for range 3 {
		require.NoError(t, c.generateSnapshot(ctx))
		assert.Same(t, rebuilt, vhostOf(), "a snapshot build with no rule event rebuilt the entry's virtual host")
	}
}

// TestDeclaredServicePorts_OneClassPerNumber: what the cache keeps of a
// generated Service's ports, whatever it is handed. The reconciler already
// sorts this out; the cache does not depend on it (the sink is an interface,
// and a number in two classes is one authority in two virtual hosts).
func TestDeclaredServicePorts_OneClassPerNumber(t *testing.T) {
	got := declaredServicePorts([]uint32{9090, 8080, 8080, 18081, 0}, []uint32{8080, 9000, 18082, 5353, 9000})
	assert.Equal(t, []uint32{8080, 9090}, got.http, "sorted, once each, no mesh port")
	assert.Equal(t, []uint32{5353, 9000}, got.tcp, "8080 is HTTP and nothing else")
	assert.Empty(t, got.udp)
	assert.False(t, declaredServicePorts(nil, nil).known())
}

// TestPerPort_BackendRefToAColdBackendAfterARuleEventAlone: a rule's backendRef
// to a service this node holds no endpoint of is judged by that service's
// DECLARED ports, on the loopback table as on the capture tables, also when
// the entries' virtual hosts are rebuilt by a rule event and not by a registry
// refresh. The rebuild read the registry's ports only: the route to a port the
// cold backend does not declare was parked on the loopback table (503 at the
// on-demand timeout) where the capture table of the same snapshot said 500.
func TestPerPort_BackendRefToAColdBackendAfterARuleEventAlone(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	front := a043Service{name: "front", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	back := a043Service{name: "back", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{front, back}) // both Services declare 8080
	// The node holds front and not back.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{front})))
	require.NoError(t, c.generateSnapshot(ctx))

	// A rule event alone: front's route now names a port back does not declare.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: 7777, Weight: 1}},
	}}})
	require.NoError(t, c.generateSnapshot(ctx))
	tables := a043Tables(t, c)
	const host = "front.demo." + a043Mesh + ":8080"
	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081", "cap_http_8080"} {
		got := a043Resolve(t, tables[tname], host)
		assert.Equalf(t, uint32(500), got.status, "table %s: a backendRef to a port the cold backend does not declare: %+v", tname, got)
		assert.Emptyf(t, got.cluster, "table %s: routed to %s", tname, got.cluster)
	}
}
