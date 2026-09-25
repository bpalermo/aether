package cache

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/registry"
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
		if entry.l4Floor {
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
	// c.clusters is a map, so the three slices above come out in a different
	// order on every call. vhosts is the protocol-visible one — it becomes the
	// aether_outbound RouteConfiguration's repeated virtual_hosts field, which
	// re-hashes (and makes Envoy rebuild the whole route table) on every push
	// unless the order is stable. See ordering.go.
	sortResourcesByName(clusters)
	sortResourcesByName(clas)
	sortVirtualHostsByName(vhosts)
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
	sortResourcesByName(resources)
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

	// UDP (datagram) services: the SAME pods, reached in PLAINTEXT at their
	// application UDP port by the udp_proxy capture listener. captureUDPClusters
	// builds the "udp:<svc>" cluster by rewriting the load assignment published
	// here, and udpClusterForLocked refuses to name a cluster the snapshot does
	// not carry -- so without this listing a UDP-only service has no entry, no
	// cluster, and therefore no listener at all.
	udpServiceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_UDP)
	if err != nil {
		return fmt.Errorf("failed to list UDP endpoints from registry: %w", err)
	}
	coldFillUDPEndpoints(ctx, c.log, reg, deps, serviceEndpoints, tcpServiceEndpoints, udpServiceEndpoints)

	c.log.DebugContext(ctx, "found service endpoints in registry",
		"count", len(serviceEndpoints), "tcpCount", len(tcpServiceEndpoints),
		"udpCount", len(udpServiceEndpoints), "dependencySet", len(deps))

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
	c.buildUDPClustersLocked(ctx, deps, udpServiceEndpoints, localRegion, localZone, waypoint)
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

	// Refresh each capture-TCP service's derived per-port set (proposal 037).
	// Derived from BOTH listings: a service whose pods split across protocols
	// appears in each, and either carries the full port_protocols map.
	//
	// This is a no-op unless a declared port set actually changed — endpoint
	// churn yields the same sorted slice — which is what keeps per-pod capture
	// listeners from being regenerated, and their connections drained, on every
	// pod ADD/DEL (Risk 4).
	derived := deriveTCPPorts(tcpServiceEndpoints)
	for svc, ports := range deriveTCPPorts(serviceEndpoints) {
		if _, have := derived[svc]; !have {
			derived[svc] = ports
		}
	}
	primary := make(map[string]uint32, len(tcpServiceEndpoints))
	for svc, eps := range tcpServiceEndpoints {
		if len(eps) > 0 {
			primary[svc] = eps[0].GetPort()
		}
	}
	c.refreshCaptureTCPPorts(derived, primary)

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
		cluster:        proxy.NewServiceCluster(fqdn, serviceName, serviceName, sortedKeys),
		loadAssignment: defaultCla,
		endpoints:      defaultEpMap,
		vhost:          outboundVhostWithChainFilter(fqdn, []string{fqdn, fmt.Sprintf("%s:%d", fqdn, defaultPort)}, gammaRoutes[serviceName], chainFilters, serviceName),
		sanNamespaces:  sanNamespaces,
		service:        serviceName,
		sni:            strconv.Itoa(int(defaultPort)),
	}

	c.buildPortAliasesLocked(serviceName, fqdn, defaultPort, sanNamespaces, sortedKeys, buckets)

	// One cluster per non-default advertised port.
	for port, b := range buckets {
		portName := proxy.PortClusterName(serviceName, c.meshDomain, port)
		pcla := proxy.NewClusterLoadAssignment(portName)
		pcla.Endpoints = b.eps
		proxy.SortLocalityLbEndpoints(pcla.Endpoints)
		c.clusters[portName] = clusterEntry{
			cluster:        proxy.NewServiceCluster(portName, portName, serviceName, sortedKeys),
			loadAssignment: pcla,
			endpoints:      b.epMap,
			vhost:          outboundPortVhostWithChainFilter(portName, chainFilters, serviceName),
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(port)),
		}
	}
}

// buildPortAliasesLocked stores the cold-path alias clusters for the ports an
// ODCDS ":authority" can legitimately carry for a service but that have no
// per-port cluster of their own: "<fqdn>:<port>". Caller must hold clusterMu.
//
// The ODCDS catch-all resolves its cluster from the request :authority
// (onDemandClusterHeader), and a client addressing a service on a non-80 port
// sends "<fqdn>:<port>" as the authority. For every NON-default advertised port
// that name is a real per-port cluster; for the two ports below it was nothing
// at all, so an on-demand request for them named a cluster the agent could
// never publish:
//
//   - The MESH port (meshconst.ProxyOutboundPort, 18081). This is the port every
//     mesh VIP Service advertises — the registrar's Service generator writes
//     exactly one ServicePort, MeshPort, on every generated Service — so it is
//     the port a client resolving through mesh DNS actually dials, and therefore
//     the authority the catch-all sees in production. It is NOT in the registry
//     endpoints at all: registryv1.ServiceEndpoint.port is the APPLICATION port
//     (endpoint.aether.io/port, projected onto the Service as the aether.io/port
//     annotation), which for the soak's echo is 8080 while the dialed Service
//     port is 18081. Deriving the alias from endpoints[0].GetPort() alone
//     therefore published ":8080" and left ":18081" — the name that actually
//     wedged for 5m10s on 2026-09-05 — unpublished on every node.
//   - The DEFAULT/application port, which the default entry's own vhost claims
//     as a host-match domain, so a client that dials it must find a cluster too.
//
// That is not merely a slow cold path, it is a permanent wedge (issue #682):
// go-control-plane only lists a name in a wildcard delta response's
// removed_resources if it had previously RETURNED it, so the agent never tells
// Envoy the name does not exist either. Envoy keeps it in its delta
// subscription state as "waiting for server" forever and dedupes every later
// on-demand subscribe for it, so once the service leaves the dependency set and
// its vhost disappears, no ODCDS request ever reaches the agent again: every
// request to that authority fails until the ADS stream resets.
//
// An alias shares the default cluster's bare-service EDS (same endpoints, same
// SAN pinning, same SNI) and carries NO vhost of its own: the default entry's
// vhost already claims the "<fqdn>:<defaultPort>" domain and the capture route
// domains already claim the mesh-port spelling, and a second vhost with either
// domain would be a duplicate-domain RDS reject. A port that has a real per-port
// cluster is skipped — that cluster is the authoritative, port-filtered EDS
// membership and must not be shadowed by a whole-service alias. Node proxies
// only: the edge serves an explicit exposed set with no ODCDS catch-all (see
// BuildEdgeRouteConfiguration).
func (c *SnapshotCache) buildPortAliasesLocked(serviceName, fqdn string, defaultPort uint32, sanNamespaces, sortedKeys []string, buckets map[uint32]*portBucket) {
	if c.edge || fqdn == "" {
		return
	}
	for _, port := range aliasAuthorityPorts(defaultPort, buckets) {
		alias := proxy.PortClusterName(serviceName, c.meshDomain, port)
		if alias == "" {
			continue
		}
		c.clusters[alias] = clusterEntry{
			// EDS resource name is the BARE service (not the alias): the alias is the
			// same endpoint set as the default cluster, so it must not publish a
			// second, duplicate load assignment.
			cluster:       proxy.NewServiceCluster(alias, serviceName, serviceName, sortedKeys),
			sanNamespaces: sanNamespaces,
			service:       serviceName,
			sni:           strconv.Itoa(int(port)),
		}
	}
}

// aliasAuthorityPorts returns the ports needing an alias cluster: the mesh VIP
// Service port every client dials through mesh DNS, plus the service's default
// (application) port, minus any port that already has its own per-port cluster
// and minus 0 (an endpoint with no port). Deterministic order — the mesh port
// first — so two rebuilds of the same input write the same entries (map order
// is protocol-visible, see #135).
func aliasAuthorityPorts(defaultPort uint32, buckets map[uint32]*portBucket) []uint32 {
	ports := make([]uint32, 0, 2)
	for _, p := range [...]uint32{meshconst.ProxyOutboundPort, defaultPort} {
		if p == 0 || slices.Contains(ports, p) {
			continue // 0 = unset; already emitted (the default port IS the mesh port)
		}
		if _, real := buckets[p]; real {
			continue // a real per-port cluster owns this name
		}
		ports = append(ports, p)
	}
	return ports
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
	// that EDS resource (by bare name) and pins peer identity from sanNamespaces.
	//
	// Keyed by the entry's own Envoy cluster name, "tcp:<fqdn>", NOT by the bare
	// service name (proposal 037 design (a)). The HTTP pass above writes
	// c.clusters[serviceName]; keying TCP entries there too made the two passes
	// collide, and the later TCP write CLOBBERED the service's h2 cluster,
	// outbound vhost and GAMMA cap_http vhost. That was invisible while a
	// service could only be one protocol, and became reachable the moment a
	// ServiceAccount could have pods declaring different protocols — which is
	// true on etcd (the CNI registers each pod under its own protocol key) and,
	// since #878, on the kubernetes backend too. Per-port and alias entries were
	// already keyed by cluster name; this brings TCP entries in line.
	for serviceName, endpoints := range tcpServiceEndpoints {
		if _, inScope := deps[serviceName]; !inScope {
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		sanNamespaces := endpointSANNamespaces(endpoints)
		defaultPort := endpoints[0].GetPort()
		cla, epMap := c.buildTCPEndpointsLocked(serviceName, endpoints, localRegion, localZone, waypoint)

		tcpName := proxy.TCPClusterName(serviceName, c.meshDomain)
		c.clusters[tcpName] = clusterEntry{
			loadAssignment: cla,
			endpoints:      epMap,
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(defaultPort)),
			l4Floor:        true,
		}

		c.buildTCPPortEntriesLocked(serviceName, tcpName, endpoints, defaultPort, sanNamespaces, localRegion, localZone, waypoint)
	}
}

// buildTCPPortEntriesLocked adds one entry per NON-PRIMARY raw-TCP port the
// service advertises (proposal 037). Caller must hold clusterMu.
//
// These exist because the capture listener emits a destination_port-qualified
// chain per such port, and that chain names tcp:<fqdn>:<port>. A chain whose
// cluster is absent from the snapshot does not fail loudly: tcp_proxy has no
// ODCDS cold path, so the connection is simply killed. Chain and cluster
// therefore have to be produced from the same derived facts in the same
// snapshot generation — proposal 037 Risk 1, and the same shape as #877.
//
// Each carries its OWN load assignment, named <fqdn>:<port> and filtered to the
// endpoints that advertise that port AS TCP. Sharing the bare-name EDS would
// put every pod of the service in the pool, including ones that do not serve
// the port at all — the per-port membership filter is what makes adding a port
// to a rolling Deployment safe, exactly as it already is for HTTP (proposal
// 005).
//
// entry.sni is the port, which refreshEntryMTLSLocked renders onto the upstream
// mTLS socket so the destination inbound can demux to the right loopback port.
// The primary port deliberately has no entry here: it is what the floor cluster
// already reaches, and the floor carries NO SNI on purpose (#306) so it lands on
// the destination's default inbound chain.
func (c *SnapshotCache) buildTCPPortEntriesLocked(
	serviceName, tcpName string,
	endpoints []*registryv1.ServiceEndpoint,
	defaultPort uint32,
	sanNamespaces []string,
	localRegion, localZone string,
	waypoint proxy.WaypointRewrite,
) {
	// The PRIMARY port gets an alias entry: same cluster name shape
	// (tcp:<fqdn>:<port>) so that a port-qualified reference resolves for EVERY
	// TCP port, not only the non-primary ones (proposal 037 Phase 3).
	//
	// Without it, a TCPRoute whose backendRef.port names the primary would
	// resolve to a cluster that does not exist, and tcp_proxy would kill those
	// connections silently — the same Risk 1 shape as a chain without a cluster.
	//
	// It carries NO load assignment of its own: it shares the floor's bare-name
	// EDS, exactly as the HTTP :<port> aliases share their default cluster's
	// (buildPortAliasesLocked). And its sni stays EMPTY, because it addresses
	// the primary port — the destination's default inbound floor chain is what
	// serves it, and a non-empty SNI would route it to a per-port chain that
	// does not exist (#306).
	c.clusters[proxy.TCPPortClusterName(tcpName, defaultPort)] = clusterEntry{
		sanNamespaces: sanNamespaces,
		service:       serviceName,
		sni:           "",
		l4Floor:       true,
	}

	for _, port := range nonPrimaryTCPPorts(endpoints) {
		if port == defaultPort {
			continue
		}
		members := make([]*registryv1.ServiceEndpoint, 0, len(endpoints))
		for _, ep := range endpoints {
			if ep.GetPortProtocols()[port] == registryv1.PortProtocol_PORT_PROTOCOL_TCP {
				members = append(members, ep)
			}
		}
		if len(members) == 0 {
			continue
		}

		claName := proxy.PortClusterName(serviceName, c.meshDomain, port)
		portCla := proxy.NewClusterLoadAssignment(claName)
		epMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(members))
		for _, ep := range members {
			lbEp := proxy.ServiceLocalityLbEndpointFromRegistryEndpoint(ep, localRegion, localZone, waypoint)
			portCla.Endpoints = append(portCla.Endpoints, lbEp)
			epMap[ep.GetIp()] = lbEp
		}
		proxy.SortLocalityLbEndpoints(portCla.Endpoints)

		c.clusters[proxy.TCPPortClusterName(tcpName, port)] = clusterEntry{
			loadAssignment: portCla,
			endpoints:      epMap,
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(port)),
			l4Floor:        true,
		}
	}
}

// buildTCPEndpointsLocked builds a TCP service's endpoint map and, when this
// entry owns it, its bare-name load assignment. Caller must hold clusterMu.
//
// Bare-name CLA ownership: both an HTTP default entry and a TCP floor entry for
// the same service reference a load assignment named <serviceName>, and two EDS
// resources with one name is a snapshot-consistency error in go-control-plane
// (or a silent last-writer-wins). The HTTP entry owns it when the service has
// one; the TCP entry then references it by name and carries no load assignment
// of its own, exactly as the :<port> aliases do — clustersEndpointsAndVhosts
// already guards on loadAssignment != nil.
//
// An HTTP entry with no CLA of its own (the retained-absent alias shape) does
// not own one either, so the TCP entry takes ownership rather than leave the
// bare EDS name unpublished: the floor cluster resolves through it, and
// tcp_proxy has no ODCDS cold path to recover from a missing one.
func (c *SnapshotCache) buildTCPEndpointsLocked(
	serviceName string,
	endpoints []*registryv1.ServiceEndpoint,
	localRegion, localZone string,
	waypoint proxy.WaypointRewrite,
) (*endpointv3.ClusterLoadAssignment, map[string]*endpointv3.LocalityLbEndpoints) {
	httpEntry, httpOwnsCLA := c.clusters[serviceName]
	owns := !httpOwnsCLA || httpEntry.loadAssignment == nil

	var cla *endpointv3.ClusterLoadAssignment
	if owns {
		cla = proxy.NewClusterLoadAssignment(serviceName)
	}
	epMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(endpoints))
	for _, endpoint := range endpoints {
		lbEp := proxy.ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint, localRegion, localZone, waypoint)
		if cla != nil {
			cla.Endpoints = append(cla.Endpoints, lbEp)
		}
		epMap[endpoint.GetIp()] = lbEp
	}
	if cla != nil {
		proxy.SortLocalityLbEndpoints(cla.Endpoints)
	}
	return cla, epMap
}

// buildUDPClustersLocked adds the cluster entries for PROTOCOL_UDP services.
// Caller must hold clusterMu.
//
// The UDP arm of buildTCPClustersLocked, and deliberately thinner. A UDP entry
// carries only the bare-name EDS load assignment and the service's default
// application port (in sni, the same overload the TCP floor uses) because that
// is all captureUDPClusters needs: it rewrites the load assignment to the app
// port with SocketAddress_UDP and wraps it in a STATIC, transport-socket-less
// cluster.
//
// No sanNamespaces, and no per-port entries. Both would be lies: the UDP floor
// is plaintext, so there is no peer identity to pin, and it has no
// destination_port-qualified chains to name a per-port cluster from -- a
// connection-less UDP listener carries no filter chains at all.
func (c *SnapshotCache) buildUDPClustersLocked(ctx context.Context, deps map[string]struct{}, udpServiceEndpoints map[string][]*registryv1.ServiceEndpoint, localRegion, localZone string, waypoint proxy.WaypointRewrite) {
	for serviceName, endpoints := range udpServiceEndpoints {
		if _, inScope := deps[serviceName]; !inScope {
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		cla, epMap := c.buildTCPEndpointsLocked(serviceName, endpoints, localRegion, localZone, waypoint)

		udpName := proxy.UDPClusterName(serviceName, c.meshDomain)
		c.clusters[udpName] = clusterEntry{
			loadAssignment: cla,
			endpoints:      epMap,
			service:        serviceName,
			sni:            strconv.Itoa(int(endpoints[0].GetPort())),
			l4Floor:        true,
		}
		c.log.DebugContext(ctx, "built UDP floor cluster entry", "service", serviceName, "cluster", udpName)
	}
}

// tcpEntryLocked returns the TCP floor entry for a bare service name, which since
// proposal 037 design (a) is keyed by the entry's Envoy cluster name rather than
// by the service. Caller must hold clusterMu.
func (c *SnapshotCache) tcpEntryLocked(serviceName string) (clusterEntry, bool) {
	entry, ok := c.clusters[proxy.TCPClusterName(serviceName, c.meshDomain)]
	return entry, ok
}

// udpEntryLocked returns the UDP floor entry for a bare service name, keyed by
// the entry's Envoy cluster name like the TCP one. Caller must hold clusterMu.
func (c *SnapshotCache) udpEntryLocked(serviceName string) (clusterEntry, bool) {
	entry, ok := c.clusters[proxy.UDPClusterName(serviceName, c.meshDomain)]
	return entry, ok
}

// coldFillUDPEndpoints performs the RPC-fill cold path for UDP services: a
// dependency that is in neither the HTTP nor the TCP listing and not yet in the
// UDP one is fetched directly, so the FIRST reload after an observation can
// build the cluster instead of waiting a watch round-trip.
func coldFillUDPEndpoints(ctx context.Context, log interface {
	InfoContext(context.Context, string, ...any)
}, reg registry.Registry, deps map[string]struct{}, serviceEndpoints, tcpServiceEndpoints, udpServiceEndpoints map[string][]*registryv1.ServiceEndpoint,
) {
	cat, ok := reg.(registry.ServiceCatalog)
	if !ok {
		return
	}
	for svc := range deps {
		if _, have := udpServiceEndpoints[svc]; have {
			continue
		}
		if _, have := serviceEndpoints[svc]; have {
			continue // already an HTTP dependency
		}
		if _, have := tcpServiceEndpoints[svc]; have {
			continue // already a TCP dependency
		}
		if !cat.HasService(svc) {
			continue
		}
		eps, err := reg.ListEndpoints(ctx, svc, registryv1.Service_PROTOCOL_UDP)
		if err != nil {
			log.InfoContext(ctx, "cold-path UDP endpoint fetch failed; watch catch-up will fill in", "service", svc, "error", err.Error())
			continue
		}
		if len(eps) > 0 {
			udpServiceEndpoints[svc] = eps
		}
	}
}

// serviceEntryLocked returns the entry carrying a service's service-level facts
// (sni, sanURIs, the bare-name load assignment) for callers that do not care
// which protocol classified it: the HTTP default entry when the service has one,
// else its TCP floor entry, else its UDP one. Caller must hold clusterMu.
//
// The order is a preference, not a priority: a service registered under several
// protocols has the SAME endpoints under each, and only one entry owns the
// bare-name load assignment (see buildTCPEndpointsLocked's `owns`). The UDP
// fallback exists for the service registered ONLY as PROTOCOL_UDP, which before
// #931 could not be expressed at all.
func (c *SnapshotCache) serviceEntryLocked(serviceName string) (clusterEntry, bool) {
	if entry, ok := c.clusters[serviceName]; ok {
		return entry, true
	}
	if entry, ok := c.tcpEntryLocked(serviceName); ok {
		return entry, true
	}
	return c.udpEntryLocked(serviceName)
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
			// The default-port alias publishes no load assignment of its own (it
			// shares the default cluster's bare-service EDS); synthesizing an empty
			// one here would emit an orphan CLA under the alias name that nothing
			// references.
			if entry.loadAssignment != nil {
				// Name the replacement after the load assignment it replaces, NOT
				// after the map key. They coincide for HTTP and per-port entries,
				// but a TCP floor entry is keyed "tcp:<fqdn>" while publishing the
				// BARE-name EDS resource its floor cluster resolves through
				// (NewTCPServiceCluster's EdsClusterConfig.ServiceName). Keying off
				// `name` there would retain an empty CLA under a name nothing
				// references and leave the real one unpublished.
				entry.loadAssignment = proxy.NewClusterLoadAssignment(entry.loadAssignment.GetClusterName())
				entry.endpoints = map[string]*endpointv3.LocalityLbEndpoints{}
			}
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
