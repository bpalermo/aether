package cache

import (
	"fmt"
	"sync"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
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

		event := &registryv1.CNIPod{
			Name:             "test-pod",
			Namespace:        "default",
			NetworkNamespace: path,
		}

		cache.AddListeners(event)

		cache.mu.RLock()
		stored, exists := cache.listeners[path]
		cache.mu.RUnlock()

		assert.True(t, exists)
		assert.NotNil(t, stored)
		// Note: actual listeners depend on proxy.GenerateListenersFromEvent implementation
	})

	t.Run("RemoveListeners", func(t *testing.T) {
		cache := NewListenerCache()
		path := "/test-listener"

		event := &registryv1.CNIPod{
			Name:             "test-pod",
			Namespace:        "default",
			NetworkNamespace: path,
		}

		cache.AddListeners(event)
		cache.RemoveListeners(path)

		cache.mu.RLock()
		_, exists := cache.listeners[path]
		cache.mu.RUnlock()

		assert.False(t, exists)
	})

	t.Run("GetListeners", func(t *testing.T) {
		cache := NewListenerCache()
		path := "/test-listener"

		event := &registryv1.CNIPod{
			Name:             "test-pod",
			Namespace:        "default",
			NetworkNamespace: path,
		}

		cache.AddListeners(event)

		listeners := cache.GetListeners(path)
		assert.NotNil(t, listeners)
	})

	t.Run("GetAllListeners", func(t *testing.T) {
		cache := NewListenerCache()

		// Add multiple listeners
		for i := 0; i < 3; i++ {
			event := &registryv1.CNIPod{
				Name:             fmt.Sprintf("test-pod-%d", i),
				Namespace:        "default",
				NetworkNamespace: fmt.Sprintf("/listener-%d", i),
			}
			cache.AddListeners(event)
		}

		allListeners := cache.GetAllListeners()
		assert.NotNil(t, allListeners)
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
				event := &registryv1.CNIPod{
					Name:             "test-pod",
					Namespace:        "default",
					NetworkNamespace: path,
				}
				cache.AddListeners(event)
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
