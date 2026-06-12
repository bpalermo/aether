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
	"sync"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/go-logr/logr"
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

	log     logr.Logger
	metrics *cacheMetrics

	nodeName string

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
	inbound    types.Resource
	outbound   types.Resource
	appCluster types.Resource
	// healthCluster is the unrouted per-pod cluster carrying the app's active
	// health check (delegated liveness), kept separate from appCluster so the HC
	// does not gate the delivery path.
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
func NewSnapshotCache(nodeName string, log logr.Logger) *SnapshotCache {
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the cache.
	metrics, err := newCacheMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error(err, "failed to create snapshot cache metrics; continuing without instrumentation")
	}

	return &SnapshotCache{
		SnapshotCache: cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		log:           log.WithName("cache"),
		nodeName:      nodeName,
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
		depChanged:     make(chan struct{}, 1),
		version:        atomic.NewUint64(0),
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
