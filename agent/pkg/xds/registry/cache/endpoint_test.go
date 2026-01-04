package cache

import (
	"fmt"
	"sync"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndpointCache(t *testing.T) {
	t.Run("NewEndpointCache", func(t *testing.T) {
		cache := NewEndpointCache()
		assert.NotNil(t, cache)
		assert.NotNil(t, cache.clusters)
		assert.Empty(t, cache.clusters)
	})

	t.Run("AddEndpoint", func(t *testing.T) {
		cache := NewEndpointCache()
		clusterName := ClusterName("test-cluster")
		pod := &registryv1.Event_KubernetesPod{
			Name:      "test-pod",
			Namespace: "default",
			Ip:        "127.0.0.1",
		}

		cache.AddEndpoint(clusterName, pod)

		cache.mu.RLock()
		stored, exists := cache.clusters[clusterName]
		cache.mu.RUnlock()

		assert.True(t, exists)
		assert.Len(t, stored.endpoints, 1)
		podName := NamespacedPodName("default/test-pod")
		assert.Contains(t, stored.endpoints, podName)
	})

	t.Run("RemoveEndpoint", func(t *testing.T) {
		cache := NewEndpointCache()
		clusterName := ClusterName("test-cluster")
		pod := &registryv1.Event_KubernetesPod{
			Name:      "test-pod",
			Namespace: "default",
			Ip:        "127.0.0.1",
		}

		cache.AddEndpoint(clusterName, pod)
		cache.RemoveEndpoint(pod) // Pass the pod object, not podName

		cache.mu.RLock()
		_, exists := cache.clusters[clusterName]
		cache.mu.RUnlock()

		assert.False(t, exists) // Cluster removed when empty
	})

	t.Run("UpdateEndpoint", func(t *testing.T) {
		cache := NewEndpointCache()
		clusterName := ClusterName("test-cluster")

		// Add an initial endpoint
		pod1 := &registryv1.Event_KubernetesPod{
			Name:      "test-pod",
			Namespace: "default",
			Ip:        "127.0.0.1",
		}
		cache.AddEndpoint(clusterName, pod1)

		// Add another endpoint to the same cluster
		pod2 := &registryv1.Event_KubernetesPod{
			Name:      "test-pod-2",
			Namespace: "default",
			Ip:        "127.0.0.2",
		}
		cache.AddEndpoint(clusterName, pod2)

		cache.mu.RLock()
		stored, exists := cache.clusters[clusterName]
		cache.mu.RUnlock()

		require.True(t, exists)
		assert.Len(t, stored.endpoints, 2)
	})

	t.Run("ConcurrentAccess", func(t *testing.T) {
		cache := NewEndpointCache()
		var wg sync.WaitGroup
		iterations := 100

		// Store pods for removal
		pods := make([]*registryv1.Event_KubernetesPod, iterations)

		// Add endpoints concurrently
		wg.Add(iterations)
		for i := 0; i < iterations; i++ {
			go func(id int) {
				defer wg.Done()
				clusterName := ClusterName(fmt.Sprintf("cluster-%d", id))
				pod := &registryv1.Event_KubernetesPod{
					Name:      fmt.Sprintf("pod-%d", id),
					Namespace: "default",
					Ip:        fmt.Sprintf("10.0.0.%d", id),
				}
				pods[id] = pod
				cache.AddEndpoint(clusterName, pod)
			}(i)
		}
		wg.Wait()

		// Verify all endpoints were added
		cache.mu.RLock()
		assert.Len(t, cache.clusters, iterations)
		cache.mu.RUnlock()

		// Remove endpoints concurrently
		wg.Add(iterations)
		for i := 0; i < iterations; i++ {
			go func(id int) {
				defer wg.Done()
				cache.RemoveEndpoint(pods[id]) // Pass the pod object
			}(i)
		}
		wg.Wait()

		// Verify all endpoints were removed
		cache.mu.RLock()
		assert.Empty(t, cache.clusters)
		cache.mu.RUnlock()
	})
}
