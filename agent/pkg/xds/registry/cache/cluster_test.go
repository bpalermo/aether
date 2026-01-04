package cache

import (
	"fmt"
	"sync"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
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
		cluster := &clusterv3.Cluster{
			Name: "test-cluster",
		}

		cache.AddClusterOrUpdate(cluster)

		cache.mu.RLock()
		stored, exists := cache.clusters["test-cluster"]
		cache.mu.RUnlock()

		assert.True(t, exists)
		assert.Equal(t, cluster, stored)
	})

	t.Run("RemoveCluster", func(t *testing.T) {
		cache := NewClusterCache()
		cluster := &clusterv3.Cluster{
			Name: "test-cluster",
		}

		cache.AddClusterOrUpdate(cluster)
		cache.RemoveCluster("test-cluster")

		cache.mu.RLock()
		_, exists := cache.clusters["test-cluster"]
		cache.mu.RUnlock()

		assert.False(t, exists)
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
				cluster := &clusterv3.Cluster{
					Name: fmt.Sprintf("cluster-%d", id),
				}
				cache.AddClusterOrUpdate(cluster)
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
