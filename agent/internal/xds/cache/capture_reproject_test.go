package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// The chains issue #1094 is about: tcp-echo's declared primary port (9000) and
// a non-primary raw-TCP port (9001), both derived from the ENDPOINTS at
// registry load, and the portless any-port shim proposal 037 Phase 4 removes.
const (
	reprojectFloor    = "tcp:echo-tcp.aether-test.aether.internal"
	reprojectPrimary  = "cap_tcp_" + reprojectFloor + "_9000"
	reprojectNonPrime = "cap_tcp_" + reprojectFloor + "_9001"
	reprojectAnyPort  = "cap_tcp_anyport_" + reprojectFloor
)

// newReprojectCache is a capture-enabled cache with one local pod, a node
// identity, and a registry serving one TCP-primary service whose endpoints
// declare a second raw-TCP port.
func newReprojectCache(t *testing.T) (*SnapshotCache, *mockRegistry) {
	t.Helper()
	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name:             "client-0",
		Namespace:        "aether-test",
		ServiceAccount:   "client",
		NetworkNamespace: "/var/run/netns/cni-client",
	}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/echo-tcp")

	ep := makeEndpoint("10.0.0.9", "cluster-1", "node-2", 9000)
	ep.PortProtocols = map[uint32]registryv1.PortProtocol{
		9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
		9001: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
	}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{"aether-test/echo-tcp": {ep}}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	return c, reg
}

// projectEchoTCP is what the capture reconciler sends on EVERY reconcile: the
// mesh Service's name, VIP and primary-protocol class. It carries no ports --
// those are the cache's, derived from the endpoints.
//
// The projection only signals a rebuild; in the agent the next snapshot push
// (any of them -- on the reference cluster it was the one a registry load had queued) carries
// whatever listeners it left behind. The test pushes one immediately.
func projectEchoTCP(t *testing.T, c *SnapshotCache) {
	t.Helper()
	project(t, c, []capture.CaptureTCPService{
		{ServiceName: "aether-test/echo-tcp", ClusterIP: "10.96.11.228", PrimaryIsTCP: true},
	})
}

func project(t *testing.T, c *SnapshotCache, services []capture.CaptureTCPService) {
	t.Helper()
	c.SetCaptureTCPServices(services)
	require.NoError(t, c.generateSnapshot(context.Background()))
}

func assertDeclaredPortChains(t *testing.T, c *SnapshotCache, why string) {
	t.Helper()
	names := captureChainNames(t, c, "node-1")
	assert.Contains(t, names, reprojectPrimary, "declared primary port 9000 must keep its own chain: %s", why)
	assert.Contains(t, names, reprojectNonPrime, "declared non-primary port 9001 must keep its own chain: %s", why)
}

// captureListenerWire is the deterministic wire form of every listener in the
// node's current snapshot, so "nothing was rebuilt" is a byte comparison.
func captureListenerWire(t *testing.T, c *SnapshotCache) map[string][]byte {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	out := map[string][]byte{}
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		l, ok := res.(*listenerv3.Listener)
		require.True(t, ok)
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(l)
		require.NoError(t, err)
		out[name] = b
	}
	return out
}

// TestCaptureReprojectionKeepsDeclaredPortChains is issue #1094.
//
// On the reference cluster (2026-10-01, node C) a connection to tcp-echo's DECLARED
// port 9000 was served by the any-port shim 12 s after the agent restarted. The
// agent log gives the order: a registry load derived the port set
// (05:41:54.950), the capture reconciler re-projected the mesh Services
// (05:41:55.054), snapshot .11 was set (05:41:55.081) and served the connection
// (05:41:55.436), and only the NEXT registry load (05:41:55.979) put the chain
// back.
//
// SetCaptureTCPServices rebuilt its entries from the reconciler's input alone,
// dropping the endpoint-derived primaryPort/tcpPorts; equalTCPEntries then saw
// a change and rebuilt every capture listener without the declared-port chains.
// A restart made it dense -- one reconcile per mesh Service interleaved with
// registry loads -- but any mesh-Service event mid-life did the same, and each
// flap drained the connections riding the removed chains.
func TestCaptureReprojectionKeepsDeclaredPortChains(t *testing.T) {
	ctx := context.Background()

	t.Run("re-projection after a registry load (the reference-cluster order)", func(t *testing.T) {
		c, reg := newReprojectCache(t)
		projectEchoTCP(t, c)
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		assertDeclaredPortChains(t, c, "after the registry load derived them")

		before := captureListenerWire(t, c)
		projectEchoTCP(t, c) // an identical re-projection: any mesh-Service event
		assertDeclaredPortChains(t, c, "an unchanged mesh-Service re-projection must not drop them")
		assert.Equal(t, before, captureListenerWire(t, c),
			"an identical re-projection is not a change: rebuilding drains the listener's connections (Risk 4)")
	})

	t.Run("registry load before the first projection", func(t *testing.T) {
		c, reg := newReprojectCache(t)
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		projectEchoTCP(t, c)
		assertDeclaredPortChains(t, c, "a port set derived BEFORE the service list arrived must still apply")
	})

	t.Run("a new service joining keeps the existing ones' chains", func(t *testing.T) {
		c, reg := newReprojectCache(t)
		projectEchoTCP(t, c)
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		project(t, c, []capture.CaptureTCPService{
			{ServiceName: "aether-test/echo-tcp", ClusterIP: "10.96.11.228", PrimaryIsTCP: true},
			{ServiceName: "aether-test/other", ClusterIP: "10.96.11.229", PrimaryIsTCP: true},
		})
		assertDeclaredPortChains(t, c, "a real change to the service list must carry the derived ports over")
	})

	t.Run("the any-port shim is untouched", func(t *testing.T) {
		c, reg := newReprojectCache(t)
		projectEchoTCP(t, c)
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		projectEchoTCP(t, c)
		assert.Contains(t, captureChainNames(t, c, "node-1"), reprojectAnyPort,
			"removing the shim is proposal 037 Phase 4's call, not this fix's")
	})
}
