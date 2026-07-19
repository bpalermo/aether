package cache

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
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
// boxing/unboxing at the caller. Each service cluster speaks per-source mTLS to
// the destination node; the upstream transport-socket matcher (selecting the
// source pod's certificate by its network namespace) is precomputed into
// entry.mtlsCluster at entry build/invalidation time (refreshEntryMTLSLocked),
// so snapshot generation only reads the cached proto (issue #537). Before the
// node SVID is served mtlsCluster is nil and the base cluster is emitted
// without the matcher.
func (c *SnapshotCache) clustersEndpointsAndVhosts() ([]types.Resource, []types.Resource, []*routev3.VirtualHost) {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	clusters := make([]types.Resource, 0, len(c.clusters))
	clas := make([]types.Resource, 0, len(c.clusters))
	vhosts := make([]*routev3.VirtualHost, 0, len(c.clusters))
	for _, entry := range c.clusters {
		// TCP services carry no HTTP (h2) cluster or outbound vhost: their data
		// path is the transparent-capture TCP floor's "tcp:<svc>" cluster (built
		// in captureTCPClusters), which shares this entry's bare-name EDS load
		// assignment. Emit only the load assignment so that EDS resolves.
		if entry.tcp {
			if entry.loadAssignment != nil {
				clas = append(clas, entry.loadAssignment)
			}
			continue
		}
		var cluster types.Resource = entry.cluster
		if entry.mtlsCluster != nil {
			cluster = entry.mtlsCluster
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
	c.log.DebugContext(ctx, "generating clusters and endpoints from registry")

	serviceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	if err != nil {
		return fmt.Errorf("failed to list endpoints from registry: %w", err)
	}

	deps := c.DependencySet()
	// GAMMA L7 rules per service (empty unless --gamma), snapshotted once before
	// the cluster lock so the per-service outbound vhost can be enriched.
	gammaRoutes := c.serviceRoutesSnapshot()
	// Service-wide chain filters (025 M4): enabled at each service's OUTBOUND vhost
	// too — the outbound route table serves the same GAMMA/chain config as cap_http.
	chainFilters := c.serviceChainFiltersSnapshot()
	localRegion, localZone := c.nodeLocality()
	// Split-horizon east/west waypoint (proposal 019): remote-cluster endpoints
	// are dialed at their node's routable IP + tunnel port. clusterName is this
	// agent's own cluster (the local side of the split). Inert when disabled.
	waypoint := proxy.WaypointRewrite{
		Enabled:      c.waypointEnabled,
		TunnelPort:   c.waypointTunnelPort,
		LocalCluster: clusterName,
	}

	// RPC-fill (cold path): a dependency missing from the watch-fed listing —
	// typically an ODCDS observation made milliseconds ago, whose endpoints
	// the re-filtered watch hasn't delivered yet — is fetched directly from
	// the registrar, gated by the service catalog so nonexistent services
	// cost nothing. This takes the watch round-trip off the cold path: the
	// FIRST reload after an observation builds the cluster. Fetch failures
	// degrade to the old behavior (the watch catch-up repairs).
	coldFillHTTPEndpoints(ctx, c.log, reg, deps, serviceEndpoints)

	// TCP (non-HTTP) services: their endpoints are the SAME pods reached over a
	// raw mTLS passthrough through the transparent-capture TCP floor. The floor's
	// "tcp:<svc>" cluster (built in captureTCPClusters) references the bare-name
	// EDS this method publishes, so a TCP service needs a cluster entry here too
	// — holding only the load assignment (no HTTP h2 cluster/vhost). A service is
	// HTTP or TCP, never both, so the two sets never share a name.
	tcpServiceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_TCP)
	if err != nil {
		return fmt.Errorf("failed to list TCP endpoints from registry: %w", err)
	}
	coldFillTCPEndpoints(ctx, c.log, reg, deps, serviceEndpoints, tcpServiceEndpoints)

	c.log.DebugContext(ctx, "found service endpoints in registry",
		"count", len(serviceEndpoints), "tcpCount", len(tcpServiceEndpoints), "dependencySet", len(deps))

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
	c.buildHTTPClustersLocked(ctx, deps, serviceEndpoints, gammaRoutes, chainFilters, localRegion, localZone, waypoint, nodeSubsetKeys)
	c.buildTCPClustersLocked(ctx, deps, tcpServiceEndpoints, localRegion, localZone, waypoint)
	c.retainAbsentClustersLocked(ctx, prev, deps)
	// Precompute each entry's mTLS-injected cluster + SAN URIs (issue #537) so
	// snapshot generation only reads the cached protos. Covers the freshly
	// built entries AND the retained (grace-period) ones, under the same
	// clusterMu hold that rebuilt the map.
	c.recomputeMTLSClustersLocked()
	c.clusterMu.Unlock()

	// Publish the node-wide subset-key union as the shared ECDS mapping.
	c.subsetMu.Lock()
	c.subsetHeaderKeys = proxy.SortSubsetKeys(nodeSubsetKeys)
	c.subsetMu.Unlock()

	c.log.DebugContext(ctx, "loaded clusters from registry", "count", len(c.clusters))

	return c.generateClusterSnapshot(ctx)
}

// coldFillHTTPEndpoints performs the RPC-fill cold path for HTTP services: deps
// missing from the watch-fed listing are fetched directly from the registrar.
func coldFillHTTPEndpoints(ctx context.Context, log interface {
	InfoContext(context.Context, string, ...any)
}, reg registry.Registry, deps map[string]struct{}, serviceEndpoints map[string][]*registryv1.ServiceEndpoint,
) {
	cat, ok := reg.(registry.ServiceCatalog)
	if !ok {
		return
	}
	for svc := range deps {
		if _, have := serviceEndpoints[svc]; have {
			continue
		}
		if !cat.HasService(svc) {
			continue
		}
		eps, err := reg.ListEndpoints(ctx, svc, registryv1.Service_PROTOCOL_HTTP)
		if err != nil {
			log.InfoContext(ctx, "cold-path endpoint fetch failed; watch catch-up will fill in", "service", svc, "error", err.Error())
			continue
		}
		if len(eps) > 0 {
			serviceEndpoints[svc] = eps
		}
	}
}

// coldFillTCPEndpoints performs the RPC-fill cold path for TCP services.
func coldFillTCPEndpoints(ctx context.Context, log interface {
	InfoContext(context.Context, string, ...any)
}, reg registry.Registry, deps map[string]struct{}, serviceEndpoints, tcpServiceEndpoints map[string][]*registryv1.ServiceEndpoint,
) {
	cat, ok := reg.(registry.ServiceCatalog)
	if !ok {
		return
	}
	for svc := range deps {
		if _, have := tcpServiceEndpoints[svc]; have {
			continue
		}
		if _, have := serviceEndpoints[svc]; have {
			continue // already an HTTP dependency
		}
		if !cat.HasService(svc) {
			continue
		}
		eps, err := reg.ListEndpoints(ctx, svc, registryv1.Service_PROTOCOL_TCP)
		if err != nil {
			log.InfoContext(ctx, "cold-path TCP endpoint fetch failed; watch catch-up will fill in", "service", svc, "error", err.Error())
			continue
		}
		if len(eps) > 0 {
			tcpServiceEndpoints[svc] = eps
		}
	}
}

// buildHTTPClustersLocked populates c.clusters with HTTP service entries from
// serviceEndpoints. Caller must hold clusterMu.
func (c *SnapshotCache) buildHTTPClustersLocked(ctx context.Context, deps map[string]struct{}, serviceEndpoints map[string][]*registryv1.ServiceEndpoint, gammaRoutes map[string][]proxy.GammaRoute, chainFilters map[string]proxy.ExtensionFilter, localRegion, localZone string, waypoint proxy.WaypointRewrite, nodeSubsetKeys map[string]struct{}) {
	for serviceName, endpoints := range serviceEndpoints {
		// Demand scoping: only services in the node dependency set are
		// distributed to this node's proxy. Everything else stays in the
		// registrar; an undeclared upstream is fetched on demand (ODCDS).
		if _, inScope := deps[serviceName]; !inScope {
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		c.buildHTTPServiceEntryLocked(serviceName, endpoints, gammaRoutes, chainFilters, localRegion, localZone, waypoint, nodeSubsetKeys)
	}
}

// buildHTTPServiceEntryLocked builds and stores the cluster entries for one HTTP
// service (default port cluster + per-non-default-port clusters). Caller must hold clusterMu.
func (c *SnapshotCache) buildHTTPServiceEntryLocked(serviceName string, endpoints []*registryv1.ServiceEndpoint, gammaRoutes map[string][]proxy.GammaRoute, chainFilters map[string]proxy.ExtensionFilter, localRegion, localZone string, waypoint proxy.WaypointRewrite, nodeSubsetKeys map[string]struct{}) {
	// The outbound service cluster speaks per-source mTLS HTTP/2 to each
	// destination pod's mesh inbound (pod_ip:18008). The per-source mTLS transport
	// socket is precomputed into entry.mtlsCluster below (recomputeMTLSClustersLocked).
	// Server-identity pinning: the union of the endpoints' namespaces
	// renders the service's expected SPIFFE IDs there too.
	sanNamespaces := endpointSANNamespaces(endpoints)
	sortedKeys := endpointSubsetKeys(endpoints, nodeSubsetKeys)

	fqdn := proxy.ServiceClusterName(serviceName, c.meshDomain)
	// Default/primary port: what the portless FQDN resolves to. Endpoints of
	// one service share the primary; take the first.
	defaultPort := endpoints[0].GetPort()

	defaultCla, defaultEpMap, buckets := buildHTTPEndpointBuckets(serviceName, endpoints, localRegion, localZone, waypoint, defaultPort)

	c.clusters[serviceName] = clusterEntry{
		cluster:        proxy.NewServiceCluster(fqdn, serviceName, serviceName, sortedKeys, c.perDownstreamConnectionPool()),
		loadAssignment: defaultCla,
		endpoints:      defaultEpMap,
		vhost:          outboundVhostWithChainFilter(fqdn, []string{fqdn, fmt.Sprintf("%s:%d", fqdn, defaultPort)}, gammaRoutes[serviceName], chainFilters, serviceName),
		sanNamespaces:  sanNamespaces,
		service:        serviceName,
		sni:            strconv.Itoa(int(defaultPort)),
	}

	// One cluster per non-default advertised port.
	for port, b := range buckets {
		portName := proxy.PortClusterName(serviceName, c.meshDomain, port)
		pcla := proxy.NewClusterLoadAssignment(portName)
		pcla.Endpoints = b.eps
		proxy.SortLocalityLbEndpoints(pcla.Endpoints)
		c.clusters[portName] = clusterEntry{
			cluster:        proxy.NewServiceCluster(portName, portName, serviceName, sortedKeys, c.perDownstreamConnectionPool()),
			loadAssignment: pcla,
			endpoints:      b.epMap,
			vhost:          outboundPortVhostWithChainFilter(portName, chainFilters, serviceName),
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(port)),
		}
	}
}

// portBucket accumulates per-port endpoints for a service.
type portBucket struct {
	eps   []*endpointv3.LocalityLbEndpoints
	epMap map[string]*endpointv3.LocalityLbEndpoints
}

// buildHTTPEndpointBuckets builds the default CLA/epMap and per-port buckets for a
// service's endpoints.
func buildHTTPEndpointBuckets(serviceName string, endpoints []*registryv1.ServiceEndpoint, localRegion, localZone string, waypoint proxy.WaypointRewrite, defaultPort uint32) (*endpointv3.ClusterLoadAssignment, map[string]*endpointv3.LocalityLbEndpoints, map[uint32]*portBucket) {
	// Default cluster: name = FQDN, EDS = bare service (all endpoints), vhost
	// carries both the portless and :defaultPort domains, SNI = default port.
	defaultCla := proxy.NewClusterLoadAssignment(serviceName)
	defaultEpMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(endpoints))
	// Per-port buckets: endpoints advertising each non-default port (per-port
	// EDS — safe new-port rollout: a caller of :P only ever lands on pods
	// serving P).
	buckets := make(map[uint32]*portBucket)

	for _, endpoint := range endpoints {
		lbEp := proxy.ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint, localRegion, localZone, waypoint)
		defaultCla.Endpoints = append(defaultCla.Endpoints, lbEp)
		defaultEpMap[endpoint.GetIp()] = lbEp

		served := endpoint.GetPorts()
		if len(served) == 0 {
			served = []uint32{endpoint.GetPort()}
		}
		for _, p := range served {
			if p == defaultPort {
				continue
			}
			b := buckets[p]
			if b == nil {
				b = &portBucket{epMap: map[string]*endpointv3.LocalityLbEndpoints{}}
				buckets[p] = b
			}
			b.eps = append(b.eps, lbEp)
			b.epMap[endpoint.GetIp()] = lbEp
		}
	}
	// Registry listing order is not guaranteed stable across syncs; sort so a
	// re-sync with an unchanged endpoint set never hashes as an EDS change.
	proxy.SortLocalityLbEndpoints(defaultCla.Endpoints)
	return defaultCla, defaultEpMap, buckets
}

// endpointSANNamespaces derives the sorted SAN namespace list from the endpoints'
// Kubernetes metadata (server-identity pinning).
func endpointSANNamespaces(endpoints []*registryv1.ServiceEndpoint) []string {
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
	return sanNamespaces
}

// endpointSubsetKeys derives the sorted subset key list for a service's endpoints
// and adds them to the node-wide nodeSubsetKeys union.
func endpointSubsetKeys(endpoints []*registryv1.ServiceEndpoint, nodeSubsetKeys map[string]struct{}) []string {
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
	return sortedKeys
}

// buildTCPClustersLocked populates c.clusters with TCP service entries. Caller
// must hold clusterMu.
func (c *SnapshotCache) buildTCPClustersLocked(ctx context.Context, deps map[string]struct{}, tcpServiceEndpoints map[string][]*registryv1.ServiceEndpoint, localRegion, localZone string, waypoint proxy.WaypointRewrite) {
	// TCP service entries: bare-name EDS load assignment + SAN/sni only. The
	// capture TCP floor's "tcp:<svc>" cluster (captureTCPClusters) references
	// this EDS resource (by bare name) and pins peer identity from sanNamespaces.
	for serviceName, endpoints := range tcpServiceEndpoints {
		if _, inScope := deps[serviceName]; !inScope {
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		sanNamespaces := endpointSANNamespaces(endpoints)
		defaultPort := endpoints[0].GetPort()
		cla := proxy.NewClusterLoadAssignment(serviceName)
		epMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(endpoints))
		for _, endpoint := range endpoints {
			lbEp := proxy.ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint, localRegion, localZone, waypoint)
			cla.Endpoints = append(cla.Endpoints, lbEp)
			epMap[endpoint.GetIp()] = lbEp
		}
		proxy.SortLocalityLbEndpoints(cla.Endpoints)
		c.clusters[serviceName] = clusterEntry{
			loadAssignment: cla,
			endpoints:      epMap,
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(defaultPort)),
			tcp:            true,
		}
	}
}

// retainAbsentClustersLocked re-inserts entries from prev that are no longer in
// c.clusters but are still in the dependency set, applying the retention grace.
// Caller must hold clusterMu.
func (c *SnapshotCache) retainAbsentClustersLocked(ctx context.Context, prev map[string]clusterEntry, deps map[string]struct{}) {
	// Retain recently disappeared services with an empty endpoint set —
	// but only services still IN the dependency set (a destination mid-churn
	// can transiently empty its endpoint listing; the retained empty cluster
	// keeps clients on fast retriable 503s instead of ODCDS stalls until the
	// endpoints return). A service that LEFT the dependency set is dropped
	// immediately: post-FQDN its authority cannot 404 (the *.<mesh-domain>
	// catch-all is the structural backstop), and retaining its vhost only
	// shadows the on-demand cold path — the stale-503 outage behind #167.
	// Any further traffic re-warms it as an observed dependency, which is
	// the truthful state for traffic nobody on the node declares.
	now := time.Now()
	for name, entry := range prev {
		if _, present := c.clusters[name]; present {
			continue
		}
		// entry.service maps a per-port cluster (keyed <fqdn>:<port>) back to its
		// bare service for the dependency-set check, so a service's default and
		// per-port clusters are retained/dropped together.
		if _, inScope := deps[entry.service]; !inScope {
			c.log.InfoContext(ctx, "service left dependency set; dropping cluster/vhost (cold path takes over)", "cluster", name, "service", entry.service)
			continue
		}
		if entry.absentSince.IsZero() {
			entry.absentSince = now
			entry.loadAssignment = proxy.NewClusterLoadAssignment(name)
			entry.endpoints = map[string]*endpointv3.LocalityLbEndpoints{}
			c.log.InfoContext(ctx, "service disappeared from registry; retaining empty cluster/vhost for grace period",
				"service", name, "grace", c.retentionGrace().String())
		} else if now.Sub(entry.absentSince) > c.retentionGrace() {
			c.log.InfoContext(ctx, "service absent past grace period; pruning", "service", name)
			continue
		}
		c.clusters[name] = entry
	}
}

// defaultServiceRetentionGrace is how long a service that vanished from the
// registry listing keeps its (empty) cluster and vhost before being pruned.
// Sized to outlast a rolling-restart churn window (~40s observed) with margin.
const defaultServiceRetentionGrace = 90 * time.Second

// SignalIfRetentionExpired emits a dependency-change signal when any retained
// (absent) service has outlived the retention grace, so the refresher runs a
// reload that prunes it. Pruning is otherwise reload-driven — and under
// demand-scoped watches a node in steady state receives NO events once a
// service leaves its dependency set, so the retained empty cluster/vhost
// would shadow the on-demand catch-all forever (fast 503 instead of ODCDS;
// observed in vivo 2026-06-12: svc-4's pods moved off a node and the stale
// vhost stuck). The refresher calls this from its periodic prune tick.
func (c *SnapshotCache) SignalIfRetentionExpired() {
	now := time.Now()
	grace := c.retentionGrace()

	c.clusterMu.RLock()
	expired := false
	for _, entry := range c.clusters {
		if !entry.absentSince.IsZero() && now.Sub(entry.absentSince) > grace {
			expired = true
			break
		}
	}
	c.clusterMu.RUnlock()

	if expired {
		c.log.Debug("retained service past grace in steady state; triggering prune reload")
		c.signalDependencyChange()
	}
}

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

// outboundVhostWithChainFilter builds a service's outbound vhost and enables its
// service-wide chain filter (025 M4) at the vhost — parity with the capture route
// table: both tables serve the same GAMMA/chain config.
func outboundVhostWithChainFilter(fqdn string, domains []string, rules []proxy.GammaRoute, chainFilters map[string]proxy.ExtensionFilter, serviceName string) *routev3.VirtualHost {
	vh := proxy.BuildOutboundServiceVirtualHost(fqdn, domains, rules)
	if ef, ok := chainFilters[serviceName]; ok {
		proxy.ApplyServiceChainFilter(vh, &ef)
	}
	return vh
}

// outboundPortVhostWithChainFilter is the per-advertised-port variant (the chain
// filter is service-wide: every port spelling carries it).
func outboundPortVhostWithChainFilter(portName string, chainFilters map[string]proxy.ExtensionFilter, serviceName string) *routev3.VirtualHost {
	vh := proxy.BuildOutboundClusterVirtualHost(portName, []string{portName})
	if ef, ok := chainFilters[serviceName]; ok {
		proxy.ApplyServiceChainFilter(vh, &ef)
	}
	return vh
}
