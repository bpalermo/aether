package cache

import (
	"context"
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOutboundClusterMTLSMatcher verifies that once a local pod is known, regular
// service clusters in the snapshot carry the upstream mTLS transport-socket
// matcher/matches keyed on the pod's identity, while same-node ("local_") clusters
// do not.
func TestOutboundClusterMTLSMatcher(t *testing.T) {
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

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				// Local endpoint (same cluster+node) so a "local_echo" variant is
				// also produced and can be checked for exclusion.
				"echo": {makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	echo, ok := clusters["echo"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present")
	require.NotNil(t, echo.GetTransportSocketMatcher(), "regular cluster must carry the mTLS matcher")
	require.Len(t, echo.GetTransportSocketMatches(), 1)
	assert.Equal(t, "spiffe://aether.internal/ns/aether-test/sa/echo", echo.GetTransportSocketMatches()[0].GetName())

	if local, ok := clusters["local_echo"].(*clusterv3.Cluster); ok {
		assert.Nil(t, local.GetTransportSocketMatcher(), "local_ cluster must not carry the matcher")
	}
}

// TestOutboundClusterNoMatcherWithoutLocalPods verifies clusters stay plaintext
// (no transport socket matcher) until a local pod is onboarded.
func TestOutboundClusterNoMatcherWithoutLocalPods(t *testing.T) {
	c := newTestCache("node-1")
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
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo"].(*clusterv3.Cluster)
	require.True(t, ok)
	assert.Nil(t, echo.GetTransportSocketMatcher())
	assert.Empty(t, echo.GetTransportSocketMatches())
}
