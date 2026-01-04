package cache

import (
	"fmt"
	"sync"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
)

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
