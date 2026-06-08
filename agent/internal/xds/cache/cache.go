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

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/go-logr/logr"
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

	log logr.Logger

	nodeName string

	listenerMu sync.RWMutex
	listeners  map[string]listenerEntry // keyed by container network namespace

	clusterMu sync.RWMutex
	clusters  map[string]clusterEntry // keyed by cluster name

	secretMu sync.RWMutex
	secrets  map[string]*tlsv3.Secret // keyed by secret name (SPIFFE ID or trust domain)

	localMu sync.RWMutex
	// localWorkloads maps a local pod's network namespace to its SPIFFE ID. It
	// drives the outbound clusters' transport-socket matcher so each upstream
	// connection presents the originating pod's client certificate.
	localWorkloads map[string]string
	// trustDomain is the workload trust domain captured from listener loads,
	// used to name the upstream mTLS validation context.
	trustDomain string

	version *atomic.Uint64
}

// listenerEntry holds inbound and outbound Envoy listeners for a single pod,
// plus the per-pod application cluster the inbound listener forwards decrypted
// traffic to. Inbound listeners handle traffic from clients to the pod, and
// outbound listeners handle traffic from the pod to upstream services. The app
// cluster lives here (not in the registry-driven cluster map) so registry
// reloads, which rebuild that map wholesale, never drop it.
type listenerEntry struct {
	inbound    types.Resource
	outbound   types.Resource
	appCluster types.Resource
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
}

// NewSnapshotCache creates and returns a new SnapshotCache for the given node.
// The nodeName is used as the xDS node identifier when updating snapshots.
func NewSnapshotCache(nodeName string, log logr.Logger) *SnapshotCache {
	return &SnapshotCache{
		SnapshotCache: cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		log:           log.WithName("cache"),
		nodeName:      nodeName,
		// Initialize the resource maps up front so callers never assign to a nil
		// map. LoadListenersFromStorage assigns directly (no lazy init), which
		// panics on agent restart when pods already exist in local storage.
		listeners:      make(map[string]listenerEntry),
		clusters:       make(map[string]clusterEntry),
		secrets:        make(map[string]*tlsv3.Secret),
		localWorkloads: make(map[string]string),
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
