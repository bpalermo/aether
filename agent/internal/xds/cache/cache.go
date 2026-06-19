// Package cache manages Envoy xDS snapshot resources for the agent.
//
// It wraps go-control-plane's SnapshotCache and maintains a map-based cache
// of Envoy resources (listeners, clusters, endpoints, virtual hosts) with
// per-resource-type locking for thread-safe incremental updates. When the
// cache is modified, snapshots are regenerated and pushed to the xDS server.
//
// The cache organizes listeners by pod container network namespace and clusters
// by service name. Each listener entry holds inbound and outbound listeners for
// a pod. Each cluster entry holds the cluster definition, pre-built load assignment
// (with endpoints keyed by IP for granular add/remove), and virtual host for routing.
//
// Snapshot generation is incremental: listeners and clusters maintain separate
// snapshot versions. The version string format is "timestamp.counter.label"
// (e.g., "1704067200000.42.listener"), where timestamp is Unix milliseconds,
// counter is an atomic increment, and label identifies the resource type.
package cache

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bpalermo/aether/common/constants"

	commonlog "github.com/bpalermo/aether/common/log"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"go.opentelemetry.io/otel"
	"go.uber.org/atomic"
)

// SnapshotCache wraps go-control-plane's SnapshotCache and manages Envoy
// xDS resources for a single node agent. It maintains separate maps of
// listeners (keyed by pod container network namespace) and clusters
// (keyed by service name) with per-resource-type RWMutex locks for
// thread-safe incremental updates.
//
// Listeners and clusters are cached separately to allow independent snapshot
// generation. Snapshots are versioned using timestamps and atomic counters
// to track changes. SnapshotCache is safe for concurrent use.
type SnapshotCache struct {
	cachev3.SnapshotCache

	log     *slog.Logger
	metrics *cacheMetrics

	nodeName string

	// localityMu guards the node locality (region/zone from node labels,
	// resolved by the CNI server's PreListen). It drives EDS priority
	// assignment (locality-aware failover).
	localityMu sync.RWMutex
	nodeRegion string
	nodeZone   string

	// subsetMu guards subsetHeaderKeys: the sorted union of provider-defined
	// subset keys across the node's dependency set, recomputed on every
	// scoped registry load and published as the shared ECDS subset-headers
	// resource.
	subsetMu         sync.RWMutex
	subsetHeaderKeys []string

	// meshDomain is the DNS-style domain mesh authorities live under; service
	// clusters are named <service>.<meshDomain> while every control-plane key
	// (registry, watch filter, dependency set, EDS, stats) stays bare. Set
	// once before the manager starts (SetMeshDomain); not safe to change at
	// runtime.
	meshDomain string

	// emitStatsPod enables per-pod labels (source_pod/destination_pod) on the
	// aether_stats request counter. Off by default to bound cardinality; set
	// once before the manager starts (SetEmitStatsPod) and read without locking
	// on every listener build.
	emitStatsPod bool

	// edge marks this cache as backing an edge (north-south ingress) proxy
	// rather than a node proxy. In edge mode the snapshot carries a single
	// public-facing listener (no per-pod inbound/outbound or health gateway),
	// service clusters use a single-identity transport socket whose SDS points
	// at SPIRE directly (no per-source matcher, no per-downstream pooling), and
	// the route table is the explicit exposed set (no ODCDS catch-all). Set once
	// before the manager starts (SetEdgeMode); read without locking.
	edge bool
	// edgeHTTPPort is the port the edge listener binds (edge mode only).
	edgeHTTPPort uint32

	listenerMu sync.RWMutex
	listeners  map[string]listenerEntry // keyed by container network namespace

	clusterMu sync.RWMutex
	clusters  map[string]clusterEntry // keyed by cluster name
	// serviceRetentionGrace overrides defaultServiceRetentionGrace when > 0
	// (test hook; see clusterEntry.absentSince).
	serviceRetentionGrace time.Duration

	secretMu sync.RWMutex
	secrets  map[string]*tlsv3.Secret // keyed by secret name (SPIFFE ID or trust domain)

	// snapshotMu serializes generateSnapshot (version generation + map reads +
	// SetSnapshot) across concurrent mutators. Without it, two callers can
	// interleave so the snapshot with the OLDER version (and older content) is
	// set last — Envoy sees a version change and applies the stale config,
	// silently dropping the newer mutation until the next snapshot trigger.
	snapshotMu sync.Mutex

	// depMu guards podDeps and observedDeps. The node dependency set derived
	// from them scopes which registry services the snapshot carries
	// (proposal 004).
	depMu   sync.RWMutex
	podDeps map[string]podDependencies // keyed by container network namespace
	// observedDeps holds services added by the ODCDS cold path, keyed by
	// service name with the last observation time; entries idle past
	// observedTTL are pruned.
	observedDeps map[string]time.Time
	// staticDeps is a fixed dependency set (edge mode): the services the edge
	// exposes. Unioned into the dependency set so the scoped registry watch
	// carries exactly the exposed services; updated by SetStaticDependencies
	// when the exposed set changes (e.g. an EdgeRoute add/remove).
	staticDeps map[string]struct{}
	// observedTTL overrides defaultObservedTTL when > 0 (test hook).
	observedTTL time.Duration
	// depChanged receives a (coalesced) signal when the dependency set
	// changes; the registry refresher rebuilds the scoped snapshot on it.
	depChanged chan struct{}

	localMu sync.RWMutex
	// localWorkloads maps a local pod's network namespace to its SPIFFE ID. It
	// drives the outbound clusters' transport-socket matcher so each upstream
	// connection presents the originating pod's client certificate.
	localWorkloads map[string]string
	// trustDomain is the workload trust domain captured from listener loads,
	// used to name the upstream mTLS validation context.
	trustDomain string
	// nodeSpiffeID is the agent's node identity (set by the SPIRE bridge once the
	// node SVID is served). Outbound clusters present it as the client certificate
	// on the transport-socket matcher's no-match path (e.g. active health-check
	// probes, which carry no source-pod filter state); while empty, upstream mTLS
	// injection is skipped.
	nodeSpiffeID string

	version *atomic.Uint64
}

// listenerEntry holds the inbound and outbound Envoy listeners for a single pod,
// plus the per-pod application cluster that decrypted inbound traffic is forwarded
// to. The inbound listener (netns-bound, mTLS) accepts mesh traffic for the pod;
// the outbound listener handles traffic from the pod to upstream services. The app
// cluster lives here (not in the registry-driven cluster map) so registry reloads,
// which rebuild that map wholesale, never drop it.
type listenerEntry struct {
	inbound  types.Resource
	outbound types.Resource
	// appClusters holds one per-port application cluster (multi-port pods); the
	// SNI-selected inbound filter chains forward decrypted traffic to these.
	appClusters []types.Resource
	// healthCluster is the unrouted per-pod cluster carrying the app's active
	// health check (delegated liveness) on the primary port, kept separate from
	// the app clusters so the HC does not gate the delivery path.
	healthCluster types.Resource
}

// clusterEntry holds a cluster definition, its pre-built load assignment,
// granular endpoint map, and virtual host for routing. Endpoints are
// stored in a map keyed by IP address to allow efficient individual
// endpoint add/remove operations without regenerating the entire load assignment.
type clusterEntry struct {
	cluster        *clusterv3.Cluster
	loadAssignment *endpointv3.ClusterLoadAssignment
	endpoints      map[string]*endpointv3.LocalityLbEndpoints // keyed by IP, for granular add/remove
	vhost          *routev3.VirtualHost
	// sanNamespaces is the sorted union of the service's endpoints'
	// Kubernetes namespaces. At snapshot time it renders the expected server
	// SPIFFE IDs (spiffe://<td>/ns/<ns>/sa/<service>) pinned on the cluster's
	// upstream mTLS validation (anti registry-poisoning). Empty disables
	// pinning for the service (bundle-only validation).
	sanNamespaces []string
	// service is the bare service name (the map key for the default cluster,
	// but distinct from it for per-port clusters keyed <fqdn>:<port>); used to
	// render SAN identities and check dependency-set membership on retention.
	service string
	// sni is the destination port this cluster addresses, set on the upstream
	// mTLS transport socket so the destination inbound demuxes to the right
	// loopback port (multi-port routing, proposal 005). Empty = default chain.
	sni string
	// absentSince is non-zero while the service is missing from the registry
	// listing. Such entries are retained (with empty endpoints) for
	// serviceRetentionGrace before being pruned: during pod churn a service
	// can transiently lose ALL endpoints, and dropping its vhost turns
	// in-flight client traffic into 404s (route table miss) instead of the
	// honest, retriable 503 of an empty cluster (rev-66 roll, 2026-06-11).
	absentSince time.Time
}

// NewSnapshotCache creates and returns a new SnapshotCache for the given node.
// The nodeName is used as the xDS node identifier when updating snapshots.
func NewSnapshotCache(nodeName string, log *slog.Logger) *SnapshotCache {
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the cache.
	metrics, err := newCacheMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error("failed to create snapshot cache metrics; continuing without instrumentation", "error", err)
	}

	return &SnapshotCache{
		SnapshotCache: cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		log:           commonlog.Named(log, "cache"),
		nodeName:      nodeName,
		meshDomain:    constants.DefaultMeshDomain,
		metrics:       metrics,
		// Initialize the resource maps up front so callers never assign to a nil
		// map. LoadListenersFromStorage assigns directly (no lazy init), which
		// panics on agent restart when pods already exist in local storage.
		listeners:      make(map[string]listenerEntry),
		clusters:       make(map[string]clusterEntry),
		secrets:        make(map[string]*tlsv3.Secret),
		localWorkloads: make(map[string]string),
		podDeps:        make(map[string]podDependencies),
		observedDeps:   make(map[string]time.Time),
		staticDeps:     make(map[string]struct{}),
		depChanged:     make(chan struct{}, 1),
		version:        atomic.NewUint64(0),
	}
}

// SetNodeLocality records the node's topology labels once resolved (CNI
// server PreListen) and signals a dependency change so the scoped snapshot
// is rebuilt with locality-aware EDS priorities — the initial snapshot may
// have been generated before the labels were known.
func (c *SnapshotCache) SetNodeLocality(region, zone string) {
	c.localityMu.Lock()
	changed := c.nodeRegion != region || c.nodeZone != zone
	c.nodeRegion = region
	c.nodeZone = zone
	c.localityMu.Unlock()

	if changed {
		c.signalDependencyChange()
	}
}

// nodeLocality returns the node's region and zone ("" until resolved).
func (c *SnapshotCache) nodeLocality() (string, string) {
	c.localityMu.RLock()
	defer c.localityMu.RUnlock()
	return c.nodeRegion, c.nodeZone
}

// SetMeshDomain overrides the default mesh domain (--mesh-domain flag). Must
// be called before the manager starts; the domain is read without locking on
// every snapshot build.
func (c *SnapshotCache) SetMeshDomain(domain string) {
	if domain != "" {
		c.meshDomain = domain
	}
}

// MeshDomain returns the configured mesh domain.
func (c *SnapshotCache) MeshDomain() string {
	return c.meshDomain
}

// SetEmitStatsPod enables per-pod labels on the aether_stats request counter
// (--stats-emit-pod flag). Must be called before the manager starts; the flag
// is read without locking on every listener build.
func (c *SnapshotCache) SetEmitStatsPod(enabled bool) {
	c.emitStatsPod = enabled
}

// SetEdgeMode switches the cache to edge (north-south ingress) snapshot
// composition and binds the edge listener to httpPort. Must be called before
// the manager starts; the mode is read without locking on every snapshot build.
func (c *SnapshotCache) SetEdgeMode(httpPort uint32) {
	c.edge = true
	c.edgeHTTPPort = httpPort
}

// SetEdgeIdentity records the edge proxy's single SVID name and trust domain so
// the edge service clusters' upstream mTLS presents that identity and validates
// the trust-domain bundle (served by SPIRE over the spire_agent SDS cluster).
// The node proxy sets these via the SPIRE bridge instead. Must be called before
// the manager starts.
func (c *SnapshotCache) SetEdgeIdentity(spiffeID, trustDomain string) {
	c.localMu.Lock()
	c.nodeSpiffeID = spiffeID
	c.trustDomain = trustDomain
	c.localMu.Unlock()
}

// perDownstreamConnectionPool reports whether service clusters should key
// upstream pools by the downstream connection. True for the node proxy (many
// workload identities); false for the single-identity edge (full multiplexing).
func (c *SnapshotCache) perDownstreamConnectionPool() bool {
	return !c.edge
}

// SetStaticDependencies replaces the fixed (edge) dependency set with the given
// services and signals a dependency change if it differs, so the scoped
// registry watch and the cluster snapshot rebuild to exactly the exposed set.
func (c *SnapshotCache) SetStaticDependencies(services []string) {
	next := make(map[string]struct{}, len(services))
	for _, s := range services {
		if s != "" {
			next[s] = struct{}{}
		}
	}

	c.depMu.Lock()
	before := c.dependencySetLocked()
	c.staticDeps = next
	after := c.dependencySetLocked()
	c.depMu.Unlock()

	if !equalSets(before, after) {
		c.signalDependencyChange()
	}
}

// setLocalWorkload records a local pod's network namespace -> SPIFFE ID mapping
// and the workload trust domain, used to build the outbound mTLS transport
// socket matcher.
func (c *SnapshotCache) setLocalWorkload(netns, spiffeID, trustDomain string) {
	c.localMu.Lock()
	c.localWorkloads[netns] = spiffeID
	c.trustDomain = trustDomain
	c.localMu.Unlock()
}

// removeLocalWorkload drops a local pod's network namespace mapping.
func (c *SnapshotCache) removeLocalWorkload(netns string) {
	c.localMu.Lock()
	delete(c.localWorkloads, netns)
	c.localMu.Unlock()
}
