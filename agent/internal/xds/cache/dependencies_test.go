package cache

import (
	"context"
	"testing"
	"time"

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

// TestObserveDependency_ColdPath verifies the ODCDS cold path: observing an
// out-of-scope service adds it to the dependency set (returning true exactly
// once), signals a dependency change, and a scoped reload then distributes
// the service; expiry via PruneObservedDependencies removes it again.
func TestObserveDependency_ColdPath(t *testing.T) {
	c := newTestCache("node-1")
	c.observedTTL = 50 * time.Millisecond
	c.serviceRetentionGrace = time.Nanosecond // prune immediately on reload
	ctx := context.Background()

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc-cold": {makeEndpoint("10.0.0.5", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}

	// Not distributed: not in the dependency set.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NotContains(t, c.clusters, "svc-cold")
	drainDepSignal(c)

	// First observation: a miss — added + signaled.
	assert.True(t, c.ObserveDependency(ctx, "svc-cold"), "first observation is a miss")
	assert.True(t, drainDepSignal(c), "observation must signal a dependency change")
	assert.Contains(t, c.DependencySet(), "svc-cold")

	// Re-observation refreshes the TTL without a second miss.
	assert.False(t, c.ObserveDependency(ctx, "svc-cold"), "known dependency is not a miss")

	// The scoped reload now distributes the cluster.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.Contains(t, c.clusters, "svc-cold")

	// Idle past the TTL: pruned from the set and signaled.
	time.Sleep(60 * time.Millisecond)
	drainDepSignal(c)
	c.PruneObservedDependencies()
	assert.True(t, drainDepSignal(c), "expiry must signal a dependency change")
	assert.NotContains(t, c.DependencySet(), "svc-cold")
}

// TestObserveDependency_EmptyName verifies empty names are rejected.
func TestObserveDependency_EmptyName(t *testing.T) {
	c := newTestCache("node-1")
	assert.False(t, c.ObserveDependency(context.Background(), ""))
	assert.False(t, drainDepSignal(c))
}

// TestLoadClustersFromRegistry_SubsetVocabulary verifies provider-defined
// endpoint metadata keys become the service's subset selectors and the
// node-wide ECDS subset-headers mapping, with invalid/reserved keys dropped.
func TestLoadClustersFromRegistry_SubsetVocabulary(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "svc-a")
	ctx := context.Background()

	ep := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
	ep.Metadata = map[string]string{
		"version": "v2",
		"shard":   "s1",
		"Bad_Key": "x", // invalid shape: dropped
		"ip":      "y", // reserved: dropped
	}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	// Cluster selectors: ip, pod + derived power set (sorted).
	entry := c.clusters["svc-a"]
	sel := entry.cluster.GetLbSubsetConfig().GetSubsetSelectors()
	require.Len(t, sel, 5)
	assert.Equal(t, []string{"shard"}, sel[2].GetKeys())
	assert.Equal(t, []string{"version"}, sel[3].GetKeys())
	assert.Equal(t, []string{"shard", "version"}, sel[4].GetKeys())

	// Node union published for the shared ECDS mapping.
	c.subsetMu.RLock()
	keys := c.subsetHeaderKeys
	c.subsetMu.RUnlock()
	assert.Equal(t, []string{"shard", "version"}, keys)
}

// TestSetNodeLocality_SignalsAndPrioritizes verifies resolving the node
// locality signals a reload and the rebuilt EDS carries failover priorities.
func TestSetNodeLocality_SignalsAndPrioritizes(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "svc-a")
	ctx := context.Background()

	sameZone := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
	sameZone.Locality = &registryv1.ServiceEndpoint_Locality{Region: "r1", Zone: "z1"}
	otherRegion := makeEndpoint("10.0.0.2", "cluster-1", "node-9", 8080)
	otherRegion.Locality = &registryv1.ServiceEndpoint_Locality{Region: "r2", Zone: "z1"}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"svc-a": {sameZone, otherRegion}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	drainDepSignal(c)

	// No locality yet: no preference.
	for _, lle := range c.clusters["svc-a"].loadAssignment.GetEndpoints() {
		assert.Zero(t, lle.GetPriority())
	}

	c.SetNodeLocality("r1", "z1")
	assert.True(t, drainDepSignal(c), "locality resolution must trigger a scoped reload")
	assert.False(t, func() bool { c.SetNodeLocality("r1", "z1"); return drainDepSignal(c) }(), "unchanged locality must not re-signal")

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	prios := map[string]uint32{}
	for _, lle := range c.clusters["svc-a"].loadAssignment.GetEndpoints() {
		prios[lle.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress().GetAddress()] = lle.GetPriority()
	}
	assert.Equal(t, uint32(0), prios["10.0.0.1"], "same zone is P0")
	assert.Equal(t, uint32(2), prios["10.0.0.2"], "other region is P2")
}

// TestSignalIfRetentionExpired covers the steady-state retention bug
// (2026-06-12): a service leaving the dependency set is retained empty for
// the grace, but under scoped watches no further events arrive to trigger
// the pruning reload — its stale vhost then shadows the ODCDS catch-all
// forever. Time-driven expiry must signal the reload instead.
func TestSignalIfRetentionExpired(t *testing.T) {
	c := newTestCache("node-1")
	c.serviceRetentionGrace = 30 * time.Millisecond
	declareDeps(c, "svc-gone")
	ctx := context.Background()

	data := map[string][]*registryv1.ServiceEndpoint{
		"svc-gone": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
	}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return data, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	// The service vanishes (and leaves the dep set); one reload retains it.
	data = map[string][]*registryv1.ServiceEndpoint{}
	declareDeps(c) // no upstreams anymore
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.Contains(t, c.clusters, "svc-gone", "retained during grace")
	drainDepSignal(c)

	// Within the grace: no signal.
	c.SignalIfRetentionExpired()
	assert.False(t, drainDepSignal(c), "no signal while inside the grace")

	// Past the grace: the tick signals, and the triggered reload prunes.
	time.Sleep(40 * time.Millisecond)
	c.SignalIfRetentionExpired()
	require.True(t, drainDepSignal(c), "expiry must signal the pruning reload")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.NotContains(t, c.clusters, "svc-gone", "stale retained service pruned")

	// Idempotent: nothing retained, no signal.
	c.SignalIfRetentionExpired()
	assert.False(t, drainDepSignal(c))
}
