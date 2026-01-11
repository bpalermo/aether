package cache

import (
	"fmt"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/proto"
)

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
func (c *EndpointCache) AddEndpoint(clusterName ClusterName, pod *registryv1.KubernetesPod) {
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
func (c *EndpointCache) RemoveEndpoint(pod *registryv1.KubernetesPod) {
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

func namespacedPodNameFromEvent(pod *registryv1.KubernetesPod) NamespacedPodName {
	return NamespacedPodName(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
}

func (c *EndpointCache) GetAllEndpoints() []types.Resource {
	c.mu.Lock()
	defer c.mu.Unlock()

	var clas []types.Resource

	// Rebuild dirty CLAs first
	for _, cluster := range c.clusters {
		if cluster.dirty {
			c.rebuildCLA(cluster)
			cluster.dirty = false
		}
	}

	// Collect all ClusterLoadAssignments
	for _, cluster := range c.clusters {
		if cluster.cla != nil {
			clas = append(clas, cluster.cla)
		}
	}

	return clas
}
