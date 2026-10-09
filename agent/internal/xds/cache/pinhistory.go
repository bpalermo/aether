package cache

import (
	"context"
	"sync"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

// pinHistorySize is how many snapshot versions the pin history keeps. An ACK
// answers a response built from the snapshot that was current when the
// response was written, so the proxy is rarely more than a snapshot or two
// behind; 64 covers a burst of builds while one response is in flight. A
// version that has fallen out is not guessed at (ClusterPinsAcked).
const pinHistorySize = 64

// pinHistory is a fixed ring of (snapshot version, pin counts), plus the last
// counts a proxy acknowledged. Fixed-size values in a fixed-size array: a
// snapshot build adds one slot write and no allocation, whatever the size of
// the mesh.
type pinHistory struct {
	mu   sync.Mutex
	ring [pinHistorySize]pinHistoryEntry
	next int
	// acked is the pin state of the last acknowledged snapshot; hasAcked is
	// false until the first cluster ACK this process sees.
	acked    cachemetrics.PinCounts
	hasAcked bool
}

type pinHistoryEntry struct {
	version string
	counts  cachemetrics.PinCounts
}

// remember records the pin state of the snapshot about to be set as version.
func (h *pinHistory) remember(version string, counts cachemetrics.PinCounts) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring[h.next] = pinHistoryEntry{version: version, counts: counts}
	h.next = (h.next + 1) % pinHistorySize
}

// ack looks version up and makes it the acknowledged state. known is false
// when the version is not in the ring; changed reports whether the
// acknowledged counts differ from the previous acknowledged ones (true for the
// first).
func (h *pinHistory) ack(version string) (counts cachemetrics.PinCounts, known, changed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if version == "" {
		return counts, false, false
	}
	for i := range h.ring {
		if h.ring[i].version != version {
			continue
		}
		counts = h.ring[i].counts
		changed = !h.hasAcked || counts != h.acked
		h.acked, h.hasAcked = counts, true
		return counts, true, changed
	}
	return counts, false, false
}

// ackedClusterPinsMsg is the line logged when the pin state a proxy
// acknowledged changes and leaves at least one cluster unpinned.
const ackedClusterPinsMsg = "proxy acknowledged mesh clusters with no server-identity SAN pin"

// ackedClusterPinsClearMsg is its counterpart: the acknowledged state changed
// and no cluster is unpinned in it.
const ackedClusterPinsClearMsg = "proxy acknowledged mesh clusters, all with a server-identity SAN pin"

// ResponseAcked is the cache's ack.AckObserver: it is told of every delta
// response a proxy acknowledged and acts on the cluster ones
// (ClusterPinsAcked). The other types change no pin: the pin lives in the
// cluster's transport socket, not in its endpoints or its secrets.
func (c *SnapshotCache) ResponseAcked(ctx context.Context, typeURL, systemVersion string) {
	if typeURL == resourcev3.ClusterType {
		c.ClusterPinsAcked(ctx, systemVersion)
	}
}

// ClusterPinsAcked records that a proxy acknowledged a cluster (CDS) update
// built from the snapshot with this version: the pin state of that snapshot
// becomes the aether.agent.xds.acked_tls_clusters gauge (#1425).
//
// reportClusterPins says what the agent PUBLISHED. This says what a proxy
// accepted, which is the closest the agent gets to what the proxy holds
// without asking its admin interface. The two differ while an update is in
// flight, and they stay different for as long as the proxy rejects cluster
// updates: a NACK leaves the proxy on what it had, and this gauge with it.
//
// An agent that restarts against a proxy already holding exactly the current
// clusters is told too (#1483). The proxy states the version of every cluster
// it holds in the first Cluster request of its new stream, go-control-plane
// answers that request with an empty response naming the snapshot it compared
// the statement with, and the ACK of that response arrives here like any
// other (ack.Tracker.onDeltaResponse has the rule and why only the FIRST
// response of a stream may be read this way). The versions are hashes of each
// cluster's bytes, so "equal" is "the same clusters", whichever agent process
// sent them.
//
// What it cannot know, by construction:
//
//   - Nothing is recorded until a proxy acknowledges a cluster response to
//     this agent process. Absent means "not known since this agent started":
//     no proxy has connected yet, or the proxy rejected the only cluster
//     response it was sent. The second is the case to remember: a proxy that
//     is rejecting a cluster update when the agent restarts states the
//     clusters it held BEFORE the rejected update, which this process never
//     built and cannot count, so it is sent the update again, rejects it
//     again, and the gauge stays absent while the NACK counter moves.
//   - What the proxy states is what it ACCEPTED, not what it runs. Envoy
//     applies the valid clusters of a response it then rejects as a whole,
//     and keeps stating their old versions. The same is true of every ACK:
//     this gauge follows acknowledgements.
//   - The ACK names a snapshot version, not the clusters applied. A version no
//     longer in the history (pinHistorySize builds behind) changes nothing.
//   - With two proxy generations connected during a hot restart, the last ACK
//     from either one is what the gauge shows.
//
// It logs only when the acknowledged counts CHANGE, so a steady state is
// silent however many updates are acknowledged in it.
//
// Safe to call from the xDS stream's goroutine: it takes the pin history's own
// mutex and nothing else.
func (c *SnapshotCache) ClusterPinsAcked(ctx context.Context, version string) {
	counts, known, changed := c.pins.ack(version)
	if !known {
		return
	}
	c.metrics.TLSClusterPinsAcked(ctx, counts)
	if !changed {
		return
	}
	if unpinned := counts.UnpinnedTotal(); unpinned > 0 {
		attrs := make([]any, 0, 6+2*cachemetrics.NumUnpinnedCauses)
		attrs = append(attrs, "unpinned", unpinned, "pinned", counts.Pinned, "snapshot_version", version)
		for i, cause := range cachemetrics.UnpinnedCauses {
			attrs = append(attrs, string(cause), counts.Unpinned[i])
		}
		c.log.WarnContext(ctx, ackedClusterPinsMsg, attrs...)
		return
	}
	c.log.InfoContext(ctx, ackedClusterPinsClearMsg, "pinned", counts.Pinned, "snapshot_version", version)
}
