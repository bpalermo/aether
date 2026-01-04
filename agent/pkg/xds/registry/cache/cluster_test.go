package cache

import (
	"fmt"
	"sync"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
)

func TestClusterCache(t *testing.T) {
	t.Run("NewClusterCache", func(t *testing.T) {
		cache := NewClusterCache()
		assert.NotNil(t, cache)
		assert.NotNil(t, cache.clusters)
		assert.Empty(t, cache.clusters)
	})

	t.Run("AddCluster", func(t *testing.T) {
		cache := NewClusterCache()
		pod := &registryv1.Event_KubernetesPod{
			Name:        "test-pod",
			Namespace:   "default",
			ServiceName: "test-cluster",
			Ip:          "10.0.0.1",
		}

		cache.AddClusterOrUpdate(pod)

		cache.mu.RLock()
		stored, exists := cache.clusters["test-cluster"]
		cache.mu.RUnlock()

		assert.True(t, exists)
		assert.NotNil(t, stored)
		assert.Equal(t, "test-cluster", stored.Name)
	})

	t.Run("RemoveCluster", func(t *testing.T) {
		cache := NewClusterCache()
		pod := &registryv1.Event_KubernetesPod{
			Name:        "test-pod",
			Namespace:   "default",
			ServiceName: "test-cluster",
			Ip:          "10.0.0.1",
		}

		cache.AddClusterOrUpdate(pod)
		cache.RemoveCluster("test-cluster")

		cache.mu.RLock()
		_, exists := cache.clusters["test-cluster"]
		cache.mu.RUnlock()

		assert.False(t, exists)
	})

	t.Run("GetAllClusters", func(t *testing.T) {
		cache := NewClusterCache()

		// Add multiple clusters
		for i := 0; i < 3; i++ {
			pod := &registryv1.Event_KubernetesPod{
				Name:        fmt.Sprintf("pod-%d", i),
				Namespace:   "default",
				ServiceName: fmt.Sprintf("cluster-%d", i),
				Ip:          fmt.Sprintf("10.0.0.%d", i+1),
			}
			cache.AddClusterOrUpdate(pod)
		}

		clusters := cache.GetAllClusters()
		assert.Len(t, clusters, 3)
	})

	t.Run("UpdateExistingCluster", func(t *testing.T) {
		cache := NewClusterCache()

		// Add initial pod
		pod1 := &registryv1.Event_KubernetesPod{
			Name:        "pod-1",
			Namespace:   "default",
			ServiceName: "test-cluster",
			Ip:          "10.0.0.1",
		}
		cache.AddClusterOrUpdate(pod1)

		// Update with different pod (same service)
		pod2 := &registryv1.Event_KubernetesPod{
			Name:        "pod-2",
			Namespace:   "default",
			ServiceName: "test-cluster",
			Ip:          "10.0.0.2",
		}
		cache.AddClusterOrUpdate(pod2)

		cache.mu.RLock()
		assert.Len(t, cache.clusters, 1)
		cache.mu.RUnlock()
	})

	t.Run("ConcurrentAccess", func(t *testing.T) {
		cache := NewClusterCache()
		var wg sync.WaitGroup
		iterations := 100

		// Add clusters concurrently
		wg.Add(iterations)
		for i := 0; i < iterations; i++ {
			go func(id int) {
				defer wg.Done()
				pod := &registryv1.Event_KubernetesPod{
					Name:        fmt.Sprintf("pod-%d", id),
					Namespace:   "default",
					ServiceName: fmt.Sprintf("cluster-%d", id),
					Ip:          fmt.Sprintf("10.0.0.%d", id%255+1),
				}
				cache.AddClusterOrUpdate(pod)
			}(i)
		}
		wg.Wait()

		// Verify all clusters were added
		cache.mu.RLock()
		assert.Len(t, cache.clusters, iterations)
		cache.mu.RUnlock()

		// Remove clusters concurrently
		wg.Add(iterations)
		for i := 0; i < iterations; i++ {
			go func(id int) {
				defer wg.Done()
				cache.RemoveCluster(fmt.Sprintf("cluster-%d", id))
			}(i)
		}
		wg.Wait()

		// Verify all clusters were removed
		cache.mu.RLock()
		assert.Empty(t, cache.clusters)
		cache.mu.RUnlock()
	})
}
