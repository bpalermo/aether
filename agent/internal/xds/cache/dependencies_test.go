package cache

import (
	"context"
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeDepPod builds a CNIPod with the given service account and declared
// upstreams annotation.
func makeDepPod(name, sa, netns, upstreams string) *cniv1.CNIPod {
	pod := &cniv1.CNIPod{
		Name:             name,
		Namespace:        "default",
		ServiceAccount:   sa,
		NetworkNamespace: netns,
	}
	if upstreams != "" {
		pod.Annotations = map[string]string{constants.AnnotationConfigUpstreams: upstreams}
	}
	return pod
}

// drainDepSignal consumes a pending dependency-change signal, returning
// whether one was pending.
func drainDepSignal(c *SnapshotCache) bool {
	select {
	case <-c.DependencyChanges():
		return true
	default:
		return false
	}
}

// TestDependencySet_UnionsPodsAndOwnServices verifies the node dependency set
// is the union of all local pods' declared upstreams plus their own services.
func TestDependencySet_UnionsPodsAndOwnServices(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "svc-x, svc-y"), "example.org"))
	require.NoError(t, c.AddPod(ctx, makeDepPod("b-1", "svc-b", "/proc/2/ns/net", "svc-y,svc-z"), "example.org"))

	deps := c.DependencySet()
	for _, want := range []string{"svc-a", "svc-b", "svc-x", "svc-y", "svc-z"} {
		assert.Contains(t, deps, want)
	}
	assert.Len(t, deps, 5)
}

// TestDependencySet_PodRemovalShrinks verifies a removed pod's exclusive
// dependencies leave the set while shared ones survive.
func TestDependencySet_PodRemovalShrinks(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "svc-x,svc-shared"), "example.org"))
	require.NoError(t, c.AddPod(ctx, makeDepPod("b-1", "svc-b", "/proc/2/ns/net", "svc-shared"), "example.org"))

	require.NoError(t, c.RemovePod(ctx, "/proc/1/ns/net"))

	deps := c.DependencySet()
	assert.NotContains(t, deps, "svc-a", "removed pod's own service leaves the set")
	assert.NotContains(t, deps, "svc-x", "removed pod's exclusive upstream leaves the set")
	assert.Contains(t, deps, "svc-shared", "upstream still declared by another pod survives")
	assert.Contains(t, deps, "svc-b")
}

// TestDependencyChanges_SignalsOnRealChangesOnly verifies the coalesced change
// signal fires when the effective set changes and stays quiet when it does not.
func TestDependencyChanges_SignalsOnRealChangesOnly(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "svc-x"), "example.org"))
	assert.True(t, drainDepSignal(c), "first pod must signal a dependency change")

	// A second replica of the same service with identical upstreams changes
	// nothing in the union: no signal (this is the roll steady-state).
	require.NoError(t, c.AddPod(ctx, makeDepPod("a-2", "svc-a", "/proc/2/ns/net", "svc-x"), "example.org"))
	assert.False(t, drainDepSignal(c), "identical replica must not signal")

	// Removing one of two identical replicas changes nothing either.
	require.NoError(t, c.RemovePod(ctx, "/proc/2/ns/net"))
	assert.False(t, drainDepSignal(c), "removing a duplicate replica must not signal")

	// Removing the last replica shrinks the set: signal.
	require.NoError(t, c.RemovePod(ctx, "/proc/1/ns/net"))
	assert.True(t, drainDepSignal(c), "removing the last contributor must signal")
}

// TestLoadClustersFromRegistry_ScopesToDependencySet is the headline Phase 1
// behavior: only services in the node dependency set are distributed; the
// rest of the registry stays out of the snapshot (clusters, endpoints, and
// vhosts alike).
func TestLoadClustersFromRegistry_ScopesToDependencySet(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	// Local pod: own service svc-self, declared upstream svc-dep.
	require.NoError(t, c.AddPod(ctx, makeDepPod("self-1", "svc-self", "/proc/1/ns/net", "svc-dep"), "example.org"))

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc-self":  {makeEndpoint("10.0.0.1", "cluster-1", "node-1", 8080)},
				"svc-dep":   {makeEndpoint("10.0.0.2", "cluster-1", "node-2", 8080)},
				"svc-other": {makeEndpoint("10.0.0.3", "cluster-1", "node-3", 8080)},
				"svc-more":  {makeEndpoint("10.0.0.4", "cluster-1", "node-4", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	assert.Contains(t, c.clusters, "svc-self", "a pod's own service is always in scope")
	assert.Contains(t, c.clusters, "svc-dep", "declared upstreams are in scope")
	assert.NotContains(t, c.clusters, "svc-other", "undeclared services are not distributed")
	assert.NotContains(t, c.clusters, "svc-more", "undeclared services are not distributed")
}

// TestLoadClustersFromRegistry_EmptyDependencySet verifies a node with no
// local pods distributes no service clusters at all.
func TestLoadClustersFromRegistry_EmptyDependencySet(t *testing.T) {
	c := newTestCache("node-1")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg))
	assert.Empty(t, c.clusters)
}
