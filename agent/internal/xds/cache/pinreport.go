package cache

import (
	"context"
	"sort"
	"strings"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
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
// or not its cluster carries TLS at this moment (the trust-domain window is
// reported on purpose, #832, though with no trust domain the node publishes
// no TLS cluster at all), and a TCP entry is counted whether or not the
// capture set currently publishes its floor cluster.
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
}

// clusterPinReport classifies every cluster entry (pinState) under one read of
// the cluster map.
func (c *SnapshotCache) clusterPinReport() pinReport {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	var r pinReport
	for name, entry := range c.clusters {
		switch kind, cause := entry.pinState(); kind {
		case pinPresent:
			r.counts.Pinned++
		case pinMissing:
			if r.unpinned == nil {
				r.unpinned = make(map[cachemetrics.UnpinnedCause][]string, cachemetrics.NumUnpinnedCauses)
			}
			r.unpinned[cause] = append(r.unpinned[cause], name)
			r.counts.AddUnpinned(cause)
		}
	}
	for _, names := range r.unpinned {
		sort.Strings(names)
	}
	return r
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
//   - no_namespace_metadata: the service's endpoints carry no Kubernetes
//     namespace. Not a window: it lasts as long as the registry serves them.
//   - pin_not_rendered: the entry was never rendered. Unreachable today.
//
// Bounded: at most one line per cause (a closed set of
// cachemetrics.NumUnpinnedCauses) and at most maxUnpinnedClusterNames names on
// each, so a snapshot logs at most 3 lines of 20 names however many clusters
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
// (it has to be in the pin history before the proxy can acknowledge the
// snapshot). Reporting here rather than inside the recompute is deliberate:
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
