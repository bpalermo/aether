package cache

import (
	"fmt"
	"slices"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/common/serviceref"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"google.golang.org/protobuf/proto"
)

// localMTLSState is a point-in-time copy of the node-wide upstream-mTLS inputs
// (guarded by localMu): the node SVID and the trust domain. The per-entry mTLS
// caches (clusterEntry.sanURIs and clusterEntry.mtlsCluster) are rendered from
// it.
//
// IT NO LONGER CARRIES THE LOCAL WORKLOAD IDENTITIES, and that absence is issue
// #842. It used to hold the identity SET, because every mesh cluster's
// transport_socket_matches and transport_socket_matcher were built from it. The
// client certificate is now chosen per connection from filter state, so a
// cluster has NO per-workload input at all: the only identity it names is the
// node's, as the certificate mapper's default_value. That is what makes a
// cluster's bytes invariant under every pod event, not merely under churn
// within a ServiceAccount (which is as far as #815 release two could get).
//
// The netns→identity index lives on in c.localWorkloads, which the #638
// outbound-identity discriminator (identitybinding.go) still reads and which the
// LISTENERS are generated from — that is where the source identity enters the
// data plane now.
type localMTLSState struct {
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

	return localMTLSState{
		nodeSpiffeID:          c.nodeSpiffeID,
		trustDomain:           trustDomain,
		validationContextName: proxy.ValidationContextName(trustDomain),
	}
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
	key := c.mtlsRenderKeyFor(entry, st)
	if entry.mtlsRendered != nil && entry.mtlsRendered.equal(key) {
		// Nothing the render reads changed: keep the previous sanURIs and the
		// previous mTLS cluster OBJECT (#1115), so the version memo reuses its
		// version instead of re-hashing an identical clone.
		if registryReuseAudit {
			c.auditMTLSRenderLocked(*entry, st)
		}
		return
	}
	c.renderEntryMTLSLocked(entry, st)
	entry.mtlsRendered = &key
}

// mtlsRenderKey is every input the mTLS render (renderEntryMTLSLocked) reads:
// the base cluster OBJECT (never mutated once built, so the pointer stands for
// its bytes), the entry facts rendered into the socket, the SAN pin and the
// pin's recorded state (unpinnedCause, mtlsReady), the node-wide mTLS state,
// and the cache settings the render branches on.
type mtlsRenderKey struct {
	base          *clusterv3.Cluster
	service, sni  string
	sanNamespaces []string
	l4Floor       bool
	plaintext     bool
	st            localMTLSState
	edge          bool
	waypoint      bool
	meshDomain    string
}

func (c *SnapshotCache) mtlsRenderKeyFor(entry *clusterEntry, st localMTLSState) mtlsRenderKey {
	return mtlsRenderKey{
		base: entry.cluster, service: entry.service, sni: entry.sni,
		sanNamespaces: entry.sanNamespaces, l4Floor: entry.l4Floor, plaintext: entry.plaintext,
		st: st, edge: c.edge, waypoint: c.waypointEnabled, meshDomain: c.meshDomain,
	}
}

func (k mtlsRenderKey) equal(o mtlsRenderKey) bool {
	return k.base == o.base && k.service == o.service && k.sni == o.sni &&
		slices.Equal(k.sanNamespaces, o.sanNamespaces) && k.l4Floor == o.l4Floor && k.plaintext == o.plaintext &&
		k.st == o.st && k.edge == o.edge && k.waypoint == o.waypoint && k.meshDomain == o.meshDomain
}

// auditMTLSRenderLocked re-renders an entry whose render was skipped and
// panics if the result differs from what it kept (tests only,
// registryReuseAudit): a render input missing from mtlsRenderKey would show
// here as a stale mTLS cluster.
func (c *SnapshotCache) auditMTLSRenderLocked(kept clusterEntry, st localMTLSState) {
	fresh := kept
	c.renderEntryMTLSLocked(&fresh, st)
	if !slices.Equal(fresh.sanURIs, kept.sanURIs) || !proto.Equal(fresh.mtlsCluster, kept.mtlsCluster) ||
		fresh.unpinnedCause != kept.unpinnedCause || fresh.mtlsReady != kept.mtlsReady {
		panic(fmt.Sprintf("mtls render memo: entry %q (service %q) kept a STALE mTLS render (#1115)", kept.cluster.GetName(), kept.service))
	}
}

// renderEntryMTLSLocked unconditionally renders entry's sanURIs and mTLS
// cluster from st. Caller holds clusterMu for writing.
func (c *SnapshotCache) renderEntryMTLSLocked(entry *clusterEntry, st localMTLSState) {
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
	entry.sanURIs, entry.unpinnedCause = renderSANPin(st.trustDomain, entry.sanNamespaces, saName)
	if entry.plaintext {
		// The UDP floor has no handshake: an empty pin there is not a missing
		// one, and it has no cause (#1393).
		entry.unpinnedCause = ""
	}
	// Whether the node can publish a TLS cluster for this entry at all. The
	// two conditions are the ones every emission path gates on: the early
	// return below for the HTTP cluster, tcpFloorIdentityReady for the TCP
	// floor. Without them a rendered pin is carried by nothing.
	entry.mtlsReady = st.nodeSpiffeID != "" && st.trustDomain != ""
	sanURIs := entry.sanURIs

	// TCP entries carry no HTTP (h2) cluster (only the TCP floor consumes their
	// sanURIs), and before the node SVID is served the bare cluster is emitted
	// without the matcher — both leave mtlsCluster nil.
	entry.mtlsCluster = nil
	if entry.l4Floor || entry.cluster == nil || !entry.mtlsReady {
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
		proxy.InjectUpstreamMTLS(cl, st.nodeSpiffeID, st.validationContextName, sanURIs, entry.sni, waypointSNI)
	}
	entry.mtlsCluster = cl
}

// renderSANPin renders a service's expected server identities, one SPIFFE ID
// per endpoint namespace, and says why when there are none. It is the ONLY
// place a pin is left empty, so the causes it returns are every cause there is
// (cachemetrics.UnpinnedCauses, with CausePinNotRendered for an entry this
// function never saw).
//
// With no trust domain there is no identity to pin: it returns NO SAN URIs
// rather than "spiffe:///ns/…", which matches nothing and can never be
// satisfied by a real peer certificate (#815). The next recompute fills them
// in.
//
// That choice is right and the unpinned window is meant to be short, but a
// cluster with no pin is an authentication downgrade while it lasts: a TLS
// handshake then proves only trust-domain membership, so any mesh workload
// satisfies it and a foreign endpoint in the load assignment turns a would-be
// rejection into a delivered request. reportClusterPins makes every such
// snapshot loud and counted, so a window that outlives its bound cannot look
// identical to one that never happened (#832).
//
// The trust domain is checked first: without one, whether the endpoints carry
// a namespace does not matter, and every entry on the node has the same cause.
func renderSANPin(trustDomain string, sanNamespaces []string, saName string) ([]string, cachemetrics.UnpinnedCause) {
	if trustDomain == "" {
		return nil, cachemetrics.CauseTrustDomainUnknown
	}
	if len(sanNamespaces) == 0 {
		return nil, cachemetrics.CauseNoNamespaceMetadata
	}
	sanURIs := make([]string, 0, len(sanNamespaces))
	for _, ns := range sanNamespaces {
		sanURIs = append(sanURIs, fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, ns, saName))
	}
	return sanURIs, ""
}
