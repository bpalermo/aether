package cache

import (
	"context"
	"sort"
	"strings"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// maxUnpinnedClusterNames bounds how many cluster names one unpinned WARN
// renders. The count is always exact; the names are the diagnostic part and a
// node-wide unpinned state would otherwise put every service on one line.
// There is one line per cause and the causes are a closed set
// (cachemetrics.UnpinnedCauses), so a snapshot logs at most
// len(UnpinnedCauses) lines of at most this many names each.
const maxUnpinnedClusterNames = 20

// unpinnedClusterMsg is the WARN a snapshot emits, once per cause, when it
// publishes clusters with no server-identity pin.
const unpinnedClusterMsg = "mesh clusters published with no server-identity SAN pin"

// clusterPinKind is what one cluster entry contributes to the pin report.
type clusterPinKind int

const (
	// pinNotApplicable: nothing with a TLS handshake is published for the
	// entry, so it is in neither count. The UDP floor, always; any other
	// entry whose pin is rendered while the node cannot build a TLS cluster.
	pinNotApplicable clusterPinKind = iota
	// pinPresent: a TLS cluster carrying the server-identity pin.
	pinPresent
	// pinMissing: an entry that is meant to be mTLS and has no pin.
	pinMissing
)

// pinState classifies one cluster entry. It is the ONE decision the WARN, the
// counter and the gauge are all built from, so the three cannot disagree
// (#1393 was two definitions drifting; the build-time gate in
// //agent/test/envoy_validate is the other definition, and
// TestRuntimePinReportAgreesWithTheBuildTimeGate holds this one to it).
//
// The rule, by kind of entry:
//
//   - UDP floor ("udp:<svc>"): plaintext, no transport socket, so there is no
//     pin to carry or to lose (#1393). Neither count.
//   - Every other entry is meant to be mTLS and its pin is entry.sanURIs, the
//     single render every emission path reads: the HTTP default, per-port and
//     alias clusters (entry.mtlsCluster), and the TCP floor, its primary-port
//     alias and its per-port clusters ("tcp:<svc>[:<port>]", which is also
//     what TCPRoute and TLSRoute chains forward to). An empty pin is
//     pinMissing, with the cause the render recorded.
//   - A QUIC twin ("quic:<svc>@<source>") is not an entry. It is derived at
//     snapshot time from its h2 entry and handed that entry's sanURIs, so it
//     is pinned exactly when its base is; the base entry stands for both and
//     the twin is not counted a second time.
//
// A rendered pin that nothing carries is not a pinned TLS cluster: until the
// node has a served SVID and a trust domain (and for the whole life of a mesh
// run with SPIRE off) the HTTP cluster goes out bare and the TCP floor cluster
// is withheld. Such an entry is pinNotApplicable.
//
// It reads the entry as rendered, not the emitted protos, which keeps a
// snapshot build free of the unmarshalling a structural check would need. Two
// consequences, both on the loud side: an entry with no pin is named whether
// or not its cluster carries TLS at this moment, and a TCP entry is counted
// whether or not the capture set currently publishes its floor cluster (so a
// pinned floor entry whose cluster is not published is in the pinned count).
//
// The cause says which it is (#1482). Under CauseTrustDomainUnknown (the
// window reported on purpose, #832) and CauseTLSNotPublished no TLS cluster is
// published for the entry; CauseNoNamespaceMetadata is TLS published without
// a pin. The snapshot build settles the two against what it publishes: an
// entry the node can publish TLS for is never left under CauseTLSNotPublished
// (promoteTLSNotPublished), and a floor entry whose cluster is not in the
// snapshot is not left under CauseNoNamespaceMetadata
// (demoteUnpublishedFloors).
func (e *clusterEntry) pinState() (clusterPinKind, cachemetrics.UnpinnedCause) {
	switch {
	case e.plaintext:
		return pinNotApplicable, ""
	case len(e.sanURIs) == 0:
		if e.mtlsRendered == nil || e.unpinnedCause == "" {
			// Never rendered. No path publishes such an entry today; if one
			// ever does, it gets a cause of its own rather than another's.
			return pinMissing, cachemetrics.CausePinNotRendered
		}
		return pinMissing, e.unpinnedCause
	case !e.mtlsReady:
		return pinNotApplicable, ""
	}
	return pinPresent, ""
}

// publishedClusterName is the name of the cluster a snapshot publishes for the
// entry stored under key: the name a proxy acknowledges it by. A floor entry
// is keyed by the name of the cluster built from it at snapshot time
// ("tcp:<svc>[:<port>]"). Every other entry holds its cluster, and the name is
// the cluster's: a service's default entry is keyed by the bare service name
// while its cluster is named by the FQDN.
func (e *clusterEntry) publishedClusterName(key string) string {
	if e.l4Floor || e.cluster == nil {
		return key
	}
	return e.cluster.GetName()
}

// pinReport is one snapshot's pin state: the counts (how many cluster entries
// carry a server-identity pin, and how many are meant to and do not, per
// cause), and the names of the unpinned ones, sorted, under the cause that
// emptied each one's pin.
//
// The counts are a fixed-size value and the names are collected only for
// unpinned entries, so on a healthy node building the report allocates
// nothing, whatever the number of clusters.
type pinReport struct {
	counts   cachemetrics.PinCounts
	unpinned map[cachemetrics.UnpinnedCause][]string
	// unpinnedFloors is the unpinned TCP floor entries among them, by name.
	// A floor entry's cluster is built at snapshot time and only for a service
	// that is captured (or, on the edge, routed to), so whether TLS is
	// published for it is known only once the build has made the floor
	// clusters (demoteUnpublishedFloors). Nil when there is none.
	unpinnedFloors map[string]struct{}
	// classes is every entry the report read that can carry a pin (every one
	// but the plaintext UDP floor) and the class it counted it under, when
	// track is set: what the acknowledged pin state is built from
	// (ackedPins.publish). A snapshot build tracks, into a buffer it reuses.
	track   bool
	classes []entryClass
	// mtls is the map key of every entry the read found with an mTLS-injected
	// cluster (entry.mtlsCluster): the clusters the outbound identity-binding
	// log names (logIdentityBindings, #1621). Collected by the snapshot build's
	// read of the cluster map (clustersEndpointsVhostsAndPinsInto), not by add,
	// and in map order.
	mtls []string
	// promoted records promoteTLSNotPublished: the counts moved every
	// tls_not_published entry to no_namespace_metadata after the classes were
	// collected.
	promoted bool
}

// add classifies one cluster entry (pinState) into the report. A snapshot
// build calls it from the pass it already makes over the cluster map
// (clustersEndpointsVhostsAndPins), so the report describes exactly the
// entries that build read and costs no pass of its own.
func (r *pinReport) add(name string, entry *clusterEntry) {
	kind, cause := entry.pinState()
	// A plaintext entry (the UDP floor) is not tracked: it has no transport
	// socket in any version, so nothing a proxy holds of it can be in a pin
	// series. Tracked, a version of it this agent process never published
	// (stated by a proxy that then rejects the update) would be a held cluster
	// of unknown class and withdraw the whole acknowledged gauge over bytes
	// that cannot carry a pin.
	if r.track && !entry.plaintext {
		r.classes = append(r.classes, entryClass{name: entry.publishedClusterName(name), class: classOf(kind, cause)})
	}
	switch kind {
	case pinPresent:
		r.counts.Pinned++
	case pinMissing:
		if r.unpinned == nil {
			r.unpinned = make(map[cachemetrics.UnpinnedCause][]string, cachemetrics.NumUnpinnedCauses)
		}
		r.unpinned[cause] = append(r.unpinned[cause], name)
		r.counts.AddUnpinned(cause)
		if entry.l4Floor {
			if r.unpinnedFloors == nil {
				r.unpinnedFloors = make(map[string]struct{})
			}
			r.unpinnedFloors[name] = struct{}{}
		}
	}
}

// demoteUnpublishedFloors reports a TCP floor entry that is under
// CauseNoNamespaceMetadata as CauseTLSNotPublished when its floor cluster is
// not among the clusters this snapshot publishes.
//
// A floor entry is keyed by the name of the cluster built from it
// ("tcp:<svc>[:<port>]"), and that cluster exists only while the service is in
// the capture TCP set (on the edge: while a TCPRoute or TLSRoute references
// it). The entry itself stays in the cluster cache either way. Without this an
// entry with no namespace metadata and no published floor would be reported
// as TLS served without a pin, which is not on the wire. It is the gap again
// in the snapshot that first publishes its floor.
//
// published is the floor clusters the build made. Called after
// promoteTLSNotPublished, so the two agree: an entry is CauseTLSNotPublished
// exactly when this snapshot carries no TLS cluster for it. An entry under any
// other cause is left where it is. Costs nothing unless a floor entry is
// unpinned.
func (r *pinReport) demoteUnpublishedFloors(published ...[]types.Resource) {
	if len(r.unpinnedFloors) == 0 {
		return
	}
	names := r.unpinned[cachemetrics.CauseNoNamespaceMetadata]
	if len(names) == 0 {
		return
	}
	isPublished := make(map[string]struct{})
	for _, set := range published {
		for _, res := range set {
			if cl, ok := res.(*clusterv3.Cluster); ok {
				isPublished[cl.GetName()] = struct{}{}
			}
		}
	}
	kept := make([]string, 0, len(names))
	moved := 0
	for _, name := range names {
		_, floor := r.unpinnedFloors[name]
		_, out := isPublished[name]
		if floor && !out {
			r.unpinned[cachemetrics.CauseTLSNotPublished] = append(r.unpinned[cachemetrics.CauseTLSNotPublished], name)
			moved++
			continue
		}
		kept = append(kept, name)
	}
	if moved == 0 {
		return
	}
	if len(kept) == 0 {
		delete(r.unpinned, cachemetrics.CauseNoNamespaceMetadata)
	} else {
		r.unpinned[cachemetrics.CauseNoNamespaceMetadata] = kept
	}
	r.counts.Move(cachemetrics.CauseNoNamespaceMetadata, cachemetrics.CauseTLSNotPublished, moved)
	r.sortNames()
}

// promoteTLSNotPublished reports every entry the render left under
// CauseTLSNotPublished as CauseNoNamespaceMetadata: the validation gap (#1482).
//
// A snapshot build calls it when, AFTER it has built everything that carries a
// transport socket, the node turns out to be able to publish TLS. The cause on
// an entry is of the render (mtls.go), and the TCP floor clusters are built at
// snapshot time from the node identity in force then (captureTCPClusters,
// edgeTCPClusters): an identity that lands between an entry's render and that
// build puts a TLS cluster with no pin in a snapshot whose entry still says
// "no TLS published". The benign reason must never be the label of such a
// snapshot, so the report errs to the loud side for all of them: an HTTP entry
// of the same snapshot that is still published bare is named as the gap one
// snapshot early (the recompute that follows the identity publishes its TLS).
func (r *pinReport) promoteTLSNotPublished() {
	r.promoted = true
	names := r.unpinned[cachemetrics.CauseTLSNotPublished]
	if len(names) == 0 {
		return
	}
	delete(r.unpinned, cachemetrics.CauseTLSNotPublished)
	r.unpinned[cachemetrics.CauseNoNamespaceMetadata] = append(r.unpinned[cachemetrics.CauseNoNamespaceMetadata], names...)
	r.counts.Promote(cachemetrics.CauseTLSNotPublished, cachemetrics.CauseNoNamespaceMetadata)
	r.sortNames()
}

// sortNames sorts the unpinned names, so the same clusters are shown on every
// snapshot when the list is cut.
func (r *pinReport) sortNames() {
	for _, names := range r.unpinned {
		sort.Strings(names)
	}
}

// reportClusterPins records, for the snapshot just set, how many mesh cluster
// entries carry a server-identity SAN pin and how many are meant to and do
// not, per cause (the aether.agent.snapshot.tls_clusters gauge, #1425), and
// for the ones that do not, WARNs and counts them under the cause of each
// (#832, #1424).
//
// An unpinned cluster's upstream validation context carries no
// match_typed_subject_alt_names (proxy.upstreamTransportSocket's len == 0
// branch), so its handshake proves trust-domain membership and nothing more:
// any mesh workload satisfies it. The pin is what makes a wrong identity loud
// (#829 was caught by ssl_fail_verify_san); without it the same event is a
// clean handshake and a delivered request.
//
// What can empty a pin is decided in one place (renderSANPin), and there is
// one line per cause, naming each cluster under what emptied ITS pin:
//
//   - trust_domain_unknown: there was no trust domain to render an identity
//     from, the deliberate lesser evil over "spiffe:///ns/…" (#815/#819). It
//     is meant to be bounded to the window before SPIRE resolves the trust
//     domain. The line's trust_domain attribute is the one in force when the
//     snapshot was set: if it is non-empty under this cause, the trust domain
//     has since been learned and the pins have not been re-rendered yet.
//   - tls_not_published: the service's endpoints carry no Kubernetes
//     namespace and no TLS is published for the entry (#1482): the node has
//     no served SVID yet (bounded by its arrival), or the entry is a TCP
//     floor whose cluster is not in the snapshot. It becomes the next one in
//     the snapshot that publishes TLS for it.
//   - no_namespace_metadata: the service's endpoints carry no Kubernetes
//     namespace and the cluster is published with TLS: the validation gap.
//     Not a window: it lasts as long as the registry serves them.
//   - pin_not_rendered: the entry was never rendered. Unreachable today.
//
// Bounded: at most one line per cause (a closed set of
// cachemetrics.NumUnpinnedCauses) and at most maxUnpinnedClusterNames names on
// each, so a snapshot logs at most 4 lines of 20 names however many clusters
// are unpinned. The counts on the line are always exact.
//
// The gauge is recorded on EVERY snapshot, zeros included, so "no unpinned
// cluster" is a sample with a timestamp and not an absent series, and a cause
// that no longer applies reads zero. The WARN and the counter fire only when
// something is unpinned; the counter's healthy value rides on the zeros seeded
// at registration.
//
// Called from generateSnapshot with snapshotMu held, next to the #638 binding
// discriminators, with the report generateSnapshot took before SetSnapshot
// (its entries have to be in the acknowledged pin state before the proxy can
// acknowledge the snapshot). Reporting here rather than inside the recompute is deliberate:
// what matters is what a snapshot PUBLISHES, and a recompute that is
// superseded before the next generation never reached Envoy.
func (c *SnapshotCache) reportClusterPins(ctx context.Context, version string, report pinReport) {
	c.metrics.TLSClusterPins(ctx, report.counts)
	unpinned := report.counts.UnpinnedTotal()
	if unpinned == 0 {
		return
	}

	trustDomain := c.currentTrustDomain()
	for _, cause := range cachemetrics.UnpinnedCauses {
		names := report.unpinned[cause]
		if len(names) == 0 {
			continue
		}
		shown := names
		if len(shown) > maxUnpinnedClusterNames {
			shown = append(shown[:maxUnpinnedClusterNames:maxUnpinnedClusterNames], "...")
		}
		c.log.WarnContext(ctx, unpinnedClusterMsg,
			"clusters", strings.Join(shown, " "),
			"count", len(names),
			"reason", string(cause),
			"unpinned", unpinned,
			"pinned", report.counts.Pinned,
			"trust_domain", trustDomain,
			"snapshot_version", version)
		c.metrics.ClusterUnpinned(ctx, cause, int64(len(names)))
	}
}
