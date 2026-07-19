package cache

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/common/serviceref"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"google.golang.org/protobuf/proto"
)

// localMTLSState is a point-in-time copy of the node-wide upstream-mTLS inputs
// (guarded by localMu): the local workloads' netns→SPIFFE-ID map, the node
// SVID, and the trust domain. The per-entry mTLS caches (clusterEntry.sanURIs
// and clusterEntry.mtlsCluster) are rendered from it.
type localMTLSState struct {
	netnsToID             map[string]string
	ids                   []string
	nodeSpiffeID          string
	trustDomain           string
	validationContextName string
}

// localMTLSSnapshot copies the current local mTLS state under localMu.
//
// Lock order: clusterMu (outer) → localMu (inner). This is safe to call while
// holding clusterMu because no code path acquires clusterMu while holding
// localMu (localMu critical sections never nest another lock).
func (c *SnapshotCache) localMTLSSnapshot() localMTLSState {
	c.localMu.RLock()
	defer c.localMu.RUnlock()

	st := localMTLSState{
		netnsToID:             make(map[string]string, len(c.localWorkloads)),
		ids:                   make([]string, 0, len(c.localWorkloads)),
		nodeSpiffeID:          c.nodeSpiffeID,
		trustDomain:           c.trustDomain,
		validationContextName: fmt.Sprintf("spiffe://%s", c.trustDomain),
	}
	for netns, id := range c.localWorkloads {
		st.netnsToID[netns] = id
		st.ids = append(st.ids, id)
	}
	return st
}

// recomputeMTLSClusters rebuilds every cluster entry's cached mTLS material
// (sanURIs + the injected cluster proto) from the current local workload
// state, then the next snapshot generation only reads the cache (issue #537 —
// previously this work ran inline on EVERY snapshot for every cluster).
//
// Callers are the node-wide input mutators: a local workload mapping is added
// or removed (setLocalWorkload / removeLocalWorkload / the
// LoadListenersFromStorage merge), the node SVID is (re)set (SetNodeIdentity),
// or the edge identity is set (SetEdgeIdentity). Registry reloads recompute
// inline instead (LoadClustersFromRegistry → recomputeMTLSClustersLocked)
// because they rebuild the entries themselves.
func (c *SnapshotCache) recomputeMTLSClusters() {
	c.clusterMu.Lock()
	defer c.clusterMu.Unlock()
	c.recomputeMTLSClustersLocked()
}

// recomputeMTLSClustersLocked is recomputeMTLSClusters with clusterMu already
// held for writing. The local state is snapshotted INSIDE the clusterMu
// critical section so concurrent mutators can never leave the caches rendered
// from stale inputs: every mutation is followed by a recompute, recomputes
// serialize on clusterMu, and whichever runs last reads the final local state.
func (c *SnapshotCache) recomputeMTLSClustersLocked() {
	st := c.localMTLSSnapshot()
	for name, entry := range c.clusters {
		c.refreshEntryMTLSLocked(&entry, st)
		c.clusters[name] = entry
	}
}

// refreshEntryMTLSLocked rebuilds entry's cached sanURIs and mTLS-injected
// cluster from st. Caller holds clusterMu for writing.
//
// The injected cluster is built FRESH (clone + inject) on every refresh and
// only ever REPLACED on the entry, never mutated in place: prior snapshots
// handed to go-control-plane alias the previous proto, and xDS server
// goroutines marshal it without holding clusterMu — an in-place mutation would
// be a data race (torn marshal).
func (c *SnapshotCache) refreshEntryMTLSLocked(entry *clusterEntry, st localMTLSState) {
	// Expected server identities for this service: one SPIFFE ID per endpoint
	// namespace. The peer SVID's SA is the BARE service name — entry.service
	// is the namespace-qualified "<ns>/<svc>" key (020 Part 1), so parse out
	// the bare name for the sa/ segment (the namespace comes from
	// sanNamespaces). The handshake then proves the peer IS the service asked
	// for, not merely some workload in the trust domain.
	saName := entry.service
	if ref, ok := serviceref.ParseKey(entry.service); ok {
		saName = ref.Name
	}
	sanURIs := make([]string, 0, len(entry.sanNamespaces))
	for _, ns := range entry.sanNamespaces {
		sanURIs = append(sanURIs, fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", st.trustDomain, ns, saName))
	}
	entry.sanURIs = sanURIs

	// TCP entries carry no HTTP (h2) cluster (only the TCP floor consumes their
	// sanURIs), and before the node SVID is served the bare cluster is emitted
	// without the matcher — both leave mtlsCluster nil.
	entry.mtlsCluster = nil
	if entry.tcp || entry.cluster == nil || st.nodeSpiffeID == "" {
		return
	}

	cl, _ := proto.Clone(entry.cluster).(*clusterv3.Cluster)
	// entry.sni carries the destination port so the peer's inbound demuxes to
	// the right loopback port (multi-port routing).
	if c.edge {
		// The edge has one identity and no local workloads: a single transport
		// socket presenting the edge SVID, fetched over the spire_agent SDS
		// cluster (SPIRE directly, no bridge).
		cl.TransportSocket = proxy.EdgeUpstreamTransportSocket(st.nodeSpiffeID, st.validationContextName, sanURIs, entry.sni)
	} else {
		// Waypoint (proposal 019): remote endpoints are dialed at the node
		// tunnel and demux by a structured SNI <port>.<svc>.<ns>.<meshDomain>
		// (entry.sni is the port). The two-level matcher presents it only for
		// waypoint-tagged endpoints. Empty when the feature is off.
		waypointSNI := ""
		if c.waypointEnabled {
			waypointSNI = entry.sni + "." + proxy.ServiceClusterName(entry.service, c.meshDomain)
		}
		proxy.InjectUpstreamMTLS(cl, st.netnsToID, st.ids, st.nodeSpiffeID, st.validationContextName, sanURIs, entry.sni, waypointSNI)
	}
	entry.mtlsCluster = cl
}
