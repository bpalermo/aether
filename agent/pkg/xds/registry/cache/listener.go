package cache

import (
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
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
func (c *ListenerCache) AddListeners(event *registryv1.Event_NetworkNamespace) {
	c.mu.Lock()
	defer c.mu.Unlock()

	resources := proxy.GenerateListenersFromEvent(event)
	listeners := make([]*listenerv3.Listener, 0, len(resources))
	for _, res := range resources {
		if listener, ok := res.(*listenerv3.Listener); ok {
			listeners = append(listeners, listener)
		}
	}

	pl, exists := c.listeners[event.GetPath()]
	if !exists {
		pl = &pathListeners{
			listeners: make(map[string]*listenerv3.Listener, len(listeners)),
			resources: make([]types.Resource, 0, len(listeners)),
			dirty:     false,
		}
		c.listeners[event.GetPath()] = pl
	}

	// Add or update listeners
	for _, listener := range listeners {
		if _, exists := pl.listeners[listener.Name]; !exists {
			c.totalListeners++
		}
		pl.listeners[listener.Name] = listener
		pl.dirty = true
	}
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
