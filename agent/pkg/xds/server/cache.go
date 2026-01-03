package server

import (
	"fmt"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/proto"
)

// ClusterCache is an optimized cache for clusters
type ClusterCache struct {
	mu sync.RWMutex
	// Clusters indexed by name for O(1) lookups
	clusters map[string]*clusterv3.Cluster
	// Track total clusters for monitoring
	totalClusters int
	// Pre-converted resources slice for quick retrieval
	resources []types.Resource
	// Track if resources need rebuild
	dirty bool
}

// NewClusterCache creates a new cluster cache with initial capacity
func NewClusterCache() *ClusterCache {
	return &ClusterCache{
		clusters:  make(map[string]*clusterv3.Cluster, 32), // Pre-allocate for typical cluster count
		resources: make([]types.Resource, 0, 32),
		dirty:     false,
	}
}

// AddClusterOrUpdate adds a cluster if it doesn't exist or updates if it does
func (c *ClusterCache) AddClusterOrUpdate(cluster *clusterv3.Cluster) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.clusters[cluster.Name]; !exists {
		c.totalClusters++
		c.clusters[cluster.Name] = cluster
		c.dirty = true
	} else if !proto.Equal(c.clusters[cluster.Name], cluster) {
		c.clusters[cluster.Name] = cluster
		c.dirty = true
	}
}

// RemoveCluster removes a cluster from the cache
func (c *ClusterCache) RemoveCluster(clusterName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.clusters[clusterName]; exists {
		delete(c.clusters, clusterName)
		c.totalClusters--
		c.dirty = true
	}
}

// GetAllClusters retrieves all clusters as resources
func (c *ClusterCache) GetAllClusters() []types.Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.dirty {
		c.rebuildResources()
		c.dirty = false
	}

	return c.resources
}

// rebuildResources rebuilds the resource slice from clusters
func (c *ClusterCache) rebuildResources() {
	c.resources = make([]types.Resource, 0, len(c.clusters))
	for _, cluster := range c.clusters {
		c.resources = append(c.resources, cluster)
	}
}

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
func (c *ListenerCache) AddListeners(path string, listeners []*listenerv3.Listener) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pl, exists := c.listeners[path]
	if !exists {
		pl = &pathListeners{
			listeners: make(map[string]*listenerv3.Listener, len(listeners)),
			resources: make([]types.Resource, 0, len(listeners)),
			dirty:     false,
		}
		c.listeners[path] = pl
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

type ClusterName string

type NamespacedPodName string

// EndpointCache is an optimized cache for endpoints with better memory layout
type EndpointCache struct {
	mu sync.RWMutex
	// Main cache indexed by cluster name
	clusters map[ClusterName]*registryCluster
	// podToCluster is a reverse index to optimize endpoint search
	podToCluster map[NamespacedPodName]ClusterName
	// Track total endpoints for capacity planning
	totalEndpoints int
}

// registryCluster represents a cluster with its endpoints
type registryCluster struct {
	// Pre-built ClusterLoadAssignment for quick retrieval
	cla *endpointv3.ClusterLoadAssignment
	// Endpoints indexed by pod for O(1) lookups
	endpoints map[NamespacedPodName]*endpointv3.LocalityLbEndpoints
	// Track if CLA needs rebuild
	dirty bool
}

// NewEndpointCache creates a new endpoint cache with initial capacity
// when a pod is deleted, we can't fetch the service name from the labels anymore,
// so we can't use a map indexed by the cluster name.
func NewEndpointCache() *EndpointCache {
	return &EndpointCache{
		clusters:     make(map[ClusterName]*registryCluster, 8), // Pre-allocate for typical cluster count
		podToCluster: make(map[NamespacedPodName]ClusterName, 32),
	}
}

// AddEndpoint adds an endpoint to the cache efficiently
func (c *EndpointCache) AddEndpoint(clusterName ClusterName, pod *registryv1.Event_KubernetesPod) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster, exists := c.clusters[clusterName]
	if !exists {
		// Initialize a new cluster with the pre-allocated endpoint map
		cluster = &registryCluster{
			cla:       proxy.NewClusterLoadAssignment(string(clusterName)),
			endpoints: make(map[NamespacedPodName]*endpointv3.LocalityLbEndpoints, 2), // Pre-allocate
			dirty:     false,
		}
		c.clusters[clusterName] = cluster
	}

	namespacedPodName := namespacedPodNameFromEvent(pod)

	// Update reverse index
	c.podToCluster[namespacedPodName] = clusterName

	// Only update if the endpoint doesn't exist or has changed
	le := proxy.LocalityLbEndpointFromPod(pod)
	if _, exists = cluster.endpoints[namespacedPodName]; !exists {
		cluster.endpoints[namespacedPodName] = le
		cluster.dirty = true
		c.totalEndpoints++
	} else if !proto.Equal(cluster.endpoints[namespacedPodName], le) {
		cluster.endpoints[namespacedPodName] = le
		cluster.dirty = true
	}
}

// RemoveEndpoint removes an endpoint from the cache efficiently.
func (c *EndpointCache) RemoveEndpoint(pod *registryv1.Event_KubernetesPod) {
	c.mu.Lock()
	defer c.mu.Unlock()

	namespacedPodName := namespacedPodNameFromEvent(pod)

	// O(1) lookup instead of O(n) scan
	clusterName, exists := c.podToCluster[namespacedPodName]
	if !exists {
		return
	}

	cluster := c.clusters[clusterName]
	if cluster != nil {
		delete(cluster.endpoints, namespacedPodName)
		cluster.dirty = true
		c.totalEndpoints--

		// Clean up reverse index
		delete(c.podToCluster, namespacedPodName)

		// Remove empty clusters
		if len(cluster.endpoints) == 0 {
			delete(c.clusters, clusterName)
		}
	}
}

// GetClusterLoadAssignment retrieves the CLA, rebuilding if necessary
func (c *EndpointCache) GetClusterLoadAssignment(clusterName ClusterName) *endpointv3.ClusterLoadAssignment {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cluster, exists := c.clusters[clusterName]
	if !exists {
		return nil
	}

	if cluster.dirty {
		c.rebuildCLA(cluster)
		cluster.dirty = false
	}

	return cluster.cla
}

// rebuildCLA rebuilds the ClusterLoadAssignment from endpoints
func (c *EndpointCache) rebuildCLA(cluster *registryCluster) {
	localities := make([]*endpointv3.LocalityLbEndpoints, 0, len(cluster.endpoints))
	for _, endpoint := range cluster.endpoints {
		localities = append(localities, endpoint)
	}
	cluster.cla.Endpoints = localities
}

func namespacedPodNameFromEvent(pod *registryv1.Event_KubernetesPod) NamespacedPodName {
	return NamespacedPodName(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
}
