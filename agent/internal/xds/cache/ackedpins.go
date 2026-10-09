package cache

import (
	"context"
	"sync"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
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
// answered or its stream ends (clusterAck.sent). This list is what that copy is made from, and
// what a proxy's opening statement is read with. Three covers a cluster that
// is rebuilt twice between go-control-plane building a response and the
// stream's goroutine handing it to the tracker, a step inside this process
// with no proxy and no network in it; a version missed there has no class and
// is not guessed at (heldKnown).
const offeredVersions = 3

// goneBuilds is how many snapshot builds the record of an entry that LEFT the
// snapshot is kept for when the proxy does not hold its cluster and no
// response carrying it is in flight: it is dropped by the goneBuilds-th build
// without the entry.
//
// It covers the same step as offeredVersions, for an entry that is removed
// instead of rebuilt. go-control-plane builds a response from the snapshot of
// the moment, and the acknowledged pin state learns of it only when the stream
// writes it (deliver). A build that drops the entry in between sees a record
// that is neither published, nor in flight, nor held; dropped there and then,
// the response would be written and acknowledged with nothing to count it by,
// and the proxy would hold a cluster the gauge leaves out. So the record, and
// with it the class of each version, outlives the entry by the builds that
// step can span: the same two.
const goneBuilds = offeredVersions

// offeredCluster is one version of a cluster a snapshot published and the pin
// class the snapshot's report gave the entry.
type offeredCluster struct {
	version string
	class   pinClass
}

// sentCluster is one version of a cluster in flight. One item per version,
// whatever the number of responses and of streams that carry it: the class is
// of the version's bytes as the agent last published them, not of a response
// (clusterAck.offer keeps it so), which is why the ACK of any of them can be
// read with it.
type sentCluster struct {
	offeredCluster
	// known is false when the version had no class on record when it was
	// sent (see offeredVersions) and has not been published since.
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
	// response that has not been answered yet, with the class each was last
	// published with. A version stays here until every response that carried it has
	// been answered or its stream has ended, whatever the builds in between
	// do to offered, so the ACK of a version finds the class it had on record
	// when its response was written (none, if it had already fallen out of
	// offered by then: sentCluster.known). Nil when nothing is in flight, which
	// is nearly always.
	sent []sentCluster
	// build is the last snapshot build whose cluster map had the entry.
	build uint64
	// holds says the agent has read the proxy's acceptance of the cluster,
	// and held is the version it accepted it at: the one a response it
	// acknowledged carried, or the one it stated when it opened its stream.
	// Not holds: the agent has read no acceptance that still stands (the
	// cluster was never sent, is in flight, was rejected, its removal was
	// accepted, or an opening statement did not name it). That is not always
	// "the proxy does not have it": an acceptance on a stream that ended
	// before the agent read it is unknown here until the proxy states the
	// cluster on its next stream.
	holds bool
	held  string
	// heldClass is the pin class of held. heldKnown is false when held is a
	// version there was no class on record for when the proxy's acceptance of
	// it was read (classOfVersion):
	//   - a STATED version that is neither among offered nor in flight: one
	//     this agent process never published (the proxy took it from the
	//     agent process before this one), or one it published and has since
	//     rebuilt offeredVersions times;
	//   - an ACKNOWLEDGED version that had fallen out of offered BEFORE its
	//     response was recorded as in flight (offeredVersions). Falling out
	//     while in flight does not do it: the class is kept in sent from the
	//     moment the response is recorded;
	//   - any version a proxy stated for a cluster with no record: one that
	//     was dropped, or never existed (restateLocked).
	// It becomes known if a later snapshot publishes that version (offer).
	heldClass pinClass
	heldKnown bool
}

// offer records that the newest snapshot publishes the cluster at version,
// with class. It reports whether that changed the class of what the proxy
// holds: the same bytes can be counted differently by a later snapshot (an
// HTTP cluster published bare is tls_not_published until the node can publish
// TLS and no_namespace_metadata from then on, #1482), and a version the proxy
// stated before this process ever built it becomes known here.
//
// The copy kept for the version while it is in flight follows: it is what its
// ACK is read with once the version is no longer among the published ones, and
// it must not be the class of the time it was first sent. Otherwise a response
// sent before a reclassification and acknowledged after the version fell out
// of offered would count the cluster under the reason it no longer has, and
// with two proxy generations sent the same version on either side of the
// reclassification, both answers would.
func (s *clusterAck) offer(version string, class pinClass) (heldChanged bool) {
	if s.holds && s.held == version && (!s.heldKnown || s.heldClass != class) {
		s.heldClass, s.heldKnown = class, true
		heldChanged = true
	}
	for i := range s.sent {
		if s.sent[i].version == version {
			s.sent[i].class, s.sent[i].known = class, true
		}
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

// classOfVersion is the class on record for version: the one a recent snapshot
// published it with, else the one kept with it while it is in flight, else the
// one it is already held with. None of the three: not on record.
//
// Published first, because a later snapshot can count the same bytes under
// another reason (offer), and a cluster must be counted alike on both gauges.
// The class kept with a sent version is the same one for as long as the
// version is published (offer updates it), and is what is left when no
// snapshot remembers the version any more.
func (s *clusterAck) classOfVersion(version string) (pinClass, bool) {
	if version == "" {
		return pinClassNone, false
	}
	for _, o := range s.offered {
		if o.version == version {
			return o.class, true
		}
	}
	for _, o := range s.sent {
		if o.version == version && o.known {
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
// by cluster (#1508): for every cluster entry that can carry a pin (every one
// but the plaintext UDP floor, pinReport.add), the version the proxy accepted
// and the pin class of that version.
//
// It used to be the pin counts of "the snapshot of the last cluster ACK". That
// claims more than an ACK says. An ACK covers the clusters its response
// carried, and go-control-plane does not send a cluster again after the proxy
// rejected it, so the next response, and its ACK, are about other clusters
// while they name a snapshot that counts the rejected one in its new state.
//
// One record per such entry of the newest snapshot, plus one per entry
// that left the snapshot while the proxy still holds its cluster (until the
// proxy accepts the removal or opens a stream without it) or while a response
// carrying it is unanswered, plus one per entry that left in the last few
// builds (goneBuilds), plus one per cluster a proxy states it holds that the
// agent neither publishes nor has a record of (restateLocked; for as long as
// the proxy holds it). A record is a fixed size plus one small item per
// version of the cluster that is in flight, which is at most one per
// unanswered response carrying the cluster; go-control-plane leaves at most
// one response of a type unanswered per request it has not answered on a
// stream, and the tracker drops them all when the stream ends. So memory is
// bounded by the number of clusters times the number of connected proxy
// streams (one, two during a hot restart) plus what those proxies state, not
// by the number of snapshots or of ACKs.
//
// It has its own mutex: the ACK arrives on the xDS stream's goroutine, which
// must never wait on a snapshot build (snapshotMu) or on the cluster map
// (clusterMu).
//
// mu covers a change of the state AND the report of it (the gauge write and
// the log lines, SnapshotCache.reportAckedPins): the two are one critical
// section. Changes come from several goroutines (one per connected proxy
// generation, and the snapshot build), and a report made after the lock is
// released can be overtaken by the report of a later change, which leaves the
// gauge on the older state, or written when the later change withdrew it. So
// publishLocked and acceptLocked are called with mu held, and the caller
// reports before it releases it. Nothing taken under mu waits on anything but
// the metric's own leaf mutex and the log handler.
type ackedPins struct {
	mu       sync.Mutex
	clusters map[string]*clusterAck
	// published is the cluster versions of the newest snapshot, by name: the
	// snapshot's own map, which nothing changes once the snapshot is built. A
	// name in it with no record is a cluster the pin gauges do not count
	// (restateLocked).
	published map[string]string
	// build counts publish calls; a record whose build is older is of an
	// entry the newest snapshot no longer has, or of a stated cluster that
	// was never an entry of a snapshot this record saw (restateLocked).
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
// report. The caller reports it while it still holds ackedPins.mu, the lock
// it made the change under (see ackedPins).
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

// publishLocked records the cluster entries of the snapshot about to be set:
// the version each one's cluster is published at and the class the snapshot's
// pin report counts it under. Called before SetSnapshot, which is what lets a
// proxy see the snapshot: published after, an ACK could arrive first and find
// the version unknown. Callers hold h.mu and report the update before they
// release it (ackedPins).
//
// entries is the build's own read of the cluster map (pinReport.classes).
// versions is the snapshot's per-cluster version map; an entry with no cluster
// in the snapshot (a TCP floor that is not captured) has no version, is
// offered nothing, and can be held only at a version published earlier.
// promoted says the report moved every tls_not_published entry to
// no_namespace_metadata (promoteTLSNotPublished). Its other adjustment
// (demoteUnpublishedFloors) concerns only entries with no cluster in the
// snapshot, which are offered nothing: what a proxy still holds of one is a
// version from an earlier snapshot, counted with the class that snapshot gave
// it.
//
// A walk of the entries and one of the records, no allocation unless an entry
// is new: it runs on every snapshot build.
func (h *ackedPins) publishLocked(entries []entryClass, versions map[string]string, promoted bool) ackedPinsUpdate {
	if h.clusters == nil {
		h.clusters = make(map[string]*clusterAck, len(entries))
	}
	h.build++
	h.published = versions
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
// when no response carrying it is in flight and the last goneBuilds snapshots
// had no entry for it: nothing is kept for good for a cluster neither
// published, nor sent and unanswered, nor held. Not at the first build without
// it: a response built before that one may not have been written yet
// (goneBuilds). A record that an answer leaves with nothing to keep it inside
// that window is dropped by the build that ends it (publish walks them all).
// Callers hold h.mu.
func (h *ackedPins) forgetIfGoneLocked(name string, s *clusterAck) {
	if h.build-s.build >= goneBuilds && !s.holds && len(s.sent) == 0 {
		delete(h.clusters, name)
	}
}

// deliver records the clusters of a response entering or leaving flight
// (ack.Delivery), so that the class a version has on record when its response
// is written is kept for as long as its ACK can come. A cluster with no record
// at that moment is not recorded (acceptLocked says what that leaves out).
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

// acceptLocked applies what one answered cluster response says the proxy
// holds. Callers hold h.mu and report the update before they release it
// (ackedPins).
func (h *ackedPins) acceptLocked(a ack.Accepted) ackedPinsUpdate {
	if a.Opening {
		h.restateLocked(a.Stated)
	}
	// A name with no record here is a cluster the response carried that is
	// not a cluster entry (a per-pod application cluster, a QUIC twin, the
	// passthrough cluster, a plaintext UDP floor), and is in no pin count. Or
	// it is an entry that left the snapshot more than goneBuilds builds
	// before the response that carried it was written, which is the bound
	// goneBuilds states; the proxy's next opening statement names it, and
	// restateLocked does not take a name on trust.
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
//
// A stated name with no record is not thereby a cluster the pin gauges do not
// count.
// The proxy can hold a cluster entry this agent has dropped the record of: one
// it accepted on a stream that ended before the agent read the answer, removed
// from the snapshot since, and stated again more than goneBuilds builds later
// (or within them, when the builds went by while the opening response waited
// for its answer). Its class is not on record, so it is held at a version of
// unknown class, which withdraws the gauge until the proxy accepts its
// removal, states it no more, or the agent publishes that version again: a
// count without it would say the proxy holds fewer unpinned clusters than it
// may.
//
// What tells the two apart is what the agent publishes now. A stated name the
// newest snapshot publishes and that has no record is not a cluster the pin
// gauges count: every entry of that snapshot that can carry a pin has its
// record (the plaintext UDP floor has none, and no pin). One it does not
// publish is taken for an entry unless its name says it is of a family the
// pin gauges do not count (proxy.ClusterNameOutsidePinGauge, the one list of
// them, held to every cluster constructor of that package by a test there).
// The agent errs to "unknown" for a name the list does not know.
func (h *ackedPins) restateLocked(stated map[string]string) {
	for name, version := range stated {
		if h.clusters[name] != nil {
			continue
		}
		if _, published := h.published[name]; published || proxy.ClusterNameOutsidePinGauge(name) {
			continue
		}
		// build stays zero: an entry of no snapshot this record knows.
		h.clusters[name] = &clusterAck{holds: true, held: version}
	}
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
// cluster at a version this agent process has no pin class on record for, and
// the acknowledged gauge stops being written; ackedClusterPinsKnownMsg when
// that ends. It does not say "a version this agent did not publish": this
// process may have published and sent it, and dropped the record since
// (clusterAck.heldKnown lists the ways).
const (
	ackedClusterPinsUnknownMsg = "proxy holds mesh clusters whose pin state this agent cannot determine; the acknowledged pin gauge is not written"
	ackedClusterPinsKnownMsg   = "the pin state of every mesh cluster this agent has on record as held by the proxy can be determined again; the acknowledged pin gauge is written"
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
// A version is a hash of the cluster's bytes, so "the proxy accepted version
// V" is "the proxy accepted exactly the cluster this agent published as V",
// and its pin class is the one the last snapshot that published V counted it
// under.
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
//   - The class of a cluster the proxy states and the agent neither publishes
//     nor has a record of (restateLocked): the same, "not written", unless its
//     name is of a family the pin gauges do not count. A cluster the agent does
//     publish that is not a cluster entry is in no count.
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
// is published as an entry, in the published gauge. It is in this one only
// while a proxy holds its cluster from an earlier snapshot: a floor that was
// published, left the capture set, and whose removal the proxy has not
// accepted stays counted at the version the proxy accepted
// (TestAckedPinGaugeKeepsAFloorWhoseRemovalWasRejected). One whose cluster was
// never published, or whose removal the proxy accepted, is not in this gauge.
//
// It logs when the acknowledged counts CHANGE, and when the state becomes
// unknown or known again; a steady state is silent however many updates are
// acknowledged in it.
//
// Safe to call from the xDS stream's goroutine: it takes the acknowledged
// state's own mutex and, under it, only the gauge's leaf mutex and the log
// handler; never snapshotMu or clusterMu. It walks every cluster record once
// (BenchmarkAckedPinsAccept measures it: tens of microseconds at 2,000
// cluster entries on a workstation, growing with the number of entries).
//
// The state change and its report are one critical section, so that the gauge
// is written in the order the state changed (ackedPins).
func (c *SnapshotCache) ClustersAccepted(ctx context.Context, accepted ack.Accepted) {
	c.acked.mu.Lock()
	defer c.acked.mu.Unlock()
	c.reportAckedPins(ctx, c.acked.acceptLocked(accepted), accepted.SystemVersion)
}

// publishAckedPins records the cluster entries of the snapshot about to be set
// (ackedPins.publishLocked) and reports what that changed, in one critical
// section like ClustersAccepted.
func (c *SnapshotCache) publishAckedPins(ctx context.Context, pins pinReport, versions map[string]string, version string) {
	c.acked.mu.Lock()
	defer c.acked.mu.Unlock()
	c.reportAckedPins(ctx, c.acked.publishLocked(pins.classes, versions, pins.promoted), version)
}

// reportAckedPins writes one change of the acknowledged pin state to the
// gauge and the log. version is the snapshot whose build or whose response
// produced it. Callers hold c.acked.mu, from before the change was made.
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
