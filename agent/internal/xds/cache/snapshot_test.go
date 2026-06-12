package cache

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"

	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateSnapshot_NoClobberAcrossTypes verifies that the node snapshot
// holds listeners, clusters, routes and secrets simultaneously. Each mutation
// sets a complete snapshot, so a later listener/cluster/secret update must not
// drop the other resource types (the clobber that left Envoy with only the
// last-written type).
func TestGenerateSnapshot_NoClobberAcrossTypes(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	ctx := context.Background()

	// Listeners from storage.
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{makeCNIPod("pod-a", "default", "/proc/100/ns/net")}, nil
	})
	require.NoError(t, c.LoadListenersFromStorage(ctx, store, "example.org"))

	// Clusters/routes from the registry ("echo" must be in the node
	// dependency set to be distributed).
	declareDeps(c, "echo")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"echo": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	// Secrets from SPIRE. This is the most recent write; before the unified
	// snapshot it would have clobbered listeners/clusters/routes.
	require.NoError(t, c.SetSecrets(ctx, []*tlsv3.Secret{{Name: "spiffe://example.org"}}))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	assert.NotEmpty(t, snap.GetResources(resourcev3.ListenerType), "listeners must survive later updates")
	assert.NotEmpty(t, snap.GetResources(resourcev3.ClusterType), "clusters must survive later updates")
	assert.NotEmpty(t, snap.GetResources(resourcev3.RouteType), "outbound route config must survive later updates")
	assert.NotEmpty(t, snap.GetResources(resourcev3.SecretType), "secrets must be present")
	assert.Contains(t, snap.GetResources(resourcev3.ExtensionConfigType), proxy.SubsetHeadersFilterName,
		"the shared subset-headers ECDS resource is always emitted")
}
