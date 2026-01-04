package cache

import (
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
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
		clusters:  make(map[string]*clusterv3.Cluster, 16),
		resources: make([]types.Resource, 0, 32),
		dirty:     false,
	}
}

// AddClusterOrUpdate adds a cluster if it doesn't exist or updates if it does
func (c *ClusterCache) AddClusterOrUpdate(event *registryv1.Event_KubernetesPod) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cluster := proxy.NewCluster(event)

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
		c.rebuildClusterResources()
		c.dirty = false
	}

	return c.resources
}

// rebuildClusterResources rebuilds the resource slice from clusters
func (c *ClusterCache) rebuildClusterResources() {
	c.resources = make([]types.Resource, 0, len(c.clusters))
	for _, cluster := range c.clusters {
		c.resources = append(c.resources, cluster)
	}
}
