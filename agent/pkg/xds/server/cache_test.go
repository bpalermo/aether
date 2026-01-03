package server

import (
	"fmt"
	"sync"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestListenerCache(t *testing.T) {
	t.Run("NewListenerCache", func(t *testing.T) {
		cache := NewListenerCache()
		assert.NotNil(t, cache)
		assert.NotNil(t, cache.listeners)
		assert.Empty(t, cache.listeners)
	})

	t.Run("AddListeners", func(t *testing.T) {
		cache := NewListenerCache()
		path := "/test-listener"
		listener := &listenerv3.Listener{
			Name: "test-listener",
			Address: &corev3.Address{
				Address: &corev3.Address_SocketAddress{
					SocketAddress: &corev3.SocketAddress{
						Address: "0.0.0.0",
						PortSpecifier: &corev3.SocketAddress_PortValue{
							PortValue: 8080,
						},
					},
				},
			},
		}

		cache.AddListeners(path, []*listenerv3.Listener{listener})

		cache.mu.RLock()
		stored, exists := cache.listeners[path]
		cache.mu.RUnlock()

		assert.True(t, exists)
		assert.Len(t, stored.listeners, 1)
		assert.Equal(t, listener, stored.listeners["test-listener"])
	})

	t.Run("RemoveListeners", func(t *testing.T) {
		cache := NewListenerCache()
		path := "/test-listener"
		listener := &listenerv3.Listener{
			Name: "test-listener",
		}

		cache.AddListeners(path, []*listenerv3.Listener{listener})
		cache.RemoveListeners(path)

		cache.mu.RLock()
		_, exists := cache.listeners[path]
		cache.mu.RUnlock()

		assert.False(t, exists)
	})

	t.Run("ConcurrentListenerAccess", func(t *testing.T) {
		cache := NewListenerCache()
		var wg sync.WaitGroup
		iterations := 100

		// Add listeners concurrently
		wg.Add(iterations)
		for i := 0; i < iterations; i++ {
			go func(id int) {
				defer wg.Done()
				path := fmt.Sprintf("/listener-%d", id)
				listener := &listenerv3.Listener{
					Name: fmt.Sprintf("listener-%d", id),
				}
				cache.AddListeners(path, []*listenerv3.Listener{listener})
			}(i)
		}
		wg.Wait()

		// Verify all listeners were added
		cache.mu.RLock()
		assert.Len(t, cache.listeners, iterations)
		cache.mu.RUnlock()

		// Remove listeners concurrently
		wg.Add(iterations)
		for i := 0; i < iterations; i++ {
			go func(id int) {
				defer wg.Done()
				path := fmt.Sprintf("/listener-%d", id)
				cache.RemoveListeners(path)
			}(i)
		}
		wg.Wait()

		// Verify all listeners were removed
		cache.mu.RLock()
		assert.Empty(t, cache.listeners)
		cache.mu.RUnlock()
	})
}

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
