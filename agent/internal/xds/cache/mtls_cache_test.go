package cache

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	testPodSpiffeID = "spiffe://aether.internal/ns/aether-test/sa/echo"
	testPodNetns    = "/var/run/netns/cni-a"
)

// echoTestPod returns the local pod used by the mTLS cache tests: it maps
// testPodNetns to testPodSpiffeID and puts "aether-test/echo" in the node
// dependency set.
func echoTestPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: testPodNetns,
	}
}

// echoRegistry returns a mock registry serving "aether-test/echo" with a
// single endpoint in the given namespace.
func echoRegistry(endpointNamespace *string) *mockRegistry {
	return &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			ep := makeEndpoint("10.0.0.9", "cluster-1", "node-2", 18080)
			ep.KubernetesMetadata.Namespace = *endpointNamespace
			return map[string][]*registryv1.ServiceEndpoint{"aether-test/echo": {ep}}, nil
		},
	}
}

// snapshotEchoCluster fetches the emitted echo service cluster from the
// current snapshot.
func snapshotEchoCluster(t *testing.T, c *SnapshotCache, nodeName string) *clusterv3.Cluster {
	t.Helper()
	snap, err := c.GetSnapshot(nodeName)
	require.NoError(t, err)
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo.aether-test.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present in the snapshot")
	return echo
}

// matchNames collects the names of a cluster's transport socket matches.
func matchNames(cl *clusterv3.Cluster) map[string]bool {
	names := map[string]bool{}
	for _, m := range cl.GetTransportSocketMatches() {
		names[m.GetName()] = true
	}
	return names
}

// pinnedSANs extracts the exact-match SAN URIs pinned on the first transport
// socket match's upstream validation context.
func pinnedSANs(t *testing.T, cl *clusterv3.Cluster) []string {
	t.Helper()
	require.NotEmpty(t, cl.GetTransportSocketMatches())
	utc := &tlsv3.UpstreamTlsContext{}
	require.NoError(t, cl.GetTransportSocketMatches()[0].GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))
	var got []string
	for _, sm := range utc.GetCommonTlsContext().GetCombinedValidationContext().GetDefaultValidationContext().GetMatchTypedSubjectAltNames() {
		got = append(got, sm.GetMatcher().GetExact())
	}
	return got
}

// TestCachedMTLSClusterMatchesInlineInjection is the byte-identity proof for
// issue #537: the cluster emitted from the precomputed entry cache must be
// proto-equal to what the previous snapshot path produced by cloning the base
// cluster and injecting the transport socket inline, for every composition
// mode (node, node+waypoint, edge).
func TestCachedMTLSClusterMatchesInlineInjection(t *testing.T) {
	ns := "default"
	sanURIs := []string{"spiffe://aether.internal/ns/default/sa/echo"}
	validationContextName := "spiffe://aether.internal"
	fqdn := "echo.aether-test.aether.internal"

	tests := []struct {
		name     string
		setup    func(t *testing.T, c *SnapshotCache)
		expected func() *clusterv3.Cluster
	}{
		{
			name: "node mode: per-source matcher",
			setup: func(t *testing.T, c *SnapshotCache) {
				require.NoError(t, c.AddPod(context.Background(), echoTestPod(), "aether.internal"))
				require.NoError(t, c.SetNodeIdentity(context.Background(), nodeIdentity))
			},
			expected: func() *clusterv3.Cluster {
				cl := proxy.NewServiceCluster(fqdn, "aether-test/echo", "aether-test/echo", proxy.SortSubsetKeys(nil), true)
				proxy.InjectUpstreamMTLS(cl,
					map[string]string{testPodNetns: testPodSpiffeID}, []string{testPodSpiffeID},
					nodeIdentity, validationContextName, sanURIs, "18080", "")
				return cl
			},
		},
		{
			name: "node mode with waypoint: structured-SNI socket variants",
			setup: func(t *testing.T, c *SnapshotCache) {
				c.SetWaypointConfig(true, 18009)
				require.NoError(t, c.AddPod(context.Background(), echoTestPod(), "aether.internal"))
				require.NoError(t, c.SetNodeIdentity(context.Background(), nodeIdentity))
			},
			expected: func() *clusterv3.Cluster {
				cl := proxy.NewServiceCluster(fqdn, "aether-test/echo", "aether-test/echo", proxy.SortSubsetKeys(nil), true)
				proxy.InjectUpstreamMTLS(cl,
					map[string]string{testPodNetns: testPodSpiffeID}, []string{testPodSpiffeID},
					nodeIdentity, validationContextName, sanURIs, "18080", "18080."+fqdn)
				return cl
			},
		},
		{
			name: "edge mode: single-identity transport socket",
			setup: func(t *testing.T, c *SnapshotCache) {
				c.SetEdgeMode(8080)
				c.SetEdgeIdentity(nodeIdentity, "aether.internal")
				c.SetStaticDependencies([]string{"aether-test/echo"})
			},
			expected: func() *clusterv3.Cluster {
				cl := proxy.NewServiceCluster(fqdn, "aether-test/echo", "aether-test/echo", proxy.SortSubsetKeys(nil), false)
				cl.TransportSocket = proxy.EdgeUpstreamTransportSocket(nodeIdentity, validationContextName, sanURIs, "18080")
				return cl
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setup(t, c)
			require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", echoRegistry(&ns)))

			got := snapshotEchoCluster(t, c, "node-1")
			want := tt.expected()
			assert.True(t, proto.Equal(want, got),
				"cached mTLS cluster must be proto-identical to the inline-injected cluster\nwant: %v\ngot:  %v", want, got)
		})
	}
}

// TestCachedMTLSClusterInvalidatedOnWorkloadChange verifies the netns→identity
// invalidation path: adding or removing a local pod AFTER the registry load
// must rebuild the cached injected cluster so the per-source matcher tracks
// the local workload set (a stale cache would silently downgrade the new
// pod's outbound mTLS to the node certificate).
func TestCachedMTLSClusterInvalidatedOnWorkloadChange(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"

	require.NoError(t, c.AddPod(ctx, echoTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", echoRegistry(&ns)))

	names := matchNames(snapshotEchoCluster(t, c, "node-1"))
	assert.True(t, names[testPodSpiffeID], "initial matcher carries the first pod's identity")
	otherID := "spiffe://aether.internal/ns/aether-test/sa/other"
	assert.False(t, names[otherID], "second pod not added yet")

	// A second pod lands AFTER the load: the cached cluster must be rebuilt.
	other := &cniv1.CNIPod{
		Name:             "other-1",
		Namespace:        "aether-test",
		ServiceAccount:   "other",
		NetworkNamespace: "/var/run/netns/cni-b",
	}
	require.NoError(t, c.AddPod(ctx, other, "aether.internal"))

	names = matchNames(snapshotEchoCluster(t, c, "node-1"))
	assert.True(t, names[otherID], "matcher must pick up a pod added after the registry load")
	assert.True(t, names[testPodSpiffeID], "first pod's identity is retained")

	// And removed again on pod deletion.
	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-b"))
	names = matchNames(snapshotEchoCluster(t, c, "node-1"))
	assert.False(t, names[otherID], "matcher must drop a removed pod's identity")
	assert.True(t, names[testPodSpiffeID], "first pod's identity is retained")
}

// TestCachedMTLSClusterInvalidatedOnNodeIdentity verifies the node-SVID
// invalidation path: a cluster loaded BEFORE the node SVID is served is
// emitted bare (cleartext), and SetNodeIdentity afterwards must rebuild the
// cache so the next snapshot carries the injected transport socket.
func TestCachedMTLSClusterInvalidatedOnNodeIdentity(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"

	declareDeps(c, "aether-test/echo")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", echoRegistry(&ns)))

	echo := snapshotEchoCluster(t, c, "node-1")
	assert.Nil(t, echo.GetTransportSocketMatcher(), "no matcher before the node SVID is served")
	assert.Nil(t, echo.GetTransportSocket(), "no transport socket before the node SVID is served")

	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	// No local workloads: the injection falls back to a single plain transport
	// socket presenting the node identity.
	echo = snapshotEchoCluster(t, c, "node-1")
	assert.NotNil(t, echo.GetTransportSocket(), "SetNodeIdentity after the load must rebuild the cached cluster")
}

// TestCachedMTLSClusterInvalidatedOnSANNamespaceChange verifies the
// sanNamespaces invalidation path: a registry reload whose endpoints moved to
// a different namespace must re-render the pinned server identities on the
// cached cluster (and never leave the stale SAN set in place).
func TestCachedMTLSClusterInvalidatedOnSANNamespaceChange(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"
	reg := echoRegistry(&ns)

	require.NoError(t, c.AddPod(ctx, echoTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	assert.Equal(t, []string{"spiffe://aether.internal/ns/default/sa/echo"},
		pinnedSANs(t, snapshotEchoCluster(t, c, "node-1")))

	// The service's endpoints move namespaces; the reload must re-render SANs.
	ns = "prod"
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	assert.Equal(t, []string{"spiffe://aether.internal/ns/prod/sa/echo"},
		pinnedSANs(t, snapshotEchoCluster(t, c, "node-1")))
}
