package cache

import (
	"context"
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nodeIdentity = "spiffe://aether.internal/ns/aether-system/sa/aether-agent"

// TestServiceClusterMTLSInjected verifies that once a local pod and the node
// identity are known, each service cluster carries the per-source mTLS matcher
// (the originating pod's identity plus the node identity for the on-no-match
// fallback), injected at snapshot time.
func TestServiceClusterMTLSInjected(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	// A local pod establishes the netns -> SPIFFE ID mapping and trust domain.
	pod := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	// The node SVID being served gates the upstream mTLS injection.
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"echo": {makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	echo, ok := clusters["echo.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present")
	require.NotNil(t, echo.GetTransportSocketMatcher(), "service cluster must carry the per-source mTLS matcher")
	names := map[string]bool{}
	for _, m := range echo.GetTransportSocketMatches() {
		names[m.GetName()] = true
	}
	assert.True(t, names["spiffe://aether.internal/ns/aether-test/sa/echo"], "match for the local pod identity")
	assert.True(t, names[nodeIdentity], "match for the node identity (on-no-match fallback)")
}

// TestServiceClusterNoMTLSWithoutNodeIdentity verifies the upstream mTLS matcher is
// not injected until the node SVID is served (its on-no-match references the node
// identity).
func TestServiceClusterNoMTLSWithoutNodeIdentity(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "echo")
	ctx := context.Background()

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"echo": {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present")
	assert.Nil(t, echo.GetTransportSocketMatcher(), "no mTLS matcher before the node SVID is served")
}

// TestServiceClusterSANPinning verifies the snapshot-time injection pins the
// upstream peer identity to the service's expected SPIFFE IDs, derived from
// its endpoints' namespaces — on the per-source matches AND the no-match
// (node identity) path alike. This is the anti-registry-poisoning control: a
// valid-but-wrong SVID must fail the handshake.
func TestServiceClusterSANPinning(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	ep1 := makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080) // ns "default" (makeEndpoint)
	ep2 := makeEndpoint("10.0.0.10", "cluster-1", "node-2", 18080)
	ep2.KubernetesMetadata.Namespace = "aether-test"
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"echo": {ep1, ep2}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok)

	wantSANs := []string{
		"spiffe://aether.internal/ns/aether-test/sa/echo",
		"spiffe://aether.internal/ns/default/sa/echo",
	}
	require.NotEmpty(t, echo.GetTransportSocketMatches())
	for _, m := range echo.GetTransportSocketMatches() {
		utc := &tlsv3.UpstreamTlsContext{}
		require.NoError(t, m.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))
		combined := utc.GetCommonTlsContext().GetCombinedValidationContext()
		require.NotNil(t, combined, "every per-source socket pins the server identity (match %s)", m.GetName())
		var got []string
		for _, sm := range combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames() {
			got = append(got, sm.GetMatcher().GetExact())
		}
		assert.Equal(t, wantSANs, got, "sorted namespace union renders the expected identities")
	}
}
