package cache

import (
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/proto"
)

type serviceName string

type RouteCache struct {
	mu sync.RWMutex

	// outboundVHosts outbound virtual hosts indexed by service name for O(1) lookups
	outboundVHosts map[serviceName]*routev3.VirtualHost

	// Cache the slice representation to avoid rebuilding on every read
	vhostsSlice []*routev3.VirtualHost
	dirty       bool
}

// NewRouteCache creates a new route cache with initial capacity
func NewRouteCache() *RouteCache {
	return &RouteCache{
		outboundVHosts: make(map[serviceName]*routev3.VirtualHost, 16),
		vhostsSlice:    make([]*routev3.VirtualHost, 0, 16),
		dirty:          false,
	}
}

// AddOutboundVirtualHost adds an outbound virtual host to the cache
func (c *RouteCache) AddOutboundVirtualHost(svcName string) {
	key := serviceName(svcName)

	// Build VHost outside lock
	vhost := proxy.BuildOutboundClusterVirtualHost(svcName)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if already exists and is identical
	if existing, exists := c.outboundVHosts[key]; exists {
		if vhostsEqual(existing, vhost) {
			return // No changes needed
		}
	}

	c.outboundVHosts[key] = vhost
	c.dirty = true
}

// RemoveOutboundVirtualHost removes an outbound virtual host from the cache
func (c *RouteCache) RemoveOutboundVirtualHost(svcName string) {
	name := serviceName(svcName)

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.outboundVHosts[name]; !exists {
		return // Nothing to remove
	}

	delete(c.outboundVHosts, name)
	c.dirty = true
}

// GetAllRouteConfiguration returns all route configurations
func (c *RouteCache) GetAllRouteConfiguration() []types.Resource {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Rebuild slice only if dirty
	if c.dirty {
		c.rebuildSliceLocked()
	}

	// Build the outbound route configuration
	routeConfig := proxy.BuildOutboundRouteConfiguration(c.vhostsSlice)

	// Convert to types.Resource interface
	resources := make([]types.Resource, 0, 1)
	if routeConfig != nil {
		resources = append(resources, routeConfig)
	}

	return resources
}

// rebuildSliceLocked rebuilds the vhosts slice from the map
// Must be called with write lock held
func (c *RouteCache) rebuildSliceLocked() {
	c.vhostsSlice = make([]*routev3.VirtualHost, 0, len(c.outboundVHosts))
	for _, vhost := range c.outboundVHosts {
		c.vhostsSlice = append(c.vhostsSlice, vhost)
	}
	c.dirty = false
}

// Clear removes all virtual hosts from the cache
func (c *RouteCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.outboundVHosts = make(map[serviceName]*routev3.VirtualHost, 16)
	c.vhostsSlice = c.vhostsSlice[:0] // Reuse backing array
	c.dirty = false
}

// Size returns the number of virtual hosts in the cache
func (c *RouteCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.outboundVHosts)
}

// vhostsEqual compares two virtual hosts for equality
func vhostsEqual(a, b *routev3.VirtualHost) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Add more sophisticated comparison if needed
	return proto.Equal(a, b)
}
