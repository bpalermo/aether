package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
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

// clustersEndpointsVhostsAndPinsInto returns all cached cluster, endpoint, and virtual
// host resources as separate slices, and the pin report. It returns the concrete vhost type to avoid
// boxing/unboxing at the caller. Each service cluster speaks per-source mTLS to
// the destination node; the upstream transport-socket matcher (selecting the
// source pod's certificate by its network namespace) is precomputed into
// entry.mtlsCluster at entry build/invalidation time (refreshEntryMTLSLocked),
// so snapshot generation only reads the cached proto (issue #537). Before the
// node SVID is served mtlsCluster is nil and the base cluster is emitted
// without the matcher.
//
// The pin report comes from the SAME read of the cluster map (#1425). Its
// classes are filed under the versions of the clusters these resources become,
// and an ACK of a cluster at that version is read as "the proxy accepted a
// cluster with this pin state". Taken in a later read, a registry reload or an
// identity change that landed in between would put the pin state of a cluster
// the proxy was never sent under that version.
//
// The report's classes are collected into buf and its mTLS cluster names into
// mtlsBuf (each from its start), so a snapshot build can reuse the two buffers
// across builds; nil for either allocates.
func (c *SnapshotCache) clustersEndpointsVhostsAndPinsInto(buf []entryClass, mtlsBuf []string) ([]types.Resource, []types.Resource, []*routev3.VirtualHost, pinReport) {
	// The east-west QUIC fan-out inputs, snapshotted BEFORE clusterMu; their
	// own locks (depMu, localMu) never nest inside it (see mtls.go's lock order).
	quic := c.quicFanoutSnapshot()
	// One derived-object generation per snapshot (#1115, derivedmemo.go).
	c.derived.rotate()
	quic.aliasCLA = c.derived.aliasCLA
	quic.twinCluster = c.derived.twinCluster
	// What the out_http virtual hosts built here carry besides a cluster,
	// read before clusterMu like the fan-out inputs.
	// The generation is read BEFORE the inputs: an event that lands in between
	// leaves virtual hosts newer than their generation, and the next build
	// rebuilds them once more, which costs one rebuild and never keeps a
	// stale one.
	c.depMu.RLock()
	ruleGen := c.ruleGen
	c.depMu.RUnlock()
	rules := ruleInputs{routes: c.serviceRoutesSnapshot(), chainFilters: c.serviceChainFiltersSnapshot(), portMaps: c.servicePortMapsSnapshot(), declared: c.declaredByService()}
	c.refreshEntryVhosts(ruleGen, rules)

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	rules.ports = effectivePorts(c.servicePorts, rules.declared)

	clusters := make([]types.Resource, 0, len(c.clusters))
	clas := make([]types.Resource, 0, len(c.clusters))
	vhosts := make([]*routev3.VirtualHost, 0, len(c.clusters))
	quicClusters := 0
	pins := pinReport{track: true, classes: buf[:0], mtls: mtlsBuf[:0]}
	for key, entry := range c.clusters {
		pins.add(key, &entry)
		if entry.l4Floor {
			// TCP/UDP floor entries publish their load assignment only; their
			// clusters are rendered by captureTCPClusters / captureUDPClusters.
			clas = c.appendEntryCLAsLocked(clas, entry)
			continue
		}
		// Precomputed mTLS-injected cluster when the node SVID is available
		// (refreshEntryMTLSLocked); the bare cluster otherwise.
		var cluster types.Resource = entry.cluster
		if entry.mtlsCluster != nil {
			cluster = entry.mtlsCluster
			// By the name of the resource this pass publishes, not by the
			// entry's key (pinReport.mtls).
			pins.mtls = append(pins.mtls, entry.mtlsCluster.GetName())
		}
		clusters = append(clusters, cluster)
		clas = c.appendEntryCLAsLocked(clas, entry)
		twins, twinCLAs, vhost := quic.entryTwinsAndVhost(key, c.withBareCLALocked(key, entry), c.meshDomain)
		clusters = append(clusters, twins...)
		clas = append(clas, twinCLAs...)
		quicClusters += len(twins)
		// A virtual host with no domain claims no authority (a pod port
		// behind a user Service that has no port for it): not in the table.
		if vhost != nil && len(vhost.GetDomains()) > 0 {
			vhosts = append(vhosts, vhost)
		}
	}
	// The authorities out_http refuses by name (proposal 043): a mesh service
	// addressed on a port it does not serve as HTTP.
	vhosts = append(vhosts, c.outboundRefusalVhostsLocked(rules)...)
	// And the services with a service-wide HTTP filter that have no cluster
	// here yet: their own virtual host on the on-demand route.
	vhosts = append(vhosts, c.outboundColdVhostsLocked(rules)...)
	// The services this node holds no cluster of whose generated Service says
	// what their ports are: routed and refused by those ports, like a
	// service it knows.
	vhosts = append(vhosts, c.outboundDeclaredVhostsLocked(rules)...)
	c.noteQUICFanout(quicClusters, len(quic.pairs), len(quic.identities), len(quic.awaitingCert))
	sortResourcesByName(clusters)
	sortResourcesByName(clas)
	sortVirtualHostsByName(vhosts)
	pins.sortNames()
	return clusters, clas, vhosts, pins
}

// appendEntryCLAsLocked appends the load assignment(s) one cluster-cache entry
// publishes: its own, if it holds one, and -- for a port alias, which
// subscribes to its own EDS name -- the bare service's load assignment
// republished under that name, in THIS snapshot (aether#1013). Caller must
// hold clusterMu.
func (c *SnapshotCache) appendEntryCLAsLocked(clas []types.Resource, entry clusterEntry) []types.Resource {
	if entry.loadAssignment != nil {
		clas = append(clas, entry.loadAssignment)
	}
	if entry.bareEDSAlias && entry.cluster != nil {
		if cla := c.derived.aliasCLA(c.bareServiceCLALocked(entry.service), edsServiceName(entry.cluster)); cla != nil {
			clas = append(clas, cla)
		}
	}
	return clas
}

// bareServiceCLALocked returns the load assignment published under a service's
// BARE EDS resource name (the default HTTP cluster's subscription). Exactly one
// entry holds it (#1635): the HTTP default entry's when it owns one, otherwise
// the TCP floor entry's, otherwise the UDP floor entry's (ownsBareCLALocked for
// entries built from a live listing, retainAbsentClustersLocked for retained
// ones), so the order below finds the one holder, it does not choose between
// several. Nil when nothing publishes it. The clusters that must see the same membership
// without sharing the subscription -- port aliases and TCP floors -- republish
// it under their own names (proxy.LoadAssignmentAlias, aether#1013). Caller
// must hold clusterMu.
func (c *SnapshotCache) bareServiceCLALocked(serviceName string) *endpointv3.ClusterLoadAssignment {
	holder, _ := c.bareServiceHolderLocked(serviceName)
	return holder.loadAssignment
}

// bareServiceHolderLocked returns the one entry that holds a service's
// bare-name load assignment (see bareServiceCLALocked), and false when none
// does. A reader that needs more than the endpoints -- the udp: cluster dials
// the application port the entry carries in sni -- takes BOTH from this entry:
// two listings of one service need not register the same port, so endpoints
// from the holder with the port of another entry (a retained one, say) is a
// live pod dialed at a port it does not serve (#1635). Caller must hold
// clusterMu.
func (c *SnapshotCache) bareServiceHolderLocked(serviceName string) (clusterEntry, bool) {
	if e, ok := c.clusters[serviceName]; ok && e.loadAssignment != nil {
		return e, true
	}
	if e, ok := c.tcpEntryLocked(serviceName); ok && e.loadAssignment != nil {
		return e, true
	}
	if e, ok := c.udpEntryLocked(serviceName); ok && e.loadAssignment != nil {
		return e, true
	}
	return clusterEntry{}, false
}

// withBareCLALocked returns a default HTTP entry (keyed by the bare service
// name) with the load assignment its cluster's EDS subscription resolves to:
// its own, or -- when it is retained and a live floor entry publishes the bare
// name (retainAbsentClustersLocked, #1635) -- that entry's. For what is DERIVED
// from the default cluster's membership (the QUIC twins' own-name copies); the
// entry's own publication reads entry.loadAssignment, so the bare name is not
// published twice. Any other entry is returned as is. Caller must hold
// clusterMu.
func (c *SnapshotCache) withBareCLALocked(key string, entry clusterEntry) clusterEntry {
	if entry.loadAssignment == nil && key == entry.service {
		entry.loadAssignment = c.bareServiceCLALocked(entry.service)
	}
	return entry
}

// edsServiceName is the EDS resource name a cluster subscribes to: its
// eds_cluster_config.service_name, or the cluster name when that is unset.
func edsServiceName(cl *clusterv3.Cluster) string {
	if n := cl.GetEdsClusterConfig().GetServiceName(); n != "" {
		return n
	}
	return cl.GetName()
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

	// Every row read below is a typed one: a row that does not say the
	// protocol of each of its ports contributes nothing (typedRegistry).
	reg, typed := typedRowsOf(reg)
	defer func() { c.reportRefusedRows(ctx, typed.refused) }()

	serviceEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	if err != nil {
		return fmt.Errorf("failed to list endpoints from registry: %w", err)
	}

	deps := c.DependencySet()
	// The generation of the rule inputs, read BEFORE them:
	// the virtual hosts this refresh builds are recorded as built at it.
	c.depMu.RLock()
	loadRuleGen := c.ruleGen
	c.depMu.RUnlock()
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
	// — holding only the load assignment (no HTTP h2 cluster/vhost). A service
	// can be in both listings (pods of one ServiceAccount that declare
	// different protocols): the TCP entry is keyed by its own cluster name and
	// one of the two owns the bare-name load assignment (ownsBareCLALocked).
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

	// What each listed service's ports are and what speaks on each (proposal
	// 043): derived once per refresh from every listing, before the passes
	// below, because a GAMMA backendRef of one service resolves to a port of
	// ANOTHER service.
	ports := deriveAllServicePorts(serviceEndpoints, tcpServiceEndpoints, udpServiceEndpoints)
	listed := make(map[string]struct{}, len(ports))
	for svc := range ports {
		listed[svc] = struct{}{}
	}
	portMaps := c.servicePortMapsSnapshot()
	// What the generated Services say of the ports of the services this node
	// holds no endpoint of: a rule's backendRef to such a service is judged by
	// them here exactly as in the capture tables and in a snapshot build
	// (effectivePorts). Without them this refresh, and the rebuild of the
	// entries' virtual hosts on a rule event, took a cold backend's port on
	// trust: the loopback table parked a backendRef to a port the backend
	// does not declare (503 after the on-demand timeout) where the capture
	// table of the same snapshot answered 500.
	declared := c.declaredByService()

	c.clusterMu.Lock()
	// A service that left every listing keeps what was last derived for it
	// for as long as it keeps a (retained) cluster entry: see
	// pruneServicePortsLocked.
	prevPorts := c.servicePorts
	for svc, last := range prevPorts {
		if _, ok := ports[svc]; !ok {
			ports[svc] = last
		}
	}
	c.servicePorts = ports
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
	// A service whose builder inputs did not change keeps the entries -- the
	// very proto objects -- of the previous refresh, so the snapshot's version
	// memo does not re-marshal and re-hash them (#1115, registryreuse.go).
	in := reuseInputs{localRegion: localRegion, localZone: localZone, waypoint: waypoint}
	reuse := c.beginRegistryReuseLocked()
	rules := ruleInputs{routes: gammaRoutes, chainFilters: chainFilters, portMaps: portMaps, declared: declared}
	rules.ports = effectivePorts(c.servicePorts, declared)
	c.buildHTTPClustersLocked(deps, serviceEndpoints, rules, in, nodeSubsetKeys, prev, reuse)
	c.buildTCPClustersLocked(deps, tcpServiceEndpoints, in, prev, reuse)
	c.buildUDPClustersLocked(ctx, deps, udpServiceEndpoints, in, prev, reuse)
	c.commitRegistryReuseLocked(reuse)
	c.retainAbsentClustersLocked(ctx, prev, deps, retentionEvidence{listed: listed, declared: declared})
	// Every virtual host built or refreshed above came from the rule inputs
	// read at loadRuleGen. If a newer generation exists (an event landed while
	// the listings were being read), the next snapshot build must rebuild
	// them: say which generation they are at, never leave a newer one standing.
	c.vhostRuleGen = loadRuleGen
	c.pruneServicePortsLocked(listed)
	c.noteServicePortsLocked(prevPorts)
	conflicts := c.servicePortConflictsLocked()
	derived := make(map[string]servicePorts, len(c.servicePorts))
	for svc, p := range c.servicePorts {
		derived[svc] = p
	}
	// Precompute each entry's mTLS-injected cluster + SAN URIs (issue #537) so
	// snapshot generation only reads the cached protos. Covers the freshly
	// built entries AND the retained (grace-period) ones, under the same
	// clusterMu hold that rebuilt the map.
	c.recomputeMTLSClustersLocked()
	// Counted under the lock: the log below used to read len(c.clusters)
	// after Unlock, racing a concurrent recomputeMTLSClusters (AddPod).
	loaded := len(c.clusters)
	reusedN, builtN := c.registryReuse.reused, c.registryReuse.built
	c.clusterMu.Unlock()

	// Publish the node-wide subset-key union as the shared ECDS mapping.
	c.subsetMu.Lock()
	c.subsetHeaderKeys = proxy.SortSubsetKeys(nodeSubsetKeys)
	c.subsetMu.Unlock()

	c.reportServicePortConflicts(ctx, conflicts)

	// Hand the derivation to the capture side, which builds the listener's
	// chains from it (proposals 037 and 043).
	//
	// This is a no-op unless a service's declared ports actually changed —
	// endpoint churn yields the same derivation — which is what keeps per-pod
	// capture listeners from being regenerated, and their connections drained,
	// on every pod ADD/DEL (Risk 4).
	c.refreshCaptureServicePorts(derived)

	c.log.DebugContext(ctx, "loaded clusters from registry", "count", loaded,
		"servicesReused", reusedN, "servicesBuilt", builtN)

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
// serviceEndpoints, reusing a service's previous entries when its inputs are
// unchanged (#1115). Caller must hold clusterMu.
func (c *SnapshotCache) buildHTTPClustersLocked(deps map[string]struct{}, serviceEndpoints map[string][]*registryv1.ServiceEndpoint, rules ruleInputs, in reuseInputs, nodeSubsetKeys map[string]struct{}, prev map[string]clusterEntry, reuse map[string]reuseRecord) {
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
		in := in
		in.ports = c.servicePorts[serviceName]
		build := func(dst map[string]clusterEntry, subsetKeys map[string]struct{}) {
			c.buildHTTPServiceEntryLocked(dst, serviceName, endpoints, rules, in, subsetKeys)
		}
		refresh := func(names []string) {
			c.refreshReusedVhostsLocked(names, serviceName, rules)
		}
		keys, _ := c.reuseOrBuildLocked(reusePassHTTP, serviceName, endpoints, false, in, prev, reuse, build, refresh)
		for _, k := range keys {
			nodeSubsetKeys[k] = struct{}{}
		}
	}
}

// refreshReusedVhostsLocked rebuilds the outbound vhosts of a service's reused
// HTTP entries from the CURRENT GAMMA rules, chain filters and port maps, which
// are not part of the reuse key (#1115): a vhost is not an xDS resource of its
// own (it is embedded in out_http, which every snapshot rebuilds), so
// rebuilding it costs no version and keeps it exact. The default entry is
// keyed by the bare service name; every other entry carrying a vhost is a
// per-port one, keyed by its cluster name. Port aliases carry none. Caller
// must hold clusterMu.
//
// An entry's port is read back from the entry (its sni is the port, set when
// it was built from these same rows) rather than decided again from the
// endpoints: this runs for every reused service on every refresh, and the
// point of reuse is that a refresh does not walk the rows of a service that
// did not change.
func (c *SnapshotCache) refreshReusedVhostsLocked(names []string, serviceName string, rules ruleInputs) {
	for _, name := range names {
		e := c.clusters[name]
		if e.vhost == nil {
			continue
		}
		e.vhost = c.outboundPortVhostLocked(serviceName, name, primaryPortOf(e), name == serviceName, rules)
		c.clusters[name] = e
	}
}

// refreshEntryVhosts rebuilds the loopback virtual host of every cluster entry
// when the rule inputs changed since they were last built (bumpRuleGenLocked),
// so that a snapshot's loopback table and its capture tables are built from
// the same rules, filters and port maps. It does nothing, and takes only the
// read lock, when the generation is the one the virtual hosts already carry:
// a snapshot is built on every pod event, and the stored virtual hosts are
// what keeps the route table's bytes from being rebuilt and hashed again
// (#1105, #1115).
//
// An entry that publishes no virtual host (an alias, a retained entry that
// yielded its authorities, an entry whose port the service does not serve as
// HTTP) stays without one: which entries have one is the registry refresh's
// decision.
func (c *SnapshotCache) refreshEntryVhosts(ruleGen uint64, rules ruleInputs) {
	c.clusterMu.RLock()
	current := c.vhostRuleGen == ruleGen
	c.clusterMu.RUnlock()
	if current {
		return
	}
	c.clusterMu.Lock()
	defer c.clusterMu.Unlock()
	if c.vhostRuleGen == ruleGen {
		return
	}
	rules.ports = effectivePorts(c.servicePorts, rules.declared)
	for name, e := range c.clusters {
		if e.vhost == nil || e.l4Floor {
			continue
		}
		e.vhost = c.outboundPortVhostLocked(e.service, name, primaryPortOf(e), name == e.service, rules)
		c.clusters[name] = e
	}
	c.vhostRuleGen = ruleGen
}

// buildHTTPServiceEntryLocked builds the cluster entries for one service in the
// HTTP listing into dst: one cluster per port the service serves AS HTTP, and
// the port aliases of the default one. Every input it reads is either in the
// #1115 reuse key (registryreuse.go) or re-derived on reuse (the vhosts); keep
// it that way when adding one. Caller must hold clusterMu.
//
// Membership is by (port, class): a port an endpoint declares raw TCP or UDP
// gets no cluster here, so no h2 cluster is built for a port that cannot
// answer one (proposal 043, Phase 1).
//
// The DEFAULT cluster, "<fqdn>" with the bare service's EDS name, is the
// cluster of the service's legacy target, and exists only when that target is
// an HTTP port (legacyTarget). It is what the legacy spellings reach, what the
// QUIC twins are built from, and the cluster of that port number for every
// spelling. A service whose legacy target is NOT HTTP gets no default cluster
// (#1672): "<fqdn>" and "<fqdn>:18081" aimed at a raw-TCP port produced an h2
// cluster whose every request was a 502. Such a service is in this listing
// only because it also serves an HTTP port, and that port gets its per-port
// cluster like any other. Its bare-name load assignment is then owned by its
// TCP floor entry (ownsBareCLALocked), and the route tables refuse the legacy
// spellings by name.
//
// The legacy target and the class of each port are the service's, decided
// over every listing it is in (in.ports), not over this listing's rows: the
// route tables are built from the same answer, so a cluster exists exactly
// where a route leads. They are part of the #1115 reuse key for that reason.
func (c *SnapshotCache) buildHTTPServiceEntryLocked(dst map[string]clusterEntry, serviceName string, endpoints []*registryv1.ServiceEndpoint, rules ruleInputs, in reuseInputs, nodeSubsetKeys map[string]struct{}) {
	// The outbound service cluster speaks per-source mTLS HTTP/2 to each
	// destination pod's mesh inbound (pod_ip:18008). The per-source mTLS transport
	// socket is precomputed into entry.mtlsCluster below (recomputeMTLSClustersLocked).
	// Server-identity pinning: the union of the endpoints' namespaces
	// renders the service's expected SPIFFE IDs there too.
	sanNamespaces := endpointSANNamespaces(endpoints)
	sortedKeys := endpointSubsetKeys(endpoints, nodeSubsetKeys)

	fqdn := proxy.ServiceClusterName(serviceName, c.meshDomain)
	defaultPort, defaultClass := in.ports.legacyPort, in.ports.legacyClass
	// Rows that carry no port at all decide nothing; such a service keeps the
	// default cluster it always had. Rows whose declared port is one of the
	// mesh's own (reservedMeshPort) have no legacy target either, and are a
	// different case: there is no port for a default cluster to be the
	// cluster of, so there is none, and the service is its other ports.
	defaultIsHTTP := defaultClass == registryv1.PortProtocol_PORT_PROTOCOL_HTTP ||
		(defaultPort == 0 && !in.ports.known())

	defaultCla, defaultEpMap, buckets := buildHTTPEndpointBuckets(serviceName, endpoints, in, defaultPort, defaultIsHTTP)

	if defaultIsHTTP {
		dst[serviceName] = clusterEntry{
			cluster:        proxy.NewServiceCluster(fqdn, serviceName, serviceName, sortedKeys),
			loadAssignment: defaultCla,
			endpoints:      defaultEpMap,
			vhost:          c.outboundPortVhostLocked(serviceName, serviceName, defaultPort, true, rules),
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(defaultPort)),
		}

		c.buildPortAliasesLocked(dst, serviceName, fqdn, defaultPort, sanNamespaces, sortedKeys, buckets)
	}

	// One cluster per other advertised HTTP port.
	for port, b := range buckets {
		portName := proxy.PortClusterName(serviceName, c.meshDomain, port)
		pcla := proxy.NewClusterLoadAssignment(portName)
		pcla.Endpoints = b.eps
		proxy.SortLocalityLbEndpoints(pcla.Endpoints)
		dst[portName] = clusterEntry{
			cluster:        proxy.NewServiceCluster(portName, portName, serviceName, sortedKeys),
			loadAssignment: pcla,
			endpoints:      b.epMap,
			vhost:          c.outboundPortVhostLocked(serviceName, portName, port, false, rules),
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
// An alias has the default cluster's membership (same endpoints, same SAN
// pinning, same SNI) but NOT its EDS subscription: it subscribes to its own
// name, and clustersEndpointsAndVhosts republishes the bare service's load
// assignment under it in the same snapshot (aether#1013). Sharing the bare name
// was only accidentally safe: Envoy's delta-ADS WatchMap deduplicates
// subscription interest per resource name, so an alias added in a LATER CDS
// update than the default cluster (a Service gaining a port after the node
// depends on it) sent no subscribe, got no EDS, and warmed for the full 15 s
// initial_fetch_timeout -- the #1008 mechanism. An alias carries NO vhost of
// its own: the default entry's
// vhost already claims the "<fqdn>:<defaultPort>" domain and the capture route
// domains already claim the mesh-port spelling, and a second vhost with either
// domain would be a duplicate-domain RDS reject. A port that has a real per-port
// cluster is skipped — that cluster is the authoritative, port-filtered EDS
// membership and must not be shadowed by a whole-service alias. Node proxies
// only: the edge serves an explicit exposed set with no ODCDS catch-all (see
// BuildEdgeRouteConfiguration).
func (c *SnapshotCache) buildPortAliasesLocked(dst map[string]clusterEntry, serviceName, fqdn string, defaultPort uint32, sanNamespaces, sortedKeys []string, buckets map[uint32]*portBucket) {
	if c.edge || fqdn == "" {
		return
	}
	for _, port := range aliasAuthorityPorts(defaultPort, buckets) {
		alias := proxy.PortClusterName(serviceName, c.meshDomain, port)
		if alias == "" {
			continue
		}
		dst[alias] = clusterEntry{
			// EDS resource name is the ALIAS, never the bare service: the load
			// assignment under it is derived from the bare one at snapshot time
			// (bareEDSAlias), so the alias tracks every endpoint change of the
			// default cluster without a second copy to keep in sync here.
			cluster:       proxy.NewServiceCluster(alias, alias, serviceName, sortedKeys),
			sanNamespaces: sanNamespaces,
			service:       serviceName,
			sni:           strconv.Itoa(int(port)),
			bareEDSAlias:  true,
		}
	}
}

// aliasAuthorityPorts returns the ports needing an alias cluster: the
// service's default (application) port, unless it already has its own per-port
// cluster or is 0 (an endpoint with no port).
//
// The mesh's HTTP proxy port (18081) had an alias here until proposal 043: it
// was the port every client dialled, and "<fqdn>:18081" meant the port the
// pods declare. It is a port of no service now, so no cluster carries that
// name and nothing that names it can be delivered, including a request that
// was parked on demand for a name the node learns of meanwhile. That such a
// request is never answered by a cluster is the point, not the wedge of #682:
// the wedge was a name that SHOULD have resolved and never could.
func aliasAuthorityPorts(defaultPort uint32, buckets map[uint32]*portBucket) []uint32 {
	if defaultPort == 0 || reservedMeshPort(defaultPort) {
		return nil
	}
	if _, real := buckets[defaultPort]; real {
		return nil // a real per-port cluster owns this name
	}
	return []uint32{defaultPort}
}

// hasOwnHTTPCluster reports whether endpoint is a member of an h2 cluster of
// port p: it serves p as HTTP, and so does the SERVICE.
//
// A port the service does not serve as HTTP (its pods disagree, and most
// declare it raw TCP or UDP) gets no h2 cluster from the pods that do. No
// route leads to one: the service's answer for the port is the other class,
// and its HTTP authority is refused. But a cluster that exists can be asked
// for by name, and it was: a request for "<fqdn>:<port>" parked while the node
// knew nothing of the service was delivered to those pods when the cluster
// arrived, where every later request is refused.
func hasOwnHTTPCluster(endpoint *registryv1.ServiceEndpoint, p uint32, ports servicePorts) bool {
	return servesPortAsHTTP(endpoint, p) && (!ports.known() || ports.hasHTTP(p))
}

// portBucket accumulates per-port endpoints for a service.
type portBucket struct {
	eps   []*endpointv3.LocalityLbEndpoints
	epMap map[string]*endpointv3.LocalityLbEndpoints
}

// listsPort reports whether an endpoint row serves port at all, in any class:
// its port_protocols names the port, with a class. The row's port and ports
// fields alone do not make a port served: a port with no stated class has
// none (port_protocols is required, endpointmeta.RowProblem).
func listsPort(endpoint *registryv1.ServiceEndpoint, port uint32) bool {
	if reservedMeshPort(port) {
		return false // a port of no service (reservedMeshPort)
	}
	return endpoint.GetPortProtocols()[port] != registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED
}

// servesPortAsHTTP reports whether an endpoint serves port as HTTP: its
// port_protocols says so. A port the map does not name is not HTTP, whatever
// listing the row was read from (it used to take the listing's class).
//
// A pod of the service that does not list the port is NOT a member of that
// port's cluster, whatever class the service's other pods give the port: its
// inbound forwards to the port it declares, so a request balanced onto it
// would be answered by another port (a rollout that moves the declared port
// from 8080 to 9090 put the new pod in the cluster of 8080).
func servesPortAsHTTP(endpoint *registryv1.ServiceEndpoint, port uint32) bool {
	return !reservedMeshPort(port) && endpoint.GetPortProtocols()[port] == registryv1.PortProtocol_PORT_PROTOCOL_HTTP
}

// buildHTTPEndpointBuckets builds the default CLA/epMap and per-port buckets for a
// service's endpoints.
//
// A cluster holds the endpoints that serve ITS port AS HTTP: membership is by
// (port, class) for the default cluster and for every bucket (proposal 037
// design (c); servesPortAsHTTP). A port an endpoint declares raw TCP or UDP
// gets no bucket from it, and an endpoint that does not list the default port
// is not in the default cluster. defaultIsHTTP is false for a
// service whose legacy target is not HTTP: nothing is then set aside as the
// default, and the returned default CLA is not published.
//
// When pods disagree about a port's class, both endpoint sets exist: this one
// holds the pods that serve the port as HTTP, the raw-TCP entry the others
// (proposal 043, fact 4). Which of the two a captured connection reaches is
// the route tables' and the chains' decision, by the class most pods declare.
func buildHTTPEndpointBuckets(serviceName string, endpoints []*registryv1.ServiceEndpoint, in reuseInputs, defaultPort uint32, defaultIsHTTP bool) (*endpointv3.ClusterLoadAssignment, map[string]*endpointv3.LocalityLbEndpoints, map[uint32]*portBucket) {
	localRegion, localZone, waypoint := in.localRegion, in.localZone, in.waypoint
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
		// The default cluster is the cluster of the legacy target, an HTTP
		// port: a pod that declares that number as raw TCP (pods of one
		// service that disagree) is not a member of it.
		if servesPortAsHTTP(endpoint, defaultPort) {
			defaultCla.Endpoints = append(defaultCla.Endpoints, lbEp)
			defaultEpMap[endpoint.GetIp()] = lbEp
		}

		for _, p := range servedPortNumbers(endpoint) {
			if (defaultIsHTTP && p == defaultPort) || !hasOwnHTTPCluster(endpoint, p, in.ports) {
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

// servedPortNumbers returns the ports an endpoint row lists, or its single
// declared port for a row that lists none.
func servedPortNumbers(endpoint *registryv1.ServiceEndpoint) []uint32 {
	if served := endpoint.GetPorts(); len(served) != 0 {
		return served
	}
	return []uint32{endpoint.GetPort()}
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
func (c *SnapshotCache) buildTCPClustersLocked(deps map[string]struct{}, tcpServiceEndpoints map[string][]*registryv1.ServiceEndpoint, in reuseInputs, prev map[string]clusterEntry, reuse map[string]reuseRecord) {
	// TCP service entries: bare-name EDS load assignment + SAN/sni only. The
	// capture TCP floor's "tcp:<svc>" cluster (captureTCPClusters) republishes
	// that load assignment under its OWN EDS name (aether#1013) and pins peer
	// identity from sanNamespaces.
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
		owns := c.ownsBareCLALocked(serviceName)
		in := in
		in.ports = c.servicePorts[serviceName]
		if in.ports.reservedOnly {
			// Its pods declare one of the mesh's own ports and nothing the
			// mesh can serve: no floor, so the legacy L4 spelling does not
			// reach that port either.
			continue
		}
		build := func(dst map[string]clusterEntry, _ map[string]struct{}) {
			c.buildTCPServiceEntriesLocked(dst, serviceName, endpoints, owns, in)
		}
		c.reuseOrBuildLocked(reusePassTCP, serviceName, endpoints, owns, in, prev, reuse, build, nil)
	}
}

// buildTCPServiceEntriesLocked builds one TCP service's floor entry and its
// per-port entries into dst. owns is ownsBareCLALocked, evaluated after the
// HTTP pass. Every input it reads is in the #1115 reuse key
// (registryreuse.go); keep it that way when adding one. Caller must hold
// clusterMu.
func (c *SnapshotCache) buildTCPServiceEntriesLocked(dst map[string]clusterEntry, serviceName string, endpoints []*registryv1.ServiceEndpoint, owns bool, in reuseInputs) {
	sanNamespaces := endpointSANNamespaces(endpoints)
	// The service's legacy target (in.ports; see buildHTTPServiceEntryLocked):
	// the port the floor forwards to, and the one raw-TCP port that is reached
	// without an SNI. Rows that carry no port_protocols and are not in
	// in.ports (a test fixture's) fall back on this listing's own rows.
	defaultPort, defaultClass := in.ports.legacyPort, in.ports.legacyClass
	if !in.ports.known() {
		defaultPort, defaultClass = legacyTarget(listedEndpoints{protocol: registryv1.Service_PROTOCOL_TCP, endpoints: endpoints})
	}
	// The floor forwards to the legacy target and carries no SNI, so it lands
	// on each member's default inbound chain: only the pods that serve THAT
	// port as raw TCP are members. A pod of the service that declares another
	// raw-TCP port (a rollout that moves the declared port) answered the
	// floor's connections from that other port.
	members := endpoints
	if defaultClass == registryv1.PortProtocol_PORT_PROTOCOL_TCP && defaultPort != 0 {
		members = tcpPortMembers(endpoints)[defaultPort]
	}
	cla, epMap := buildTCPEndpoints(serviceName, members, owns, in.localRegion, in.localZone, in.waypoint)

	tcpName := proxy.TCPClusterName(serviceName, c.meshDomain)
	dst[tcpName] = clusterEntry{
		loadAssignment: cla,
		endpoints:      epMap,
		sanNamespaces:  sanNamespaces,
		service:        serviceName,
		sni:            strconv.Itoa(int(defaultPort)),
		l4Floor:        true,
	}

	legacyTCP := uint32(0)
	if defaultClass == registryv1.PortProtocol_PORT_PROTOCOL_TCP {
		legacyTCP = defaultPort
	}
	buildTCPPortEntries(dst, serviceName, tcpName, endpoints, legacyTCP, sanNamespaces, in)
}

// buildTCPPortEntries adds into dst one entry per raw-TCP port the service
// advertises other than its legacy target (proposal 037), plus the alias entry
// of a raw-TCP legacy target (defaultPort; 0 when the legacy target is not raw
// TCP).
//
// These exist because the capture listener emits a destination_port-qualified
// chain per such port, and that chain names tcp:<fqdn>:<port>. A chain whose
// cluster is absent from the snapshot does not fail loudly: tcp_proxy has no
// ODCDS cold path, so the connection is simply killed. Chain and cluster
// therefore have to be produced from the same derived facts in the same
// snapshot generation — proposal 037 Risk 1, and the same shape as #877.
//
// Each carries its OWN load assignment, named after its own cluster
// (tcp:<fqdn>:<port>) and filtered to the endpoints that advertise that port AS
// TCP. The name is the cluster's, not the HTTP per-port spelling <fqdn>:<port>:
// that one belongs to the HTTP per-port and :<port> alias clusters, and two
// load assignments (or two subscribers) under one EDS name is exactly the
// sharing aether#1013 removes. Mirroring the bare-name EDS instead would
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
func buildTCPPortEntries(
	dst map[string]clusterEntry,
	serviceName, tcpName string,
	endpoints []*registryv1.ServiceEndpoint,
	defaultPort uint32,
	sanNamespaces []string,
	in reuseInputs,
) {
	localRegion, localZone, waypoint := in.localRegion, in.localZone, in.waypoint
	// The PRIMARY port gets an alias entry: same cluster name shape
	// (tcp:<fqdn>:<port>) so that a port-qualified reference resolves for EVERY
	// TCP port, not only the non-primary ones (proposal 037 Phase 3).
	//
	// Without it, a TCPRoute whose backendRef.port names the primary would
	// resolve to a cluster that does not exist, and tcp_proxy would kill those
	// connections silently — the same Risk 1 shape as a chain without a cluster.
	//
	// It carries NO load assignment of its own: its cluster subscribes to its
	// own EDS name and captureTCPClusters republishes the bare service's load
	// assignment under it, exactly as the HTTP :<port> aliases do
	// (buildPortAliasesLocked, aether#1013). And its sni stays EMPTY, because it addresses
	// the primary port — the destination's default inbound floor chain is what
	// serves it, and a non-empty SNI would route it to a per-port chain that
	// does not exist (#306).
	//
	// Only a raw-TCP legacy target has one (defaultPort is 0 otherwise): an
	// alias named tcp:<fqdn>:<p> for a port the service declares as HTTP was a
	// raw-TCP cluster for a port that does not speak it.
	if defaultPort != 0 {
		dst[proxy.TCPPortClusterName(tcpName, defaultPort)] = clusterEntry{
			sanNamespaces: sanNamespaces,
			service:       serviceName,
			sni:           "",
			l4Floor:       true,
		}
	}

	byPort := tcpPortMembers(endpoints)
	for _, port := range slices.Sorted(maps.Keys(byPort)) {
		if port == defaultPort {
			continue // the alias above
		}
		members := byPort[port]
		if len(members) == 0 {
			continue
		}

		claName := proxy.TCPPortClusterName(tcpName, port)
		portCla := proxy.NewClusterLoadAssignment(claName)
		epMap := make(map[string]*endpointv3.LocalityLbEndpoints, len(members))
		for _, ep := range members {
			lbEp := proxy.ServiceLocalityLbEndpointFromRegistryEndpoint(ep, localRegion, localZone, waypoint)
			portCla.Endpoints = append(portCla.Endpoints, lbEp)
			epMap[ep.GetIp()] = lbEp
		}
		proxy.SortLocalityLbEndpoints(portCla.Endpoints)

		dst[proxy.TCPPortClusterName(tcpName, port)] = clusterEntry{
			loadAssignment: portCla,
			endpoints:      epMap,
			sanNamespaces:  sanNamespaces,
			service:        serviceName,
			sni:            strconv.Itoa(int(port)),
			l4Floor:        true,
		}
	}
}

// tcpPortMembers returns, per port, the rows of a raw-TCP listing that serve
// the port as raw TCP, by the rule the capture chains are derived with
// (endpointPortClasses): a chain for (service, port) exists when most pods
// declare the port raw TCP, and then at least one row here does, so the chain
// always finds its cluster.
func tcpPortMembers(endpoints []*registryv1.ServiceEndpoint) map[uint32][]*registryv1.ServiceEndpoint {
	out := map[uint32][]*registryv1.ServiceEndpoint{}
	for _, ep := range endpoints {
		for port, class := range endpointPortClasses(ep, registryv1.Service_PROTOCOL_TCP) {
			if class == registryv1.PortProtocol_PORT_PROTOCOL_TCP {
				out[port] = append(out[port], ep)
			}
		}
	}
	return out
}

// buildTCPEndpoints builds a TCP service's endpoint map and, when this entry
// owns it (owns, from ownsBareCLALocked), its bare-name load assignment.
//
// Bare-name CLA ownership: both an HTTP default entry and a TCP floor entry for
// the same service reference a load assignment named <serviceName>, and two EDS
// resources with one name is a snapshot-consistency error in go-control-plane
// (or a silent last-writer-wins). The HTTP entry owns it when the service has
// one; the TCP entry then carries no load assignment of its own —
// clustersEndpointsAndVhosts already guards on loadAssignment != nil. The TCP
// floor cluster itself never subscribes to the bare name: captureTCPClusters
// republishes it under the floor's own name (bareServiceCLALocked,
// aether#1013).
//
// Ownership is decided among the entries built from a LIVE listing, in pass
// order: HTTP, then TCP, then UDP. An entry RETAINED after its listing went
// (retainAbsentClustersLocked) is not in the map while the passes run and so
// never keeps the name from a live entry: the floor cluster resolves through
// it, and tcp_proxy has no ODCDS cold path to recover from an empty or a
// missing one. The retained entry gives its (empty) load assignment up to the
// live one in the retention step (#1635).
func buildTCPEndpoints(
	serviceName string,
	endpoints []*registryv1.ServiceEndpoint,
	owns bool,
	localRegion, localZone string,
	waypoint proxy.WaypointRewrite,
) (*endpointv3.ClusterLoadAssignment, map[string]*endpointv3.LocalityLbEndpoints) {
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

// ownsBareCLALocked reports whether an L4 floor entry built now for
// serviceName owns the service's bare-name load assignment: it does unless an
// entry built EARLIER in the same refresh owns it (see buildTCPEndpoints) --
// the HTTP default entry for the TCP pass; that or the TCP floor entry for the
// UDP pass. Before #1635 the UDP pass looked at the HTTP entry only, so a
// service listed under TCP and UDP published two load assignments of one name
// for as long as it was. Caller must hold clusterMu, during the refresh's
// passes: the map then holds entries built from a live listing only.
func (c *SnapshotCache) ownsBareCLALocked(serviceName string) bool {
	return c.bareServiceCLALocked(serviceName) == nil
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
func (c *SnapshotCache) buildUDPClustersLocked(ctx context.Context, deps map[string]struct{}, udpServiceEndpoints map[string][]*registryv1.ServiceEndpoint, in reuseInputs, prev map[string]clusterEntry, reuse map[string]reuseRecord) {
	for serviceName, endpoints := range udpServiceEndpoints {
		if _, inScope := deps[serviceName]; !inScope {
			continue
		}
		if len(endpoints) == 0 {
			continue
		}
		owns := c.ownsBareCLALocked(serviceName)
		udpName := proxy.UDPClusterName(serviceName, c.meshDomain)
		build := func(dst map[string]clusterEntry, _ map[string]struct{}) {
			// The port the datagrams are sent to: the one this listing's
			// pods declare (legacyTarget; UDP has one port per service
			// until proposal 043 Phase 2).
			udpPort, _ := legacyTarget(listedEndpoints{protocol: registryv1.Service_PROTOCOL_UDP, endpoints: endpoints})
			cla, epMap := buildTCPEndpoints(serviceName, endpoints, owns, in.localRegion, in.localZone, in.waypoint)
			dst[udpName] = clusterEntry{
				loadAssignment: cla,
				endpoints:      epMap,
				service:        serviceName,
				sni:            strconv.Itoa(int(udpPort)),
				l4Floor:        true,
				plaintext:      true,
			}
		}
		if _, reused := c.reuseOrBuildLocked(reusePassUDP, serviceName, endpoints, owns, in, prev, reuse, build, nil); !reused {
			c.log.DebugContext(ctx, "built UDP floor cluster entry", "service", serviceName, "cluster", udpName)
		}
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

// retainAbsentClustersLocked re-inserts entries from prev that are no longer in
// c.clusters but are still in the dependency set, applying the retention grace.
// Caller must hold clusterMu.
func (c *SnapshotCache) retainAbsentClustersLocked(ctx context.Context, prev map[string]clusterEntry, deps map[string]struct{}, evidence retentionEvidence) {
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
	//
	// One load assignment per name (#1635). A retained entry's load assignment
	// is EMPTY, and the name it is published under can be one an entry built
	// from a live listing publishes in this refresh: the bare service name,
	// which the HTTP default entry, the TCP floor entry and the UDP floor
	// entry of one service all name. Two resources of one name do not reach
	// the proxy as two: go-control-plane keeps one, and which one is redrawn
	// on every build, so the published endpoints alternated between none and
	// the live ones for the whole grace. The live listing wins: a retained
	// entry holds a load assignment only under a name nothing else holds, and
	// otherwise none (the shape of a port alias). It keeps its cluster and its
	// vhost, which is what the retention is for; its cluster's subscription to
	// the bare name resolves to the live entry's load assignment, exactly as
	// it does for the floor cluster of a service listed under both.
	//
	// This runs for every retained entry on every refresh, not only when the
	// entry is first retained: the live holder can appear later (both
	// listings go, one comes back).
	held := c.heldLoadAssignmentNamesLocked()
	// One virtual host per authority, by the same rule. A retained entry's
	// virtual host can claim an authority that an entry built from a live
	// listing claims in this refresh: a service whose declared port moved
	// keeps its old default entry ("<fqdn>", "<fqdn>:8080") beside the new
	// per-port entry of that port ("<fqdn>:8080"). Two virtual hosts with one
	// domain do not degrade a route: the proxy rejects the whole out_http
	// table, which is the loopback path of every pod on the node. The live
	// entry wins; the retained one keeps its cluster and loses its virtual
	// host.
	claimed := c.claimedVhostDomainsLocked()
	now := time.Now()
	replaced := map[string]struct{}{}
	defer func() {
		c.forgetReplacedHTTPPortsLocked(replaced, evidence)
		c.rehomeBareLoadAssignmentLocked(replaced)
	}()
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
		// A REPLACEMENT is not retained: see retentionEvidence.
		if source := evidence.replacedBy(c.servicePorts, entry); source != "" {
			c.log.InfoContext(ctx, "HTTP cluster/vhost not retained: the service no longer declares its port as HTTP",
				"cluster", name, "service", entry.service, "port", primaryPortOf(entry), "evidence", source)
			replaced[entry.service] = struct{}{}
			continue
		}
		if entry.absentSince.IsZero() {
			entry.absentSince = now
			// A port alias holds no load assignment of its own (its EDS copy is
			// derived from the bare one at snapshot time, so a retained, emptied
			// default entry empties it too); synthesizing one here would publish
			// a second CLA under the alias name.
			if entry.loadAssignment != nil {
				// Name the replacement after the load assignment it replaces, NOT
				// after the map key. They coincide for HTTP and per-port entries,
				// but a TCP floor entry is keyed "tcp:<fqdn>" while publishing the
				// BARE-name EDS resource (when no HTTP entry owns it) that its floor
				// cluster's own-name copy is derived from (bareServiceCLALocked).
				// Keying off `name` there would retain an empty CLA under a name
				// that collides with the floor's copy and leave the bare one
				// unpublished.
				entry.loadAssignment = proxy.NewClusterLoadAssignment(entry.loadAssignment.GetClusterName())
				entry.endpoints = map[string]*endpointv3.LocalityLbEndpoints{}
			}
			c.log.InfoContext(ctx, "service disappeared from registry; retaining empty cluster/vhost for grace period",
				"service", name, "grace", c.retentionGrace().String())
		} else if now.Sub(entry.absentSince) > c.retentionGrace() {
			c.log.InfoContext(ctx, "service absent past grace period; pruning", "service", name)
			continue
		}
		c.clusters[name] = c.yieldClaimedDomains(ctx, claimed, name, c.yieldHeldLoadAssignment(ctx, held, name, entry))
	}
}

// retentionEvidence is what a refresh knows of what each service IS now, as
// opposed to what it was when its entries were built. It decides whether an
// HTTP entry that left the listing is kept for the grace period or not.
//
// Two different things make an HTTP entry leave the listing:
//
//   - A REMOVAL. The HTTP rows of the service are gone and nothing says what
//     the service is now: a pod restart of a single-replica service, a
//     registry that has not caught up. The entry and its virtual host are
//     kept, empty, for the grace: a request is a retriable 503 and not a
//     refusal or an on-demand stall (#1635, #167).
//
//   - A REPLACEMENT. There is positive evidence of the service's shape now,
//     and it does not declare the entry's port as HTTP: the service serves
//     another port, or that number as raw TCP or UDP. Then nothing is kept:
//     the entry's cluster resolves its endpoints through the bare service
//     name, which a live entry of another class may publish, so a virtual
//     host kept on it delivered HTTP requests to endpoints that do not serve
//     that port as HTTP. The port is answered at once by what the evidence
//     says: a refusal by name, like any port the service does not declare.
//
// The evidence, in order:
//
//  1. The rows this refresh lists of the service, under any protocol
//     (listed). Each says the protocol of each of its ports (port_protocols is
//     required), so the ports derived from them are what the service is. A
//     port they do not serve as HTTP is replaced.
//  2. With no row listed, the real ports of the service's generated Service
//     (declared), when it declares any. A Service whose last endpoint left
//     declares none, and is no evidence.
//
// Neither: a removal.
type retentionEvidence struct {
	// listed is the set of services at least one row of which is in a
	// listing of this refresh; their derived ports are in servicePorts.
	listed map[string]struct{}
	// declared is declaredByService().
	declared map[string]servicePorts
}

// replacedBy returns the evidence that replaces a retained HTTP entry, "" when
// there is none (the entry is kept for the grace). ports is the cache's
// servicePorts, which for a listed service holds this refresh's derivation.
func (e retentionEvidence) replacedBy(ports map[string]servicePorts, entry clusterEntry) string {
	if entry.l4Floor {
		return "" // a raw-TCP or UDP floor: no virtual host, no HTTP request
	}
	port := primaryPortOf(entry)
	if port == 0 {
		return ""
	}
	if _, ok := e.listed[entry.service]; ok {
		if ports[entry.service].hasHTTP(port) {
			return ""
		}
		return "listed endpoints"
	}
	if d, ok := e.declared[entry.service]; ok && d.known() && !d.hasHTTP(port) {
		return "generated Service"
	}
	return ""
}

// forgetReplacedHTTPPortsLocked takes the HTTP ports the generated Service no
// longer declares out of what is remembered of a service no row is listed of
// (servicePorts keeps the last derivation for as long as an entry is
// retained). The tables are built from it: left alone, a port whose entry was
// just replaced would still be routed, to a cluster that is gone. A listed
// service needs nothing: its ports were derived from its rows in this
// refresh. Caller must hold clusterMu.
func (c *SnapshotCache) forgetReplacedHTTPPortsLocked(replaced map[string]struct{}, evidence retentionEvidence) {
	for svc := range replaced {
		if _, ok := evidence.listed[svc]; ok {
			continue
		}
		last, declared := c.servicePorts[svc], evidence.declared[svc]
		kept := make([]uint32, 0, len(last.http))
		for _, port := range last.http {
			if declared.hasHTTP(port) {
				kept = append(kept, port)
			}
		}
		last.http = kept
		if last.legacyClass == registryv1.PortProtocol_PORT_PROTOCOL_HTTP && !declared.hasHTTP(last.legacyPort) {
			last.legacyPort, last.legacyClass = 0, registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED
		}
		if last.known() {
			c.servicePorts[svc] = last
		} else {
			delete(c.servicePorts, svc)
		}
	}
}

// rehomeBareLoadAssignmentLocked gives the bare-name load assignment of a
// service whose HTTP default entry was just replaced to its retained floor
// entry, when nothing else publishes that name. A floor cluster subscribes to
// its own copy of the bare load assignment; while the HTTP default entry held
// the bare name the floor entry held none, so with that entry gone and the
// floor itself retained (the service left the raw-TCP listing in the same
// refresh) the copy had nothing to be derived from, and the floor cluster
// subscribed to a resource that was not published. One holder per name, as
// everywhere (#1635): the TCP floor before the UDP one, the order
// bareServiceHolderLocked reads them in. Caller must hold clusterMu.
func (c *SnapshotCache) rehomeBareLoadAssignmentLocked(replaced map[string]struct{}) {
	for svc := range replaced {
		if c.bareServiceCLALocked(svc) != nil {
			continue
		}
		for _, key := range []string{proxy.TCPClusterName(svc, c.meshDomain), proxy.UDPClusterName(svc, c.meshDomain)} {
			if entry, ok := c.clusters[key]; ok && entry.loadAssignment == nil {
				entry.loadAssignment = proxy.NewClusterLoadAssignment(svc)
				entry.endpoints = map[string]*endpointv3.LocalityLbEndpoints{}
				c.clusters[key] = entry
				break
			}
		}
	}
}

// claimedVhostDomainsLocked returns the authorities the virtual hosts of the
// entries now in the cluster map claim. Called by the retention step before it
// re-inserts anything, so these are the ones claimed from a live listing in
// this refresh. Caller must hold clusterMu.
//
// Each authority maps to the service that claims it: the collision this is for
// is between two entries of ONE service, and two services never share an
// authority.
func (c *SnapshotCache) claimedVhostDomainsLocked() map[string]string {
	claimed := make(map[string]string, 2*len(c.clusters))
	for _, entry := range c.clusters {
		for _, d := range entry.vhost.GetDomains() {
			claimed[d] = entry.service
		}
	}
	return claimed
}

// yieldClaimedDomains returns a retained entry as the retention step stores
// it: without its virtual host when an authority it claims is already claimed
// (see retainAbsentClustersLocked), unchanged otherwise, in which case its
// authorities are recorded so that no later retained entry claims them too.
// The virtual host is dropped whole, never edited: it is a published proto.
func (c *SnapshotCache) yieldClaimedDomains(ctx context.Context, claimed map[string]string, name string, entry clusterEntry) clusterEntry {
	if entry.vhost == nil {
		return entry
	}
	for _, d := range entry.vhost.GetDomains() {
		if owner, taken := claimed[d]; taken && owner == entry.service {
			c.log.InfoContext(ctx, "retained entry publishes no virtual host of its own: another entry of the service claims one of its authorities",
				"cluster", name, "service", entry.service, "authority", d)
			entry.vhost = nil
			return entry
		}
	}
	for _, d := range entry.vhost.GetDomains() {
		claimed[d] = entry.service
	}
	return entry
}

// heldLoadAssignmentNamesLocked returns the names under which the entries now
// in the cluster map hold a load assignment. Called by the retention step
// before it re-inserts anything, so these are the names published from a live
// listing in this refresh. Caller must hold clusterMu.
func (c *SnapshotCache) heldLoadAssignmentNamesLocked() map[string]struct{} {
	held := make(map[string]struct{}, len(c.clusters))
	for _, entry := range c.clusters {
		if entry.loadAssignment != nil {
			held[entry.loadAssignment.GetClusterName()] = struct{}{}
		}
	}
	return held
}

// yieldHeldLoadAssignment returns a retained entry as the retention step
// stores it: without its load assignment when another entry already holds
// that name (#1635, see retainAbsentClustersLocked), unchanged otherwise, in
// which case the name is recorded as held so that no later retained entry
// publishes it as well.
func (c *SnapshotCache) yieldHeldLoadAssignment(ctx context.Context, held map[string]struct{}, name string, entry clusterEntry) clusterEntry {
	if entry.loadAssignment == nil {
		return entry
	}
	claName := entry.loadAssignment.GetClusterName()
	if _, taken := held[claName]; !taken {
		held[claName] = struct{}{}
		return entry
	}
	c.log.InfoContext(ctx, "retained entry publishes no load assignment of its own: another entry of the service publishes that name",
		"cluster", name, "service", entry.service, "loadAssignment", claName)
	entry.loadAssignment = nil
	entry.endpoints = map[string]*endpointv3.LocalityLbEndpoints{}
	return entry
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

// quicFanout is the per-snapshot input to the east-west QUIC fan-out: the
// local workload identities whose client certificate the snapshot carries
// (sorted), the OBSERVED (destination, source) pairs (issue #1020), the mTLS
// state the twins are rendered from, and whether the waypoint split is on.
// East-west QUIC is unconditional (decision 2026-09-26, #979): every eligible
// destination (see armsFor) gets selection arms, there is no allow-list.
//
// identities is deliberately NOT every local identity (issue #1049). A twin
// names its source's SVID statically in its transport socket, so a twin
// published before that secret is in the snapshot warms on SDS until SPIRE
// delivers it -- 6.9-7.4 s after the CNI ADD of a new pod on the reference cluster (rev248) --
// and every request its arm routes there meanwhile 503s NC at the 2 s
// on_demand timeout. An identity still waiting for its certificate
// (awaitingCert) gets neither an arm nor a twin, so its requests ride the h2
// cluster: that one is already warm, and its client certificate is fetched on
// demand per connection (the #842 selector), so no route points at a cluster
// that cannot exist yet. The snapshot that carries the certificate carries the
// arm and the twin with it.
type quicFanout struct {
	// deps is the node's dependency set: the destinations eligible for arms.
	deps         map[string]struct{}
	identities   []string
	awaitingCert []string
	pairs        map[quicPair]struct{}
	mtls         localMTLSState
	waypoint     bool
	// idleTimeout is every twin's pool idle timeout (aether#1054).
	idleTimeout time.Duration
	// aliasCLA republishes a load assignment under a twin's EDS name and
	// twinCluster builds a twin: the snapshot's memoized ones (#1115,
	// derivedmemo.go), or proxy.LoadAssignmentAlias / proxy.QUICClusterFrom
	// when nil.
	aliasCLA    func(base *endpointv3.ClusterLoadAssignment, name string) *endpointv3.ClusterLoadAssignment
	twinCluster func(base *clusterv3.Cluster, name, clientSpiffeID, validationContextName string, sanURIs []string, sni string, idleTimeout time.Duration) *clusterv3.Cluster
}

// quicFanoutSnapshot takes the fan-out inputs for one snapshot. It also
// prunes the observed pairs whose source left the node or whose destination
// left the dependency set, so a pruned pair's twin leaves in the
// same snapshot. localMu and depMu are taken one after the other, never
// nested, and neither inside clusterMu.
func (c *SnapshotCache) quicFanoutSnapshot() quicFanout {
	if c.edge {
		return quicFanout{}
	}
	identities := c.localWorkloadIdentities()
	deps, pairs := c.quicDemandSnapshot(identities)
	f := quicFanout{deps: deps, pairs: pairs, waypoint: c.waypointEnabled, idleTimeout: time.Duration(c.quicIdleTimeout.Load())}
	if len(identities) == 0 {
		return f
	}
	f.identities, f.awaitingCert = c.splitByClientCertificate(identities)
	f.mtls = c.localMTLSSnapshot()
	return f
}

// armsFor returns the (source SPIFFE ID -> twin name) selection arms for one
// cluster-cache entry -- one per local ServiceAccount whose certificate is
// served, whether or not its twin has been built -- or nil when the entry is
// not an eligible destination: a service in the node's dependency set (an
// entry can outlive its membership until the scoped reload drops it, and
// recordQUICPair refuses such a pair), its DEFAULT entry (keyed by the bare
// service, so never a per-port or alias entry), identity-ready (mtlsCluster !=
// nil), with no endpoint dialed through a remote cluster's waypoint tunnel
// (proxy.HasWaypointEndpoint -- that path has no QUIC leg). Every service in
// the node's dependency set that passes is eligible; there is no allow-list.
//
// An arm whose twin is not in the snapshot is how a new pair is observed
// (issue #1020): the source's first request routes to the missing name, the
// on_demand filter asks for it over ODCDS, and ObserveQUICTwin builds it.
func (q quicFanout) armsFor(key string, entry clusterEntry, meshDomain string) map[string]string {
	if len(q.identities) == 0 || key != entry.service || entry.mtlsCluster == nil || entry.cluster == nil {
		return nil
	}
	if !has(q.deps, entry.service) {
		return nil
	}
	if q.waypoint && proxy.HasWaypointEndpoint(entry.loadAssignment) {
		return nil
	}
	arms := make(map[string]string, len(q.identities))
	for _, id := range q.identities {
		arms[id] = proxy.QUICClusterName(entry.service, meshDomain, proxy.SourceSAKeyFromSpiffeID(id))
	}
	return arms
}

// twinsFor returns the per-source HTTP/3 clusters for one entry: one per arm
// whose (destination, source) pair has been OBSERVED, in sorted identity
// order, with their names in the same order.
func (q quicFanout) twinsFor(entry clusterEntry, arms map[string]string, meshDomain string) ([]types.Resource, []string) {
	if len(arms) == 0 || len(q.pairs) == 0 {
		return nil, nil
	}
	// The default entry's sni is its primary port; QUIC needs the hostname
	// form "<port>.<authority>" (aether#957, proxy.QUICServerName).
	sni := proxy.QUICServerName(entry.sni, proxy.ServiceClusterName(entry.service, meshDomain))
	twinCluster := q.twinCluster
	if twinCluster == nil {
		twinCluster = proxy.QUICClusterFrom
	}
	var twins []types.Resource
	var names []string
	for _, id := range q.identities {
		if _, observed := q.pairs[quicPair{service: entry.service, source: proxy.SourceSAKeyFromSpiffeID(id)}]; !observed {
			continue
		}
		twins = append(twins, twinCluster(entry.cluster, arms[id], id, q.mtls.validationContextName, entry.sanURIs, sni, q.idleTimeout))
		names = append(names, arms[id])
	}
	return twins, names
}

// entryTwinsAndVhost returns an entry's HTTP/3 twins (proposal 038 Phase 4b),
// their load assignments, and the vhost to publish for it: the entry's own
// vhost, or -- for an eligible, identity-ready default entry -- a clone
// whose route to the h2 cluster selects per source identity.
//
// Twins are demand-scoped (issue #1020): the vhost carries an arm for every
// local ServiceAccount, but only an OBSERVED pair's arm has its twin in the
// snapshot. The others are fetched on first use over ODCDS
// (ObserveQUICTwin).
//
// Each twin subscribes to its OWN EDS resource (named after the twin, see
// proxy.QUICClusterFrom), so the base's load assignment is published again
// under every twin's name in the SAME snapshot that introduces the twin. A
// twin sharing the base's EDS name is deduplicated away by Envoy's delta-ADS
// WatchMap when it arrives after its base, and warms for the full
// initial_fetch_timeout (aether#1008). An on-demand twin is always such a
// late twin, so the rule is load-bearing for every one of them. The copies
// follow twinsFor's order and are re-sorted with every other CLA by the
// caller.
func (q quicFanout) entryTwinsAndVhost(key string, entry clusterEntry, meshDomain string) ([]types.Resource, []types.Resource, *routev3.VirtualHost) {
	arms := q.armsFor(key, entry, meshDomain)
	if len(arms) == 0 {
		return nil, nil, entry.vhost
	}
	twins, names := q.twinsFor(entry, arms, meshDomain)
	var clas []types.Resource
	if entry.loadAssignment != nil && len(names) > 0 {
		alias := q.aliasCLA
		if alias == nil {
			alias = proxy.LoadAssignmentAlias
		}
		clas = make([]types.Resource, 0, len(names))
		for _, name := range names {
			clas = append(clas, alias(entry.loadAssignment, name))
		}
	}
	if entry.vhost == nil {
		return twins, clas, nil
	}
	vhost, _ := proto.Clone(entry.vhost).(*routev3.VirtualHost)
	proxy.ApplyQUICClusterSelection(vhost, entry.cluster.GetName(), arms)
	return twins, clas, vhost
}

// AwaitingClientCertificates is how many local workload identities the most
// recently BUILT snapshot held out of east-west QUIC -- no selection arm, no
// `quic:` twin -- because SPIRE had not delivered their client certificate yet
// (issue #1049). 0 before the first build.
//
// The xDS server's first serve after an agent restart waits (bounded) for it to
// reach 0 (issue #1103): a proxy reconnecting to a snapshot built before the
// SPIRE bridge caught up is told to REMOVE every twin it holds, and routes into
// the gap until the certificate's snapshot re-adds them.
func (c *SnapshotCache) AwaitingClientCertificates() int {
	c.quicBudgetMu.Lock()
	defer c.quicBudgetMu.Unlock()
	return c.quicAwaitingSeen
}

// noteQUICFanout logs the cluster budget east-west QUIC costs whenever it
// changes: one twin per OBSERVED (destination, source) pair (issue #1020), out
// of local ServiceAccounts x eligible services in the node's dependency set
// possible ones. INFO because the number is the thing an operator sizing a
// node needs to see; it must equal the proxy's `quic:` cluster count.
// awaitingCert is how many local identities are held out of the fan-out -- no
// arm, no twin -- until SPIRE delivers their certificate (issue #1049); a value
// that never returns to 0 is a stuck SVID, not a QUIC fault.
func (c *SnapshotCache) noteQUICFanout(twins, pairs, identities, awaitingCert int) {
	c.quicBudgetMu.Lock()
	changed := c.quicBudgetSeen != twins || c.quicAwaitingSeen != awaitingCert
	c.quicBudgetSeen = twins
	c.quicAwaitingSeen = awaitingCert
	c.quicBudgetMu.Unlock()
	if changed {
		c.log.Info("east-west QUIC fan-out", "quic_clusters", twins, "observed_pairs", pairs, "local_identities", identities,
			"awaiting_client_cert", awaitingCert)
	}
}

// quicArmsByService returns, for every eligible service (see armsFor), the
// (source SPIFFE ID -> quic: twin) arms its vhosts select with -- the same
// predicate armsFor applies on out_http, so both route tables select
// identically. An arm may name a twin that is not built yet; see
// entryTwinsAndVhost. Takes clusterMu for reading; callers must not hold it.
func (c *SnapshotCache) quicArmsByService() map[string]map[string]string {
	quic := c.quicFanoutSnapshot()
	if len(quic.identities) == 0 {
		return nil
	}
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	out := map[string]map[string]string{}
	for key, entry := range c.clusters {
		if arms := quic.armsFor(key, entry, c.meshDomain); len(arms) > 0 {
			out[entry.service] = arms
		}
	}
	return out
}
