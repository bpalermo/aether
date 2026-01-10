package cache

import (
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/proto"
)

// ListenerCache is an optimized cache for listeners
type ListenerCache struct {
	mu sync.RWMutex
	// Listeners indexed by path for O(1) lookups
	listeners map[string]*pathListeners
	// Track total listeners for monitoring
	totalListeners int
}

// pathListeners represents listeners for a specific path
type pathListeners struct {
	// Pre-converted listener resources
	resources []types.Resource
	// Track if resources need rebuild
	dirty bool
	// Raw listeners for efficient updates
	listeners map[string]*listenerv3.Listener
}

// NewListenerCache creates a new listener cache with initial capacity
func NewListenerCache() *ListenerCache {
	return &ListenerCache{
		listeners: make(map[string]*pathListeners, 16), // Pre-allocate for typical path count
	}
}

// AddListeners adds listeners to the cache efficiently
func (c *ListenerCache) AddListeners(cniPod *registryv1.CNIPod) {
	// Generate listeners first (before lock) if this operation is pure/thread-safe
	inbound, outbound := proxy.GenerateListenersFromEvent(cniPod)

	// Early return if no listeners generated
	if inbound == nil && outbound == nil {
		return
	}

	// Pre-allocate slice with the exact size
	listeners := make([]*listenerv3.Listener, 0, 2)
	if inbound != nil {
		listeners = append(listeners, inbound)
	}
	if outbound != nil {
		listeners = append(listeners, outbound)
	}

	// Early return if no valid listeners
	if len(listeners) == 0 {
		return
	}

	path := cniPod.NetworkNamespace

	c.mu.Lock()
	defer c.mu.Unlock()

	pl, exists := c.listeners[path]
	if !exists {
		// Pre-size map exactly for the listeners we have
		pl = &pathListeners{
			listeners: make(map[string]*listenerv3.Listener, len(listeners)),
			resources: make([]types.Resource, 0, len(listeners)),
			dirty:     false,
		}
		c.listeners[path] = pl

		// Fast path for new - all listeners are new
		for _, listener := range listeners {
			pl.listeners[listener.Name] = listener
			c.totalListeners++
		}
		pl.dirty = true
		return
	}

	// Slow path - check for existing listeners
	var added bool
	for _, listener := range listeners {
		if existing, exists := pl.listeners[listener.Name]; !exists {
			pl.listeners[listener.Name] = listener
			c.totalListeners++
			added = true
		} else if !listenersEqual(existing, listener) {
			// Only update if actually changed
			pl.listeners[listener.Name] = listener
			added = true
		}
	}

	// Only mark dirty if we actually changed something
	if added {
		pl.dirty = true
	}
}

func listenersEqual(a, b *listenerv3.Listener) bool {
	// Simple pointer equality or implement deeper comparison
	return proto.Equal(a, b)
}

// RemoveListeners removes all listeners for a path
func (c *ListenerCache) RemoveListeners(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if pl, exists := c.listeners[path]; exists {
		c.totalListeners -= len(pl.listeners)
		delete(c.listeners, path)
	}
}

// GetListeners retrieves listeners for a path, rebuilding if necessary
func (c *ListenerCache) GetListeners(path string) []types.Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	pl, exists := c.listeners[path]
	if !exists {
		return nil
	}

	if pl.dirty {
		c.rebuildListenerResources(pl)
		pl.dirty = false
	}

	return pl.resources
}

// GetAllListeners collects all listeners from all paths
func (c *ListenerCache) GetAllListeners() []types.Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resources := make([]types.Resource, 0, c.totalListeners)
	for _, pl := range c.listeners {
		if pl.dirty {
			c.rebuildListenerResources(pl)
			pl.dirty = false
		}
		resources = append(resources, pl.resources...)
	}
	return resources
}

// rebuildListenerResources rebuilds the resource slice from listeners
func (c *ListenerCache) rebuildListenerResources(pl *pathListeners) {
	pl.resources = make([]types.Resource, 0, len(pl.listeners))
	for _, listener := range pl.listeners {
		pl.resources = append(pl.resources, listener)
	}
}
