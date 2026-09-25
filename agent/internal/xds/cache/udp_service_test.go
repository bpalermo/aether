package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// udpOnlyRegistry serves one service under PROTOCOL_UDP and nothing under the
// other two.
//
// It switches on the protocol EXPLICITLY rather than treating "not UDP" as
// something else. The shared fixtures in this package predate a third protocol
// and use the "if TCP ... else HTTP" shape, which silently folds a new
// protocol's call into another branch -- the exact bug that made
// registrar/internal/server's syncer test fail when UDP was added.
func udpOnlyRegistry(serviceKey, ip string, port uint32) *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			switch protocol {
			case registryv1.Service_PROTOCOL_UDP:
				return map[string][]*registryv1.ServiceEndpoint{
					serviceKey: {makeEndpoint(ip, "cluster-1", "node-2", port)},
				}, nil
			default:
				return map[string][]*registryv1.ServiceEndpoint{}, nil
			}
		},
	}
}

// TestLoadClustersFromRegistry_UDPOnlyServiceGetsAnEntry is the agent-side half
// of #931.
//
// A UDP-only service could not previously exist: PROTOCOL_UDP was not in the
// enum, and LoadClustersFromRegistry listed only HTTP and TCP. Even once the
// enum existed, a service registered solely under it produced NO cluster entry
// -- and udpClusterForLocked refuses to name a cluster the snapshot does not
// carry, so captureUDPClusters emitted nothing and the per-pod udp_proxy
// listener was never built. The datagram path did not exist rather than being
// broken, and nothing said so.
func TestLoadClustersFromRegistry_UDPOnlyServiceGetsAnEntry(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "udponly-0",
		Namespace:        "aether-test",
		ServiceAccount:   "udponly",
		NetworkNamespace: "/var/run/netns/cni-udponly",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/udponly")

	reg := udpOnlyRegistry("aether-test/udponly", "10.0.0.40", 9001)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	cla, ok := snap.GetResources(resourcev3.EndpointType)["aether-test/udponly"].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok, "a UDP-only service must still publish its bare-name EDS resource")
	require.Len(t, cla.GetEndpoints(), 1)

	c.clusterMu.RLock()
	_, bareKeyed := c.clusters["aether-test/udponly"]
	udpEntry, hasUDP := c.clusters[proxy.UDPClusterName("aether-test/udponly", c.meshDomain)]
	c.clusterMu.RUnlock()

	assert.False(t, bareKeyed, "a UDP entry is keyed by its cluster name, like the TCP one")
	require.True(t, hasUDP, "a UDP-only service must get a udp: cluster entry")
	assert.True(t, udpEntry.l4Floor, "UDP entries are L4 floor entries: no h2 cluster, no vhost, no mTLS")
	assert.NotNil(t, udpEntry.loadAssignment, "with no HTTP entry the UDP entry owns the bare-name CLA")
	assert.Equal(t, "aether-test/udponly", udpEntry.loadAssignment.GetClusterName(),
		"the CLA is named for the SERVICE, not for the udp: cluster key")

	// sni carries the app port for the UDP floor: captureUDPClusters parses it
	// to rewrite the load assignment onto the datagram port. A UDP entry whose
	// sni does not parse is skipped, which is the silent-no-listener path.
	assert.Equal(t, "9001", udpEntry.sni, "sni carries the registered application port")

	// No peer identity is pinned: the UDP floor is plaintext, so claiming SANs
	// here would assert an authentication that does not happen.
	assert.Empty(t, udpEntry.sanNamespaces, "the UDP floor has no mTLS, so there is nothing to pin")
}

// TestUDPOnlyServiceResolvesThroughServiceEntry pins the lookup fallback.
// serviceEntryLocked tries HTTP, then TCP, then UDP; without the third arm a
// UDP-only service's facts were unreachable even once its entry existed.
func TestUDPOnlyServiceResolvesThroughServiceEntry(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "udponly-1",
		Namespace:        "aether-test",
		ServiceAccount:   "udponly2",
		NetworkNamespace: "/var/run/netns/cni-udponly2",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/udponly2")

	reg := udpOnlyRegistry("aether-test/udponly2", "10.0.0.41", 5353)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	c.clusterMu.RLock()
	entry, ok := c.serviceEntryLocked("aether-test/udponly2")
	udpCluster := c.udpClusterForLocked("aether-test/udponly2")
	c.clusterMu.RUnlock()

	require.True(t, ok, "serviceEntryLocked must fall through to the UDP entry")
	assert.Equal(t, "5353", entry.sni)
	assert.Equal(t, proxy.UDPClusterName("aether-test/udponly2", c.meshDomain), udpCluster,
		"udpClusterForLocked must now name a cluster for a UDP-only service")
}
