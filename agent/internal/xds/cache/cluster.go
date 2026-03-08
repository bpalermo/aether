package cache

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

const (
	clusterVersionLabel = "cluster"
	vhostVersionLabel   = "vhost"
)

// RemoveCluster removes the cluster, its endpoints, and its virtual host
// associated with the given name, then regenerates the snapshot.
func (c *SnapshotCache) RemoveCluster(ctx context.Context, clusterName string) error {
	c.clusterMu.Lock()
	_, exists := c.clusters[clusterName]
	if exists {
		delete(c.clusters, clusterName)
	}
	c.clusterMu.Unlock()

	if !exists {
		return nil
	}

	return c.generateClusterSnapshot(ctx)
}

// ClustersAndEndpointsAndVhosts returns all cached cluster resources as a flat slice.
func (c *SnapshotCache) ClustersAndEndpointsAndVhosts() ([]types.Resource, []types.Resource, []types.Resource) {
	c.clusterMu.RLock()
	c.vhostMu.RLock()
	defer c.clusterMu.RUnlock()
	defer c.vhostMu.RUnlock()

	clusters := make([]types.Resource, 0, len(c.clusters))
	clas := make([]types.Resource, 0, len(c.clusters))
	vhosts := make([]types.Resource, 0, len(c.clusters))
	for clusterName, entry := range c.clusters {
		clusters = append(clusters, entry.cluster)

		if entry.loadAssignment != nil {
			clas = append(clas, entry.loadAssignment)
		}

		vhosts = append(vhosts, c.vhosts[clusterName])
	}
	return clusters, clas, vhosts
}

// Endpoints return all cached endpoint resources as a flat slice.
func (c *SnapshotCache) Endpoints() []types.Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resources := make([]types.Resource, 0, len(c.entries))
	for _, entry := range c.entries {
		if entry.loadAssignment != nil {
			resources = append(resources, entry.loadAssignment)
		}
	}
	return resources
}

// VirtualHosts returns all cached virtual host resources as a flat slice.
func (c *SnapshotCache) VirtualHosts() []types.Resource {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	resources := make([]types.Resource, 0, len(c.clusters))
	for _, entry := range c.clusters {
		if entry.vhost != nil {
			resources = append(resources, entry.vhost)
		}
	}
	return resources
}

// LoadClustersFromRegistry fetches all service endpoints from the registry,
// generates clusters and load assignments, populates the cache, and generates a snapshot.
func (c *SnapshotCache) LoadClustersFromRegistry(ctx context.Context, clusterName string, nodeName string, reg registry.Registry) error {
	c.log.V(2).Info("generating clusters and endpoints from registry")

	serviceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	if err != nil {
		return fmt.Errorf("failed to list endpoints from registry: %w", err)
	}
	c.log.V(1).Info("found service endpoints in registry", "count", len(serviceEndpoints))

	c.mu.Lock()
	for serviceName, endpoints := range serviceEndpoints {
		cluster := proxy.NewClusterForService(serviceName)
		cla := proxy.NewClusterLoadAssignment(serviceName)
		vhost := proxy.BuildOutboundClusterVirtualHost(serviceName)

		var localCluster *clusterv3.Cluster
		var localCla *endpointv3.ClusterLoadAssignment
		for _, endpoint := range endpoints {
			cla.Endpoints = append(cla.Endpoints, proxy.LocalityLbEndpointFromRegistryEndpoint(endpoint))

			if isLocal(clusterName, nodeName, endpoint) {
				localClusterName := fmt.Sprintf("local_%s", serviceName)

				if localCluster == nil {
					localCluster = proxy.NewLocalClusterForService(localClusterName, endpoint)
				}

				if localCla == nil {
					localCla = proxy.NewClusterLoadAssignment(localClusterName)
				}

				localCla.Endpoints = append(localCla.Endpoints, proxy.LocalLocalityLbEndpointFromRegistryEndpoint(endpoint))
			}
		}

		c.entries[serviceName] = clusterEntry{
			cluster:        cluster,
			loadAssignment: cla,
		}

		if localCluster != nil {
			localName := fmt.Sprintf("local_%s", serviceName)
			c.entries[localName] = clusterEntry{
				cluster:        localCluster,
				loadAssignment: localCla,
			}
		}

		c.vhosts[serviceName] = vhost
	}

	// Add the local SPIRE cluster
	c.entries[config.SpireAgentClusterName] = clusterEntry{
		cluster:        config.NewLocalSpireCluster(),
		loadAssignment: config.NewLocalSpireClusterLoadAssignment(),
	}
	c.mu.Unlock()

	c.log.V(1).Info("loaded clusters from registry", "count", len(c.entries))

	return c.generateClusterSnapshot(ctx)
}

// generateClusterSnapshot creates a new snapshot with the current clusters, endpoints,
// and routes, then sets it on the underlying snapshot cache.
func (c *SnapshotCache) generateClusterSnapshot(ctx context.Context) error {
	v := generateSnapshotVersion(clusterVersionLabel, c.version)

	clusters, endpoints, vhostResources := c.ClustersAndEndpointsAndVhosts()
	c.log.V(1).Info("setting snapshot", "version", v, "clusters", len(clusters), "endpoints", len(endpoints), "vhosts", len(vhostResources))

	// Convert []types.Resource back to []*routev3.VirtualHost for route building
	vhosts := make([]*routev3.VirtualHost, 0, len(vhostResources))
	for _, r := range vhostResources {
		if vh, ok := r.(*routev3.VirtualHost); ok {
			vhosts = append(vhosts, vh)
		}
	}

	snapshot, err := cachev3.NewSnapshot(v, map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  clusters,
		resourcev3.EndpointType: endpoints,
		resourcev3.RouteType:    {proxy.BuildOutboundRouteConfiguration(vhosts)},
	})
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if err := snapshot.Consistent(); err != nil {
		return fmt.Errorf("snapshot inconsistency: %w", err)
	}

	if err := c.SnapshotCache.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	return nil
}

// generateVhostsSnapshot creates a new snapshot with the current virtual hosts,
// then sets it on the underlying snapshot cache.
func (c *SnapshotCache) generateVhostsSnapshot(ctx context.Context) error {
	v := generateSnapshotVersion(vhostVersionLabel, c.version)

	vhosts := c.VirtualHosts()
	c.log.V(1).Info("setting vhosts snapshot", "version", v, "vhosts", len(vhosts))

	snapshot, err := cachev3.NewSnapshot(v, map[resourcev3.Type][]types.Resource{
		resourcev3.VirtualHostType: vhosts,
	})
	if err != nil {
		return fmt.Errorf("failed to create vhosts snapshot: %w", err)
	}

	if err := snapshot.Consistent(); err != nil {
		return fmt.Errorf("vhosts snapshot inconsistency: %w", err)
	}

	if err := c.SnapshotCache.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set vhosts snapshot: %w", err)
	}

	return nil
}

func isLocal(clusterName string, nodeName string, endpoint *registryv1.ServiceEndpoint) bool {
	return clusterName == endpoint.GetClusterName() && nodeName == endpoint.GetKubernetesMetadata().GetNodeName()
}
