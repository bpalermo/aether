package cache

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/common/serviceref"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"google.golang.org/protobuf/proto"
)

// localMTLSState is a point-in-time copy of the node-wide upstream-mTLS inputs
// (guarded by localMu): the SET of local workload SPIFFE IDs, the node SVID, and
// the trust domain. The per-entry mTLS caches (clusterEntry.sanURIs and
// clusterEntry.mtlsCluster) are rendered from it.
//
// ids is the only per-workload input, and it is an identity SET — one entry per
// ServiceAccount present on the node, deduplicated and sorted downstream by
// proxy.sortedUniqueIdentities. That is what makes a cluster's bytes invariant
// under pod churn within a ServiceAccount (issue #815, release two). The
// netns→identity index it used to carry lives on in c.localWorkloads, which the
// #638 outbound-identity discriminator (identitybinding.go) still reads — but
// nothing in the Cluster proto is keyed by netns any more.
type localMTLSState struct {
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
	trustDomain := c.currentTrustDomain()

	c.localMu.RLock()
	defer c.localMu.RUnlock()

	st := localMTLSState{
		ids:                   make([]string, 0, len(c.localWorkloads)),
		nodeSpiffeID:          c.nodeSpiffeID,
		trustDomain:           trustDomain,
		validationContextName: proxy.ValidationContextName(trustDomain),
	}
	for _, id := range c.localWorkloads {
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
	// With no trust domain there is no identity to pin: emit NO SAN matchers
	// rather than "spiffe:///ns/…", which matches nothing and can never be
	// satisfied by a real peer certificate (#815). The next recompute — one
	// happens on every snapshot — fills them in.
	//
	// That choice is right and the unpinned window is meant to be one snapshot
	// wide, but an unpinned cluster is an authentication downgrade while it
	// lasts: the handshake then proves only trust-domain membership, so any mesh
	// workload satisfies it and a foreign endpoint in the load assignment turns
	// a would-be rejection into a delivered request. reportUnpinnedClusters
	// makes every such snapshot loud and counted, so a window that outlives its
	// bound cannot look identical to one that never happened (#832).
	var sanURIs []string
	if st.trustDomain != "" {
		sanURIs = make([]string, 0, len(entry.sanNamespaces))
		for _, ns := range entry.sanNamespaces {
			sanURIs = append(sanURIs, fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", st.trustDomain, ns, saName))
		}
	}
	entry.sanURIs = sanURIs

	// TCP entries carry no HTTP (h2) cluster (only the TCP floor consumes their
	// sanURIs), and before the node SVID is served the bare cluster is emitted
	// without the matcher — both leave mtlsCluster nil.
	entry.mtlsCluster = nil
	if entry.tcp || entry.cluster == nil || st.nodeSpiffeID == "" || st.trustDomain == "" {
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
		proxy.InjectUpstreamMTLS(cl, st.ids, st.nodeSpiffeID, st.validationContextName, sanURIs, entry.sni, waypointSNI)
	}
	entry.mtlsCluster = cl
}

// maxUnpinnedClusterNames bounds how many cluster names the unpinned WARN
// renders. The count is always exact; the names are the diagnostic part and a
// node-wide unpinned state would otherwise put every service on one line.
const maxUnpinnedClusterNames = 20

// unpinnedClusterMsg is the WARN a snapshot emits when it publishes clusters
// with no server-identity pin.
const unpinnedClusterMsg = "mesh clusters published with no server-identity SAN pin"

// reportUnpinnedClusters WARNs once per snapshot, naming the clusters this
// generation publishes with an EMPTY SAN pin, and counts them (#832).
//
// An unpinned cluster's upstream validation context carries no
// match_typed_subject_alt_names (proxy.upstreamTransportSocket's len == 0
// branch), so its handshake proves trust-domain membership and nothing more —
// any mesh workload satisfies it. The pin is what makes a wrong identity loud
// (#829 was caught by ssl_fail_verify_san); without it the same event is a
// clean handshake and a delivered request.
//
// Two inputs can empty the pin, and the WARN names which:
//
//   - the trust domain is not (yet) known, so there is no identity to render —
//     the deliberate lesser evil over "spiffe:///ns/…" (#815/#819), bounded to
//     the window before SPIRE resolves it. THIS is the window the issue is
//     about: it is meant to be one snapshot wide, and nothing observed it.
//   - the service's endpoints carry no Kubernetes namespace metadata, so
//     sanNamespaces is empty. Not a window at all: it persists for as long as
//     the registry keeps serving those endpoints.
//
// Called from generateSnapshot with snapshotMu held, next to the #638
// binding discriminators. Reporting here rather than inside the recompute is
// deliberate: what matters is what a snapshot PUBLISHES, and a recompute that
// is superseded before the next generation never reached Envoy.
func (c *SnapshotCache) reportUnpinnedClusters(ctx context.Context, version string) {
	names := c.unpinnedClusterNames()
	if len(names) == 0 {
		// The healthy case rides on the zero seeded at metric registration —
		// an unseeded zero reads as a false zero (a counter never incremented
		// is not a series at all).
		return
	}

	trustDomain := c.currentTrustDomain()
	reason := "service endpoints carry no namespace metadata"
	if trustDomain == "" {
		reason = "trust domain not yet known"
	}

	shown := names
	if len(shown) > maxUnpinnedClusterNames {
		shown = append(shown[:maxUnpinnedClusterNames:maxUnpinnedClusterNames], "...")
	}
	c.log.WarnContext(ctx, unpinnedClusterMsg,
		"clusters", strings.Join(shown, " "),
		"count", len(names),
		"reason", reason,
		"trust_domain", trustDomain,
		"snapshot_version", version)
	c.metrics.ClusterUnpinned(ctx, int64(len(names)))
}

// unpinnedClusterNames returns, sorted, the names of the cluster entries whose
// cached SAN pin is empty. entry.sanURIs is the single render of the pin every
// emission path reads — the HTTP/edge mTLS cluster (refreshEntryMTLSLocked) and
// the TCP floor's "tcp:<svc>" cluster (captureTCPClusters / edgeTCPClusters) —
// so checking it here covers all of them without re-walking the emitted protos.
func (c *SnapshotCache) unpinnedClusterNames() []string {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	var names []string
	for name, entry := range c.clusters {
		if len(entry.sanURIs) == 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
