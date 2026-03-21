package cache

import (
	"context"
	"fmt"

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
	// clusterVersionLabel is used in snapshot version strings to identify
	// cluster and endpoint resource snapshots.
	clusterVersionLabel = "cluster"
	// vhostVersionLabel is used in snapshot version strings to identify
	// virtual host resource snapshots.
	vhostVersionLabel = "vhost"
)

// RemoveEndpoint removes a single endpoint by IP from the given cluster's
// load assignment and regenerates the cluster snapshot. If the cluster does
// not exist or the IP is not found in the endpoint map, it returns nil
// without regenerating the snapshot. The cluster itself is kept even if
// the endpoint map becomes empty.
func (c *SnapshotCache) RemoveEndpoint(ctx context.Context, clusterName string, ip string) error {
	c.clusterMu.Lock()
	entry, exists := c.clusters[clusterName]
	if !exists {
		c.clusterMu.Unlock()
		return nil
	}

	if _, ok := entry.endpoints[ip]; !ok {
		c.clusterMu.Unlock()
		return nil
	}

	delete(entry.endpoints, ip)

	// Rebuild the load assignment endpoints slice from the map.
	endpoints := make([]*endpointv3.LocalityLbEndpoints, 0, len(entry.endpoints))
	for _, ep := range entry.endpoints {
		endpoints = append(endpoints, ep)
	}
	entry.loadAssignment.Endpoints = endpoints

	c.clusters[clusterName] = entry
	c.clusterMu.Unlock()

	return c.generateClusterSnapshot(ctx)
}

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

// clustersEndpointsAndVhosts returns all cached cluster, endpoint, and virtual host
// resources as separate slices. It returns concrete vhost type to avoid boxing/unboxing
// at the caller. Must be called with clusterMu held.
func (c *SnapshotCache) clustersEndpointsAndVhosts() ([]types.Resource, []types.Resource, []*routev3.VirtualHost) {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	clusters := make([]types.Resource, 0, len(c.clusters))
	clas := make([]types.Resource, 0, len(c.clusters))
	vhosts := make([]*routev3.VirtualHost, 0, len(c.clusters))
	for _, entry := range c.clusters {
		clusters = append(clusters, entry.cluster)
		if entry.loadAssignment != nil {
			clas = append(clas, entry.loadAssignment)
		}
		if entry.vhost != nil {
			vhosts = append(vhosts, entry.vhost)
		}
	}
	return clusters, clas, vhosts
}

// Endpoints returns the pre-built load assignment (cluster endpoints) for the given
// cluster name as a resource slice. Returns nil if the cluster does not exist or
// has no load assignment. Thread-safe.
func (c *SnapshotCache) Endpoints(clusterName string) []types.Resource {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	entry, ok := c.clusters[clusterName]
	if !ok || entry.loadAssignment == nil {
		return nil
	}
	return []types.Resource{entry.loadAssignment}
}

// VirtualHosts returns all cached virtual host resources as a flat slice.
// Virtual hosts define routing rules for outbound traffic to services. Thread-safe.
func (c *SnapshotCache) VirtualHosts() []types.Resource {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	resources := make([]types.Resource, 0, len(c.clusters))
	for _, entry := range c.clusters {
		resources = append(resources, entry.vhost)
	}
	return resources
}

// LoadClustersFromRegistry fetches all HTTP service endpoints from the registry,
// generates Envoy clusters and load assignments for each service, and populates
// the cache. It also creates a local cluster for each service with endpoints
// on the same node, and adds a local SPIRE cluster for mTLS configuration.
// After populating the cache, it generates and sets a new cluster snapshot.
// Returns an error if registry listing fails or snapshot generation fails.
func (c *SnapshotCache) LoadClustersFromRegistry(ctx context.Context, clusterName string, nodeName string, reg registry.Registry) error {
	c.log.V(2).Info("generating clusters and endpoints from registry")

	serviceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	if err != nil {
		return fmt.Errorf("failed to list endpoints from registry: %w", err)
	}
	c.log.V(1).Info("found service endpoints in registry", "count", len(serviceEndpoints))

	c.clusterMu.Lock()
	if c.clusters == nil {
		c.clusters = make(map[string]clusterEntry)
	}
	for serviceName, endpoints := range serviceEndpoints {
		cluster := proxy.NewClusterForService(serviceName)
		cla := proxy.NewClusterLoadAssignment(serviceName)
		vhost := proxy.BuildOutboundClusterVirtualHost(serviceName)
		epMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(endpoints))

		var localCluster *clusterv3.Cluster
		var localCla *endpointv3.ClusterLoadAssignment
		var localEpMap map[string]*endpointv3.LocalityLbEndpoints
		for _, endpoint := range endpoints {
			lbEp := proxy.LocalityLbEndpointFromRegistryEndpoint(endpoint)
			cla.Endpoints = append(cla.Endpoints, lbEp)
			epMap[endpoint.GetIp()] = lbEp

			if isLocal(clusterName, nodeName, endpoint) {
				localClusterName := fmt.Sprintf("local_%s", serviceName)

				if localCluster == nil {
					localCluster = proxy.NewLocalClusterForService(localClusterName, endpoint)
					localCla = proxy.NewClusterLoadAssignment(localClusterName)
					localEpMap = make(map[string]*endpointv3.LocalityLbEndpoints)
				}

				localLbEp := proxy.LocalLocalityLbEndpointFromRegistryEndpoint(endpoint)
				localCla.Endpoints = append(localCla.Endpoints, localLbEp)
				localEpMap[endpoint.GetIp()] = localLbEp
			}
		}

		c.clusters[serviceName] = clusterEntry{
			cluster:        cluster,
			loadAssignment: cla,
			endpoints:      epMap,
			vhost:          vhost,
		}

		if localCluster != nil {
			localName := fmt.Sprintf("local_%s", serviceName)
			c.clusters[localName] = clusterEntry{
				cluster:        localCluster,
				loadAssignment: localCla,
				endpoints:      localEpMap,
			}
		}
	}

	c.clusterMu.Unlock()

	c.log.V(1).Info("loaded clusters from registry", "count", len(c.clusters))

	return c.generateClusterSnapshot(ctx)
}

// generateClusterSnapshot creates a new snapshot with the current clusters, endpoints,
// and routes, validates it for consistency, and sets it on the underlying snapshot cache.
// The snapshot version is generated using the cluster version label. Returns an error
// if snapshot creation or validation fails.
func (c *SnapshotCache) generateClusterSnapshot(ctx context.Context) error {
	v := generateSnapshotVersion(clusterVersionLabel, c.version)

	clusters, endpoints, vhosts := c.clustersEndpointsAndVhosts()
	c.log.V(1).Info("setting snapshot", "version", v, "clusters", len(clusters), "endpoints", len(endpoints), "vhosts", len(vhosts))

	resources := map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  clusters,
		resourcev3.EndpointType: endpoints,
	}
	if len(vhosts) > 0 {
		resources[resourcev3.RouteType] = []types.Resource{proxy.BuildOutboundRouteConfiguration(vhosts)}
	}

	snapshot, err := cachev3.NewSnapshot(v, resources)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if err := c.SnapshotCache.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	return nil
}

// generateVhostsSnapshot creates a new snapshot with the current virtual hosts,
// validates it for consistency, and sets it on the underlying snapshot cache.
// The snapshot version is generated using the vhost version label. Returns an error
// if snapshot creation or validation fails.
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

// isLocal reports whether the given endpoint belongs to the local cluster and node.
// It is used to identify endpoints that should be included in the local cluster variant.
func isLocal(clusterName string, nodeName string, endpoint *registryv1.ServiceEndpoint) bool {
	return clusterName == endpoint.GetClusterName() && nodeName == endpoint.GetKubernetesMetadata().GetNodeName()
}
