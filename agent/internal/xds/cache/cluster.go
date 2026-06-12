package cache

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/proto"
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

	// Build a NEW load assignment rather than mutating the existing one in place:
	// the current proto is aliased into snapshots already set on go-control-plane,
	// which xDS server goroutines marshal without holding clusterMu — an in-place
	// mutation is a data race (torn marshal). The LocalityLbEndpoints values are
	// never mutated after creation, so sharing them between assignments is safe.
	cla := proxy.NewClusterLoadAssignment(clusterName)
	for _, ep := range entry.endpoints {
		cla.Endpoints = append(cla.Endpoints, ep)
	}
	// Map iteration order is random; sort so the remaining (unchanged) endpoints
	// don't make the EDS resource hash as changed beyond the actual removal.
	proxy.SortLocalityLbEndpoints(cla.Endpoints)
	entry.loadAssignment = cla

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

// clustersEndpointsAndVhosts returns all cached cluster, endpoint, and virtual
// host resources as separate slices. It returns the concrete vhost type to avoid
// boxing/unboxing at the caller. Each service cluster speaks per-source mTLS to the
// destination node, so the upstream transport-socket matcher (selecting the source
// pod's certificate by its network namespace) is injected here from the current
// local workloads; the stored cluster is cloned so prior snapshots are unaffected.
// Before the node SVID is served the base cluster is emitted without the matcher.
func (c *SnapshotCache) clustersEndpointsAndVhosts() ([]types.Resource, []types.Resource, []*routev3.VirtualHost) {
	c.localMu.RLock()
	netnsToID := make(map[string]string, len(c.localWorkloads))
	ids := make([]string, 0, len(c.localWorkloads))
	for netns, id := range c.localWorkloads {
		netnsToID[netns] = id
		ids = append(ids, id)
	}
	nodeSpiffeID := c.nodeSpiffeID
	trustDomain := c.trustDomain
	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)
	c.localMu.RUnlock()

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	clusters := make([]types.Resource, 0, len(c.clusters))
	clas := make([]types.Resource, 0, len(c.clusters))
	vhosts := make([]*routev3.VirtualHost, 0, len(c.clusters))
	for serviceName, entry := range c.clusters {
		cluster := entry.cluster
		if nodeSpiffeID != "" {
			// Expected server identities for this service: one SPIFFE ID per
			// endpoint namespace. The handshake then proves the peer IS the
			// service asked for, not merely some workload in the trust domain.
			sanURIs := make([]string, 0, len(entry.sanNamespaces))
			for _, ns := range entry.sanNamespaces {
				sanURIs = append(sanURIs, fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, ns, serviceName))
			}
			cl, _ := proto.Clone(entry.cluster).(*clusterv3.Cluster)
			proxy.InjectUpstreamMTLS(cl, netnsToID, ids, nodeSpiffeID, validationContextName, sanURIs)
			cluster = cl
		}
		clusters = append(clusters, cluster)
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

// LoadClustersFromRegistry fetches the HTTP service endpoints from the registry
// for the node dependency set (local pods' declared upstreams + their own
// services — demand-scoped distribution, proposal 004), generates Envoy
// clusters and load assignments for each in-scope service, and populates the
// cache. The outbound RDS virtual-host set shrinks identically (vhosts are
// derived from the cluster entries). After populating the cache, it generates
// and sets a new cluster snapshot.
// Returns an error if registry listing fails or snapshot generation fails.
func (c *SnapshotCache) LoadClustersFromRegistry(ctx context.Context, clusterName string, nodeName string, reg registry.Registry) error {
	c.log.V(2).Info("generating clusters and endpoints from registry")

	serviceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	if err != nil {
		return fmt.Errorf("failed to list endpoints from registry: %w", err)
	}

	deps := c.DependencySet()
	localRegion, localZone := c.nodeLocality()
	c.log.V(1).Info("found service endpoints in registry",
		"count", len(serviceEndpoints), "dependencySet", len(deps))

	c.clusterMu.Lock()
	// Rebuild the cluster set so this method is idempotent and safe to call
	// repeatedly (on registry changes, not just at startup). Services that
	// disappeared from the listing are NOT dropped immediately: pod churn can
	// transiently empty a service's endpoint set, and removing its vhost
	// turns live client traffic into 404s (route-table miss) instead of the
	// honest, retriable 503 of an empty cluster. Absent services are retained
	// with empty endpoints for serviceRetentionGrace, then pruned. The same
	// grace acts as hysteresis for services leaving the dependency set.
	prev := c.clusters
	c.clusters = make(map[string]clusterEntry, len(deps))
	nodeSubsetKeys := make(map[string]struct{})
	for serviceName, endpoints := range serviceEndpoints {
		// Demand scoping: only services in the node dependency set are
		// distributed to this node's proxy. Everything else stays in the
		// registrar; an undeclared upstream is fetched on demand (ODCDS).
		if _, inScope := deps[serviceName]; !inScope {
			continue
		}
		// The outbound service cluster speaks per-source mTLS HTTP/2 to each
		// destination pod's mesh inbound (pod_ip:15008). The per-source mTLS transport
		// socket is injected at snapshot time (clustersEndpointsAndVhosts).
		// Server-identity pinning: the union of the endpoints' namespaces
		// renders the service's expected SPIFFE IDs at snapshot time.
		nsSet := make(map[string]struct{})
		for _, endpoint := range endpoints {
			if ns := endpoint.GetKubernetesMetadata().GetNamespace(); ns != "" {
				nsSet[ns] = struct{}{}
			}
		}
		sanNamespaces := make([]string, 0, len(nsSet))
		for ns := range nsSet {
			sanNamespaces = append(sanNamespaces, ns)
		}
		sort.Strings(sanNamespaces)

		// Provider-defined subset keys: the union of this service's endpoint
		// metadata keys becomes its subset selectors, and the node-wide union
		// (below) the shared subset-headers ECDS mapping — the provider's
		// vocabulary travels to its consumers via the control plane.
		serviceKeys := make(map[string]struct{})
		for _, endpoint := range endpoints {
			for key := range endpoint.GetMetadata() {
				if proxy.ValidSubsetKey(key) {
					serviceKeys[key] = struct{}{}
				}
			}
		}
		sortedKeys := proxy.SortSubsetKeys(serviceKeys)
		for _, k := range sortedKeys {
			nodeSubsetKeys[k] = struct{}{}
		}

		cluster := proxy.NewServiceCluster(serviceName, c.meshDomain, sortedKeys)
		cla := proxy.NewClusterLoadAssignment(serviceName)
		vhost := proxy.BuildOutboundClusterVirtualHost(serviceName, c.meshDomain)
		epMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(endpoints))

		for _, endpoint := range endpoints {
			lbEp := proxy.ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint, localRegion, localZone)
			cla.Endpoints = append(cla.Endpoints, lbEp)
			epMap[endpoint.GetIp()] = lbEp
		}
		// Registry listing order is not guaranteed stable across syncs; sort so a
		// re-sync with an unchanged endpoint set never hashes as an EDS change.
		proxy.SortLocalityLbEndpoints(cla.Endpoints)

		c.clusters[serviceName] = clusterEntry{
			cluster:        cluster,
			loadAssignment: cla,
			endpoints:      epMap,
			vhost:          vhost,
			sanNamespaces:  sanNamespaces,
		}
	}

	// Retain recently disappeared services with an empty endpoint set.
	now := time.Now()
	for name, entry := range prev {
		if _, present := c.clusters[name]; present {
			continue
		}
		if entry.absentSince.IsZero() {
			entry.absentSince = now
			entry.loadAssignment = proxy.NewClusterLoadAssignment(name)
			entry.endpoints = map[string]*endpointv3.LocalityLbEndpoints{}
			c.log.Info("service disappeared from registry; retaining empty cluster/vhost for grace period",
				"service", name, "grace", c.retentionGrace().String())
		} else if now.Sub(entry.absentSince) > c.retentionGrace() {
			c.log.Info("service absent past grace period; pruning", "service", name)
			continue
		}
		c.clusters[name] = entry
	}

	c.clusterMu.Unlock()

	// Publish the node-wide subset-key union as the shared ECDS mapping.
	c.subsetMu.Lock()
	c.subsetHeaderKeys = proxy.SortSubsetKeys(nodeSubsetKeys)
	c.subsetMu.Unlock()

	c.log.V(1).Info("loaded clusters from registry", "count", len(c.clusters))

	return c.generateClusterSnapshot(ctx)
}

// defaultServiceRetentionGrace is how long a service that vanished from the
// registry listing keeps its (empty) cluster and vhost before being pruned.
// Sized to outlast a rolling-restart churn window (~40s observed) with margin.
const defaultServiceRetentionGrace = 90 * time.Second

// retentionGrace returns the configured service retention grace (test hook).
func (c *SnapshotCache) retentionGrace() time.Duration {
	if c.serviceRetentionGrace > 0 {
		return c.serviceRetentionGrace
	}
	return defaultServiceRetentionGrace
}

// generateClusterSnapshot regenerates the node snapshot after a cluster,
// endpoint or route change. It delegates to generateSnapshot, which emits a
// complete snapshot of all resource types so cluster updates do not clobber
// listeners or secrets.
func (c *SnapshotCache) generateClusterSnapshot(ctx context.Context) error {
	return c.generateSnapshot(ctx)
}
