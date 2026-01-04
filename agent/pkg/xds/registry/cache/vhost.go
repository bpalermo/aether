package cache

import (
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
)

type serviceName string

type RouteCache struct {
	mu sync.RWMutex
	// Virtual hosts indexed by service name for O(1) lookups
	outboundVHosts map[serviceName]*routev3.VirtualHost
}

// NewRouteCache creates a new cluster cache with initial capacity
func NewRouteCache() *RouteCache {
	return &RouteCache{
		sync.RWMutex{},
		make(map[serviceName]*routev3.VirtualHost, 16),
	}
}

// AddOutboundVirtualHost adds an outbound virtual host to the cache
func (c *RouteCache) AddOutboundVirtualHost(svcName serviceName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outboundVHosts[svcName] = proxy.NewServiceVirtualHost(string(svcName))
}
