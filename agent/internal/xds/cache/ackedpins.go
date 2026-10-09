package cache

import (
	"context"
	"sync"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

// pinClass is where one cluster entry is counted in a pin gauge: nowhere, as
// pinned, or as unpinned under one cause. One byte, so that the per-cluster
// record below stays small.
type pinClass uint8

const (
	// pinClassNone: not a TLS cluster that is meant to carry a pin
	// (pinNotApplicable). Counted in no series.
	pinClassNone pinClass = iota
	pinClassPinned
	// pinClassUnpinned + i is unpinned under cachemetrics.UnpinnedCauses[i].
	pinClassUnpinned
)

// classOf is pinState's answer as a pinClass. A cause outside the closed set
// is counted as the last one (CausePinNotRendered), as PinCounts.AddUnpinned
// counts it.
func classOf(kind clusterPinKind, cause cachemetrics.UnpinnedCause) pinClass {
	switch kind {
	case pinPresent:
		return pinClassPinned
	case pinMissing:
		for i, c := range cachemetrics.UnpinnedCauses {
			if c == cause {
				return pinClassUnpinned + pinClass(i)
			}
		}
		return pinClassUnpinned + pinClass(cachemetrics.NumUnpinnedCauses-1)
	}
	return pinClassNone
}

// unpinnedClass is the class of an entry unpinned under cause, which must be
// one of the closed set.
func unpinnedClass(cause cachemetrics.UnpinnedCause) pinClass {
	return classOf(pinMissing, cause)
}

// count adds one entry of class to counts.
func (class pinClass) count(counts *cachemetrics.PinCounts) {
	switch {
	case class == pinClassPinned:
		counts.Pinned++
	case class >= pinClassUnpinned:
		counts.Unpinned[class-pinClassUnpinned]++
	}
}

// entryClass is one cluster entry of a snapshot build and how the build's pin
// report counted it.
type entryClass struct {
	name  string
	class pinClass
}

// offeredVersions is how many PUBLISHED versions of one cluster the agent
// remembers the pin class of: the one the newest snapshot publishes and the
// two before it.
//
// It is not what an ACK is read with, and so not a bound on how long a proxy
// may take to answer: a version is copied into the record's `sent` list when
// a response carrying it is written, and stays there until that response is
// answered (clusterAck.sent). This list is what that copy is made from, and
// what a proxy's opening statement is read with. Three covers a cluster that
// is rebuilt twice between go-control-plane building a response and the
// stream's goroutine handing it to the tracker, a step inside this process
// with no proxy and no network in it; a version missed there has no class and
// is not guessed at (heldKnown).
const offeredVersions = 3

// offeredCluster is one version of a cluster a snapshot published and the pin
// class the snapshot's report gave the entry.
type offeredCluster struct {
	version string
	class   pinClass
}

// sentCluster is one version of a cluster in flight.
type sentCluster struct {
	offeredCluster
	// known is false when the version had no class on record when it was
	// sent (see offeredVersions).
	known bool
	// responses is how many unanswered responses carry it.
	responses int
}

// clusterAck is what the agent knows about one cluster entry on the proxy.
type clusterAck struct {
	// offered is the versions the agent published the cluster at, newest
	// first; a zero slot is unused.
	offered [offeredVersions]offeredCluster
	// sent is the versions of the cluster in flight: written to a proxy in a
	// response that has not been answered yet, with the class each had when it
	// was sent. A version stays here until every response that carried it has
	// been answered or its stream has ended, whatever the builds in between
	// do to offered, so the ACK of a version this agent sent always finds its
	// class. Nil when nothing is in flight, which is nearly always.
	sent []sentCluster
	// build is the last snapshot build whose cluster map had the entry.
	build uint64
	// holds says the proxy has accepted the cluster, and held is the version
	// it accepted it at: the one a response it acknowledged carried, or the
	// one it stated when it opened its stream. Not holds: the proxy has not
	// accepted it in any version (it was never sent, is in flight, was
	// rejected, or its removal was accepted).
	holds bool
	held  string
	// heldClass is the pin class of held. heldKnown is false when held is a
	// version this agent process has no class for: one it never published
	// (the proxy took it from the agent process before this one), or one that
	// fell out of offered while its response was in flight.
	heldClass pinClass
	heldKnown bool
}

// offer records that the newest snapshot publishes the cluster at version,
// with class. It reports whether that changed the class of what the proxy
// holds: the same bytes can be counted differently by a later snapshot (an
// HTTP cluster published bare is tls_not_published until the node can publish
// TLS and no_namespace_metadata from then on, #1482), and a version the proxy
// stated before this process ever built it becomes known here.
func (s *clusterAck) offer(version string, class pinClass) (heldChanged bool) {
	if s.holds && s.held == version && (!s.heldKnown || s.heldClass != class) {
		s.heldClass, s.heldKnown = class, true
		heldChanged = true
	}
	for i := range s.offered {
		if s.offered[i].version != version {
			continue
		}
		// Published before: move it to the front with its class of now.
		copy(s.offered[1:i+1], s.offered[:i])
		s.offered[0] = offeredCluster{version: version, class: class}
		return heldChanged
	}
	copy(s.offered[1:], s.offered[:offeredVersions-1])
	s.offered[0] = offeredCluster{version: version, class: class}
	return heldChanged
}

// classOfVersion is the class on record for version: the one it was sent with
// when it is in flight, else the one it was published with.
func (s *clusterAck) classOfVersion(version string) (pinClass, bool) {
	if version == "" {
		return pinClassNone, false
	}
	for _, o := range s.sent {
		if o.version == version && o.known {
			return o.class, true
		}
	}
	for _, o := range s.offered {
		if o.version == version {
			return o.class, true
		}
	}
	if s.holds && s.heldKnown && s.held == version {
		return s.heldClass, true
	}
	return pinClassNone, false
}

// send records that a response carrying the cluster at version was written.
func (s *clusterAck) send(version string) {
	for i := range s.sent {
		if s.sent[i].version == version {
			s.sent[i].responses++
			return
		}
	}
	class, known := s.classOfVersion(version)
	s.sent = append(s.sent, sentCluster{offeredCluster: offeredCluster{version: version, class: class}, known: known, responses: 1})
}

// answered records that a response carrying the cluster at version was
// answered, or will never be.
func (s *clusterAck) answered(version string) {
	for i := range s.sent {
		if s.sent[i].version != version {
			continue
		}
		if s.sent[i].responses--; s.sent[i].responses > 0 {
			return
		}
		s.sent = append(s.sent[:i], s.sent[i+1:]...)
		if len(s.sent) == 0 {
			s.sent = nil
		}
		return
	}
}

// hold records that the proxy accepted the cluster at version.
func (s *clusterAck) hold(version string) {
	if class, known := s.classOfVersion(version); known {
		s.holds, s.held, s.heldClass, s.heldKnown = true, version, class, true
		return
	}
	if s.holds && s.held == version {
		// What it already held, restated: the class is the one on record.
		return
	}
	s.holds, s.held, s.heldClass, s.heldKnown = true, version, pinClassNone, false
}

// release records that the proxy does not hold the cluster.
func (s *clusterAck) release() {
	s.holds, s.held, s.heldClass, s.heldKnown = false, "", pinClassNone, false
}

// ackedPins is the pin state of the clusters the proxy has ACCEPTED, cluster
// by cluster (#1508): for every cluster entry, the version the proxy accepted
// and the pin class of that version.
//
// It used to be the pin counts of "the snapshot of the last cluster ACK". That
// claims more than an ACK says. An ACK covers the clusters its response
// carried, and go-control-plane does not send a cluster again after the proxy
// rejected it, so the next response, and its ACK, are about other clusters
// while they name a snapshot that counts the rejected one in its new state.
//
// One record per cluster entry of the newest snapshot, plus one per entry
// that left the snapshot while the proxy still holds its cluster (until the
// proxy accepts the removal or opens a stream without it) or while a response
// carrying it is unanswered. A record is a fixed size plus one small item per
// version of the cluster that is in flight, which is at most one per
// unanswered response carrying the cluster; go-control-plane leaves at most
// one response of a type unanswered per request it has not answered on a
// stream, and the tracker drops them all when the stream ends. So memory is
// bounded by the number of clusters times the number of connected proxy
// streams (one, two during a hot restart), not by the number of snapshots or
// of ACKs.
//
// It has its own mutex and is a leaf: the ACK arrives on the xDS stream's
// goroutine, which must never wait on a snapshot build (snapshotMu) or on the
// cluster map (clusterMu).
type ackedPins struct {
	mu       sync.Mutex
	clusters map[string]*clusterAck
	// build counts publish calls; a record whose build is older is of an
	// entry the newest snapshot no longer has.
	build uint64
	// counts is the acknowledged pin state last reported; reported is false
	// until the first.
	counts   cachemetrics.PinCounts
	reported bool
	// unclassified is how many clusters the proxy held at a version with no
	// known class when the state was last settled. While it is not zero
	// nothing is reported: a count that leaves them out would say the proxy
	// holds fewer unpinned clusters than it may.
	unclassified int
}

// ackedPinsUpdate is what a change to ackedPins leaves for its caller to
// report, outside the lock.
type ackedPinsUpdate struct {
	counts cachemetrics.PinCounts
	// report: the counts are complete and are to be written to the gauge.
	report bool
	// changed: they differ from the last ones reported (true for the first).
	changed bool
	// unclassified is the number of held clusters with no known class;
	// unclassifiedChanged says it moved to or from zero.
	unclassified        int
	unclassifiedChanged bool
}

// publish records the cluster entries of the snapshot about to be set: the
// version each one's cluster is published at and the class the snapshot's pin
// report counts it under. Called before SetSnapshot, which is what lets a
// proxy see the snapshot: published after, an ACK could arrive first and find
// the version unknown.
//
// entries is the build's own read of the cluster map (pinReport.classes).
// versions is the snapshot's per-cluster version map; an entry with no cluster
// in the snapshot (a TCP floor that is not captured) has no version, is
// offered nothing, and can be held only at a version published earlier.
// promoted says the report moved every tls_not_published entry to
// no_namespace_metadata (promoteTLSNotPublished). Its other adjustment
// (demoteUnpublishedFloors) concerns only entries with no cluster in the
// snapshot, which are offered nothing.
//
// A walk of the entries and one of the records, no allocation unless an entry
// is new: it runs on every snapshot build.
func (h *ackedPins) publish(entries []entryClass, versions map[string]string, promoted bool) ackedPinsUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clusters == nil {
		h.clusters = make(map[string]*clusterAck, len(entries))
	}
	h.build++
	notPublished, gap := unpinnedClass(cachemetrics.CauseTLSNotPublished), unpinnedClass(cachemetrics.CauseNoNamespaceMetadata)
	heldChanged := false
	for _, e := range entries {
		s := h.recordLocked(e.name)
		version := versions[e.name]
		if version == "" {
			continue
		}
		class := e.class
		if promoted && class == notPublished {
			class = gap
		}
		if s.offer(version, class) {
			heldChanged = true
		}
	}
	for name, s := range h.clusters {
		h.forgetIfGoneLocked(name, s)
	}
	if !heldChanged {
		// Nothing the proxy holds is counted differently: a build alone
		// reports nothing.
		return ackedPinsUpdate{}
	}
	return h.settleLocked()
}

// recordLocked is the record of the entry published as the cluster name,
// marked as part of the build in progress; a new one for an entry seen for the
// first time. Callers hold h.mu.
func (h *ackedPins) recordLocked(name string) *clusterAck {
	s := h.clusters[name]
	if s == nil {
		s = &clusterAck{}
		h.clusters[name] = s
	}
	s.build = h.build
	return s
}

// forgetIfGoneLocked drops the record of a cluster the proxy does not hold
// when the newest snapshot has no entry for it either and no response carrying
// it is in flight: nothing is kept for a cluster neither published, nor sent
// and unanswered, nor held. Callers hold h.mu.
func (h *ackedPins) forgetIfGoneLocked(name string, s *clusterAck) {
	if s.build != h.build && !s.holds && len(s.sent) == 0 {
		delete(h.clusters, name)
	}
}

// deliver records the clusters of a response entering or leaving flight
// (ack.Delivery), so that a version is known for as long as its ACK can come.
func (h *ackedPins) deliver(d ack.Delivery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range d.Resources {
		s := h.clusters[r.Name]
		if s == nil {
			continue
		}
		if !d.Ended {
			s.send(r.Version)
			continue
		}
		s.answered(r.Version)
		h.forgetIfGoneLocked(r.Name, s)
	}
}

// accept applies what one answered cluster response says the proxy holds.
func (h *ackedPins) accept(a ack.Accepted) ackedPinsUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.Opening {
		h.restateLocked(a.Stated)
	}
	// A name with no record is not a cluster entry of a snapshot this process
	// built since the proxy last held it (a per-pod application cluster, a
	// QUIC twin, the passthrough cluster): it is in no pin count.
	for _, r := range a.Added {
		if s := h.clusters[r.Name]; s != nil {
			s.hold(r.Version)
		}
	}
	for _, name := range a.Removed {
		if s := h.clusters[name]; s != nil {
			s.release()
			h.forgetIfGoneLocked(name, s)
		}
	}
	return h.settleLocked()
}

// restateLocked applies the proxy's own statement of every cluster it holds,
// made in the first cluster request of a stream: it replaces what was known,
// cluster by cluster. A cluster the proxy does not state it does not hold.
// Callers hold h.mu.
func (h *ackedPins) restateLocked(stated map[string]string) {
	for name, s := range h.clusters {
		if version, holds := stated[name]; holds {
			s.hold(version)
			continue
		}
		s.release()
		h.forgetIfGoneLocked(name, s)
	}
}

// settleLocked recounts what the proxy holds. Callers hold h.mu.
func (h *ackedPins) settleLocked() ackedPinsUpdate {
	var u ackedPinsUpdate
	for _, s := range h.clusters {
		switch {
		case !s.holds:
		case !s.heldKnown:
			u.unclassified++
		default:
			s.heldClass.count(&u.counts)
		}
	}
	u.unclassifiedChanged = (u.unclassified == 0) != (h.unclassified == 0)
	h.unclassified = u.unclassified
	if u.unclassified > 0 {
		return u
	}
	u.report = true
	u.changed = !h.reported || u.counts != h.counts
	h.counts, h.reported = u.counts, true
	return u
}

// ackedClusterPinsMsg is the line logged when the pin state a proxy
// acknowledged changes and leaves at least one cluster unpinned.
const ackedClusterPinsMsg = "proxy acknowledged mesh clusters with no server-identity SAN pin"

// ackedClusterPinsClearMsg is its counterpart: the acknowledged state changed
// and no cluster is unpinned in it.
const ackedClusterPinsClearMsg = "proxy acknowledged mesh clusters, all with a server-identity SAN pin"

// ackedClusterPinsUnknownMsg is logged when the proxy turns out to hold a
// cluster at a version this agent process has no pin class for, and the
// acknowledged gauge stops being written; ackedClusterPinsKnownMsg when that
// ends.
const (
	ackedClusterPinsUnknownMsg = "proxy holds mesh clusters at a version this agent did not publish; their pin state is not known and the acknowledged pin gauge is not written"
	ackedClusterPinsKnownMsg   = "the pin state of every mesh cluster the proxy holds is known again"
)

// ResponseDelivery is the cache's ack.DeliveryObserver: the clusters of a
// response entering or leaving flight. The other types carry no pin.
func (c *SnapshotCache) ResponseDelivery(_ context.Context, delivery ack.Delivery) {
	if delivery.TypeURL == resourcev3.ClusterType {
		c.acked.deliver(delivery)
	}
}

// ResponseAccepted is the cache's ack.AckObserver: it is told what every
// answered delta response says the proxy holds and acts on the cluster ones
// (ClustersAccepted). The other types change no pin: the pin lives in the
// cluster's transport socket, not in its endpoints or its secrets.
func (c *SnapshotCache) ResponseAccepted(ctx context.Context, accepted ack.Accepted) {
	if accepted.TypeURL == resourcev3.ClusterType {
		c.ClustersAccepted(ctx, accepted)
	}
}

// ClustersAccepted records what one answered cluster (CDS) response says the
// proxy holds, and writes the pin state of the clusters the proxy has accepted
// as the aether.agent.xds.acked_tls_clusters gauge (#1425, #1508).
//
// reportClusterPins says what the agent PUBLISHED. This says what a proxy
// accepted, which is the closest the agent gets to what the proxy holds
// without asking its admin interface. The two differ while an update is in
// flight, and they stay different for as long as the proxy rejects a cluster.
//
// It is kept per cluster (ackedPins), from two things a proxy says:
//
//   - An ACK is about the clusters its response carried, and no other: each
//     is now held at the version it was sent at, each removed one is gone. A
//     cluster the proxy REJECTED stays at the version it last accepted, or at
//     none, through every later ACK, until a response that carries it is
//     acknowledged. The pinned go-control-plane does not send it again on that
//     stream unless it changes (#1510).
//   - The first cluster request of a stream states every cluster the proxy
//     holds, by version, and the answer to the first response settles the
//     whole set (ack.Accepted). That is what gives a restarted agent its
//     sample from a proxy that is already in sync (#1483), and what corrects
//     the record after a stream is lost with an answer in flight.
//
// A version is a hash of the cluster's bytes, so "the proxy holds version V"
// is "the proxy holds exactly the cluster this agent published as V", and its
// pin class is the one the snapshot that published V counted it under.
//
// What it cannot know, by construction:
//
//   - Nothing is written until a proxy answers this agent process: absent
//     means "not known since this agent started".
//   - The class of a version this process never published. A proxy that was
//     rejecting a cluster update when the agent restarted states the cluster
//     it held BEFORE that update; the new process sends the update again and
//     it is rejected again. The gauge is then NOT written (a count without
//     that cluster would say the proxy holds fewer unpinned clusters than it
//     may), the agent says so once (ackedClusterPinsUnknownMsg), and the NACK
//     counter moves. It is written again when every held version is known.
//     "Not written" is absent, also for a gauge that was written before: it
//     is an observable gauge and has no sample while this lasts.
//   - A cluster the proxy holds that is not a cluster entry of any snapshot
//     this process built is in no count.
//   - What the proxy states is what it ACCEPTED, not what it runs. Envoy
//     applies the valid clusters of a response it then rejects as a whole,
//     and keeps stating their old versions. They are counted at the old
//     version here; a proxy generation that rejected its first response has
//     accepted nothing, and counts nothing, although it runs them.
//   - With two proxy generations connected during a hot restart there is one
//     record per cluster, not one per generation: each cluster shows the last
//     answer from either, and a generation's opening answer replaces the set.
//
// An entry with no cluster in the snapshot (a TCP floor that is not captured)
// is published as an entry and never held by a proxy: it is in the published
// gauge and not in this one.
//
// It logs only when the acknowledged counts CHANGE, so a steady state is
// silent however many updates are acknowledged in it.
//
// Safe to call from the xDS stream's goroutine: it takes the acknowledged
// state's own mutex and nothing else. It walks every cluster record once.
func (c *SnapshotCache) ClustersAccepted(ctx context.Context, accepted ack.Accepted) {
	c.reportAckedPins(ctx, c.acked.accept(accepted), accepted.SystemVersion)
}

// reportAckedPins writes one change of the acknowledged pin state to the
// gauge and the log. version is the snapshot whose build or whose response
// produced it.
func (c *SnapshotCache) reportAckedPins(ctx context.Context, u ackedPinsUpdate, version string) {
	if u.unclassified > 0 {
		// Withdrawn, not left: a gauge that was written before would go on
		// exporting its last values as if they were the acknowledged state.
		c.metrics.TLSClusterPinsAckedUnknown()
	}
	if u.unclassifiedChanged {
		if u.unclassified > 0 {
			c.log.WarnContext(ctx, ackedClusterPinsUnknownMsg, "clusters", u.unclassified, "snapshot_version", version)
		} else {
			c.log.InfoContext(ctx, ackedClusterPinsKnownMsg, "snapshot_version", version)
		}
	}
	if !u.report {
		return
	}
	c.metrics.TLSClusterPinsAcked(ctx, u.counts)
	if !u.changed {
		return
	}
	if unpinned := u.counts.UnpinnedTotal(); unpinned > 0 {
		attrs := make([]any, 0, 6+2*cachemetrics.NumUnpinnedCauses)
		attrs = append(attrs, "unpinned", unpinned, "pinned", u.counts.Pinned, "snapshot_version", version)
		for i, cause := range cachemetrics.UnpinnedCauses {
			attrs = append(attrs, string(cause), u.counts.Unpinned[i])
		}
		c.log.WarnContext(ctx, ackedClusterPinsMsg, attrs...)
		return
	}
	c.log.InfoContext(ctx, ackedClusterPinsClearMsg, "pinned", u.counts.Pinned, "snapshot_version", version)
}
