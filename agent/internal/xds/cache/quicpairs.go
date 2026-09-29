package cache

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	agentv1 "aethermesh.dev/api/aether/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// quicPair is one unit of observed east-west QUIC demand (issue #1020): a
// destination service ("<ns>/<svc>") and the local ServiceAccount
// ("<ns>/<sa>") that dialled it. The agent builds a `quic:` twin only for an
// observed pair.
type quicPair struct {
	service string
	source  string
}

// QUICTwinDecision is what the agent did with an on-demand request for a
// `quic:` twin.
type QUICTwinDecision int

const (
	// QUICTwinRefused: the name is malformed, or names a destination that is
	// not in the dependency set, or a source that is not a local
	// ServiceAccount. Nothing is recorded; the proxy's paused request
	// fails at the on_demand timeout (503 NC).
	QUICTwinRefused QUICTwinDecision = iota
	// QUICTwinKnown: the pair was already observed; the twin is (or is about
	// to be) in the snapshot.
	QUICTwinKnown
	// QUICTwinAdded: a new pair; a snapshot carrying its twin and the twin's
	// load assignment is being generated.
	QUICTwinAdded
)

// Refusal reasons for an on-demand `quic:` request, as logged and counted.
const (
	QUICRefusedMalformed       = "malformed_name"
	QUICRefusedNotInDependency = "destination_not_in_dependency_set"
	QUICRefusedSourceNotOnNode = "source_not_local"
)

// Prune reasons, as logged.
const (
	quicPairPruneReasonSource = "source_left_node"
	quicPairPruneReasonDepSet = "destination_left_dependency_set"

	// quicPairsLogCap bounds how many pairs one prune log line names.
	quicPairsLogCap = 50
)

// ObserveQUICTwin handles the node proxy's on-demand CDS request for a
// `quic:<svc>.<ns>.<domain>@<ns>/<sa>` twin (issue #1020).
//
// Every local ServiceAccount has a selection arm on every eligible
// destination's routes (east-west QUIC is unconditional, #979), but the twin behind the arm is built only once that
// source has actually dialled the destination: the first request finds no
// such cluster and Envoy's on_demand filter asks for it by name. This
// validates the name -- the destination must be in the dependency set, the source must be a ServiceAccount with a pod on this node
// -- records the pair in the persisted demand set and regenerates the
// snapshot, which then carries the twin and its own load assignment together
// (the #1008 rule), answering the subscription.
//
// The regeneration runs on its own goroutine: this is called from the xDS
// stream's request callback, and publishing a snapshot from inside it could
// block on the very stream whose request is being processed.
//
// streamID is the xDS stream that asked: the proxy generation that now holds
// the name's on-demand subscription (issue #1052).
func (c *SnapshotCache) ObserveQUICTwin(ctx context.Context, streamID int64, name string) (QUICTwinDecision, string) {
	decision, reason := c.recordQUICPair(streamID, name)
	switch decision {
	case QUICTwinRefused:
		c.log.InfoContext(ctx, "refusing on-demand QUIC twin", "cluster", name, "reason", reason)
	case QUICTwinAdded:
		c.log.InfoContext(ctx, "observed east-west QUIC pair (ODCDS); building its twin", "cluster", name)
		go func() {
			if err := c.generateSnapshot(context.WithoutCancel(ctx)); err != nil {
				c.log.Error("failed to publish the snapshot for an observed QUIC pair", "cluster", name, "error", err)
			}
		}()
	case QUICTwinKnown:
	}
	return decision, reason
}

// RestateQUICSubscriptions is a fresh xDS stream's first CDS request (issue
// #1036): names -- quicdemand's Resubscribed, possibly empty -- is then exactly
// the set of on-demand subscriptions that stream's proxy process holds. They
// are recorded for streamID alone (issue #1052): across a hot restart the
// draining parent and the child are two live streams, and only the stream that
// re-subscribed a twin vouches for its subscription. A dormant pair NO live
// stream holds -- the proxy restarted, or the only live stream is a new
// generation's, whose child holds no ODCDS subscriptions -- is pruned: the next
// request that routes to its twin opens a subscription, real first use. One
// that another live generation still holds stays dormant until that stream
// ends (CloseQUICStream). The named twins are then resumed
// (ResumeQUICSubscriptions), which keeps a named pair that is not servable
// right now dormant. Called on EVERY fresh stream. Returns how many pairs were
// new.
func (c *SnapshotCache) RestateQUICSubscriptions(ctx context.Context, streamID int64, names []string) int {
	pairs := make([]quicPair, 0, len(names))
	for _, name := range names {
		if service, source, ok := proxy.ParseQUICClusterName(name, c.meshDomain); ok {
			pairs = append(pairs, quicPair{service: service, source: source})
		}
	}
	c.depMu.Lock()
	pruned := c.quicLedger.Restate(streamID, pairs)
	if len(pruned) > 0 {
		c.markObservedDirtyLocked()
	}
	dormantLeft := len(c.quicLedger.Dormant())
	c.depMu.Unlock()
	if len(pruned) > 0 {
		c.log.InfoContext(ctx, "pruned dormant east-west QUIC pairs: no live proxy stream holds an on-demand subscription for their twins",
			"stream", streamID, "count", len(pruned), "dormant", dormantLeft, "pairs", capStrings(pairStrings(pruned), quicPairsLogCap))
	}
	return c.ResumeQUICSubscriptions(ctx, streamID, names)
}

// CloseQUICStream is the end of an xDS stream (issue #1052). If another stream
// is live, the ended one was a proxy generation that exited -- the draining
// parent of a hot restart, which after an agent restart may well have
// re-subscribed twins the child never held -- so the subscriptions it vouched
// for are dropped, not kept, and a dormant pair no live stream holds is pruned.
// If it was the last live stream nothing is concluded (the proxy may be
// reconnecting); the next fresh stream's re-statement decides. Returns how many
// dormant pairs were pruned.
func (c *SnapshotCache) CloseQUICStream(ctx context.Context, streamID int64) int {
	c.depMu.Lock()
	pruned := c.quicLedger.Close(streamID)
	if len(pruned) > 0 {
		c.markObservedDirtyLocked()
	}
	dormantLeft := len(c.quicLedger.Dormant())
	c.depMu.Unlock()
	if len(pruned) > 0 {
		c.log.InfoContext(ctx, "pruned dormant east-west QUIC pairs: the proxy generation that held their on-demand subscriptions ended its stream while a newer one is live",
			"stream", streamID, "count", len(pruned), "dormant", dormantLeft, "pairs", capStrings(pairStrings(pruned), quicPairsLogCap))
	}
	return len(pruned)
}

// ResumeQUICSubscriptions admits the pairs behind twins the node proxy
// re-subscribed by name (issue #1033): names it holds a live on-demand
// subscription for, each opened by a request that routed to the twin. Envoy's
// ODCDS manager keeps such a subscription for the life of the process and never
// re-sends it, so a valid pair that is not served here is stranded -- every
// request 503s at the on_demand timeout -- rather than re-fetched. Same
// validation as ObserveQUICTwin, silent on refusal; admitted and known pairs
// are marked fetched, so the unfetched-pair prune keeps them, and a refused
// well-formed name is kept dormant (issue #1036) so it is republished when it
// becomes valid. Returns how many pairs were new; those are published with one
// regeneration, off the caller's goroutine (see ObserveQUICTwin).
//
// A twin the proxy merely HOLDS (initial_resource_versions without a
// subscription) admits nothing: it is whatever an older agent generation
// built. #1032 admitted those too (RestoreQUICTwin), which on the first talos
// deploy (rev245) persisted every SAs x destinations twin as a pair.
func (c *SnapshotCache) ResumeQUICSubscriptions(ctx context.Context, streamID int64, names []string) int {
	added, parked := 0, 0
	for _, name := range names {
		switch d, reason := c.recordQUICPair(streamID, name); {
		case d == QUICTwinAdded:
			added++
		case d == QUICTwinRefused && reason != QUICRefusedMalformed:
			parked++
		}
	}
	if parked > 0 {
		c.log.InfoContext(ctx, "kept east-west QUIC pairs dormant: the proxy subscribes to their twins but the pair is not servable now; republished when it is",
			"count", parked)
	}
	if added > 0 {
		c.log.InfoContext(ctx, "resumed east-west QUIC pairs the proxy holds a live on-demand subscription for", "count", added)
		go func() {
			if err := c.generateSnapshot(context.WithoutCancel(ctx)); err != nil {
				c.log.Error("failed to publish the snapshot for resumed QUIC pairs", "error", err)
			}
		}()
	}
	return added
}

// HasQUICPair reports whether the pair behind a `quic:` twin name is in the
// observed set, i.e. whether the agent serves that twin. It records nothing.
func (c *SnapshotCache) HasQUICPair(name string) bool {
	service, source, ok := proxy.ParseQUICClusterName(name, c.meshDomain)
	if !ok {
		return false
	}
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	_, known := c.quicPairs[quicPair{service: service, source: source}]
	return known
}

// recordQUICPair validates a twin name and records its pair. It is reached
// only on evidence that a request routed to the twin -- an on-demand fetch
// (ObserveQUICTwin) or a live on-demand subscription re-stated on a fresh
// stream (ResumeQUICSubscriptions) -- so an admitted or already-known pair is
// also marked fetched in this process, which exempts it from the
// unfetched-pair prune (pruneUnfetchedQUICPairs).
//
// Either way the proxy now holds an on-demand subscription for the name, and
// keeps it for the life of the process (issue #1036), so the ledger records it
// even when the pair is refused: a well-formed name that is not servable right
// now is parked dormant and republished when it becomes valid -- its paused
// request still 503s, but the ones after it do not. The subscription is
// recorded for streamID, the proxy generation that holds it (issue #1052).
func (c *SnapshotCache) recordQUICPair(streamID int64, name string) (QUICTwinDecision, string) {
	service, source, ok := proxy.ParseQUICClusterName(name, c.meshDomain)
	if !ok {
		return QUICTwinRefused, QUICRefusedMalformed
	}
	// Local identities first, under localMu alone: localMu and depMu never nest.
	local := c.localSourceSAKeys()

	c.depMu.Lock()
	defer c.depMu.Unlock()
	p := quicPair{service: service, source: source}
	c.quicLedger.Subscribe(streamID, p)
	reason := ""
	switch {
	case !has(local, source):
		reason = QUICRefusedSourceNotOnNode
	case !has(c.dependencySetLocked(), service):
		reason = QUICRefusedNotInDependency
	}
	if reason != "" {
		if _, active := c.quicPairs[p]; !active && !c.quicLedger.IsDormant(p) {
			c.quicLedger.Park(p, time.Now())
			c.markObservedDirtyLocked()
		}
		return QUICTwinRefused, reason
	}
	c.quicFetched[p] = struct{}{}
	if _, known := c.quicPairs[p]; known {
		return QUICTwinKnown, ""
	}
	at := time.Now()
	if dormantAt, wasDormant := c.quicLedger.Wake(p); wasDormant {
		at = dormantAt
	}
	c.quicPairs[p] = at
	c.bumpDepGenLocked()
	c.markObservedDirtyLocked()
	return QUICTwinAdded, ""
}

// localSourceSAKeys returns the "<ns>/<sa>" keys of the local workload
// identities, under localMu.
func (c *SnapshotCache) localSourceSAKeys() map[string]struct{} {
	return sourceSAKeys(c.localWorkloadIdentities())
}

// sourceSAKeys reduces workload SPIFFE IDs to their "<ns>/<sa>" keys.
func sourceSAKeys(identities []string) map[string]struct{} {
	out := make(map[string]struct{}, len(identities))
	for _, id := range identities {
		if k := proxy.SourceSAKeyFromSpiffeID(id); k != "" {
			out[k] = struct{}{}
		}
	}
	return out
}

// splitByClientCertificate splits local workload identities into those whose
// client certificate -- the SDS secret a `quic:` twin names, keyed by the
// SPIFFE ID -- the cache holds, and those still waiting for SPIRE to deliver it
// (issue #1049). Only the first get QUIC selection arms and twins; see
// quicFanout. Takes secretMu alone (never nested in another lock here).
//
// With no secrets at all the SDS source is not serving yet, and every mTLS
// cluster on the node is equally blocked on it; nothing is held back then, so
// the fan-out keeps its pre-#1049 shape (and so do fixtures that do not model
// SDS). The gate is for the case that matters: SPIRE serving the node while a
// NEW identity's certificate is still in flight.
func (c *SnapshotCache) splitByClientCertificate(identities []string) (ready, awaiting []string) {
	c.secretMu.RLock()
	defer c.secretMu.RUnlock()
	if len(c.secrets) == 0 {
		return identities, nil
	}
	ready = make([]string, 0, len(identities))
	for _, id := range identities {
		if _, ok := c.secrets[id]; ok {
			ready = append(ready, id)
		} else {
			awaiting = append(awaiting, id)
		}
	}
	return ready, awaiting
}

// markLocalPodsSynced records that the node's pod records have been loaded, so
// a missing local ServiceAccount is now evidence its pairs' source has left.
func (c *SnapshotCache) markLocalPodsSynced() {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	if c.localPodsSynced {
		return
	}
	c.localPodsSynced = true
	c.bumpDepGenLocked()
}

// quicDemandSnapshot prunes the observed QUIC pairs against the current
// local identities and dependency set, then returns copies of the dependency
// set and of the surviving pairs for one snapshot. The dependency set is what
// makes a destination QUIC-eligible now that there is no allow-list (#979):
// armsFor gives arms only to its members, the same rule recordQUICPair admits
// by, so no route names a twin the agent would refuse.
func (c *SnapshotCache) quicDemandSnapshot(identities []string) (map[string]struct{}, map[quicPair]struct{}) {
	local := sourceSAKeys(identities)

	c.depMu.Lock()
	pruned := c.pruneQUICPairsLocked(local)
	revived := c.reviveDormantQUICPairsLocked(local)
	deps := maps.Clone(c.dependencySetLocked())
	var pairs map[quicPair]struct{}
	if len(c.quicPairs) > 0 {
		pairs = make(map[quicPair]struct{}, len(c.quicPairs))
		for p := range c.quicPairs {
			pairs[p] = struct{}{}
		}
	}
	c.depMu.Unlock()

	if len(pruned) > 0 {
		c.log.Info("pruned east-west QUIC pairs", "count", len(pruned), "pairs", capStrings(pruned, quicPairsLogCap))
	}
	if len(revived) > 0 {
		c.log.Info("republished dormant east-west QUIC pairs: the proxy still holds their on-demand subscriptions, so the twin is pushed with no request",
			"count", len(revived), "pairs", capStrings(revived, quicPairsLogCap))
	}
	return deps, pairs
}

// reviveDormantQUICPairsLocked moves every dormant pair that is servable again
// -- its destination in the dependency set, and its source
// ServiceAccount back on the node (judged only once the pod set is known) --
// into the observed set, so the snapshot being built republishes its twin
// (issue #1036). The proxy's on-demand subscription for the name never closed,
// so it receives the twin with no request; a pair forgotten instead would be
// stranded. Returns the revived pairs as "<svc> <- <source>" strings. Caller
// must hold depMu for writing.
func (c *SnapshotCache) reviveDormantQUICPairsLocked(local map[string]struct{}) []string {
	if !c.localPodsSynced {
		return nil
	}
	deps := c.dependencySetLocked()
	woken := c.quicLedger.Revive(func(p quicPair) bool {
		return has(deps, p.service) && has(local, p.source)
	})
	if len(woken) == 0 {
		return nil
	}
	revived := make([]string, 0, len(woken))
	for p, at := range woken {
		c.quicPairs[p] = at
		revived = append(revived, p.service+" <- "+p.source)
	}
	c.bumpDepGenLocked()
	c.markObservedDirtyLocked()
	slices.Sort(revived)
	return revived
}

// pruneQUICPairsLocked drops the pairs whose twin can no longer be built or
// selected: once the local pod set is known, the destination is no longer in
// the dependency set or the source ServiceAccount has no pod on this node.
// Both wait for the pod set: the declared half of the dependency set IS the
// pod records (their upstream annotations), so before they are loaded a
// destination's absence is not evidence -- a restored pair would otherwise be
// dropped by the first snapshot after an agent restart. (The allow-list forced
// its destinations into the set, which hid this until #979.) There is
// deliberately NO idle expiry: nothing in the xDS protocol tells the agent a twin stopped
// carrying traffic, and a wrongly pruned pair costs its next request one
// ODCDS round trip, so the rule prunes only on removal evidence. (The one
// time-based rule is the bounded post-start prune of persisted pairs that are
// never fetched, PruneUnfetchedQUICPairs, issue #1033.) A pruned pair whose
// twin the proxy holds an on-demand subscription for is kept DORMANT rather
// than forgotten (issue #1036): its twin leaves the snapshot all the same, and
// reviveDormantQUICPairsLocked republishes it when the pair is valid again.
// Returns the pruned pairs as "<svc> <- <source>" strings. Caller must hold
// depMu for writing.
func (c *SnapshotCache) pruneQUICPairsLocked(local map[string]struct{}) []string {
	if len(c.quicPairs) == 0 {
		return nil
	}
	deps := c.dependencySetLocked()
	var pruned []string
	for p := range c.quicPairs {
		reason := ""
		switch {
		case c.localPodsSynced && !has(deps, p.service):
			reason = quicPairPruneReasonDepSet
		case c.localPodsSynced && !has(local, p.source):
			reason = quicPairPruneReasonSource
		default:
			continue
		}
		if c.quicLedger.Retire(p, c.quicPairs[p]) {
			reason += ", dormant"
		}
		delete(c.quicPairs, p)
		delete(c.quicFetched, p)
		pruned = append(pruned, p.service+" <- "+p.source+" ("+reason+")")
	}
	if len(pruned) > 0 {
		c.bumpDepGenLocked()
		c.markObservedDirtyLocked()
	}
	slices.Sort(pruned)
	return pruned
}

// DefaultQUICPairFetchWindow is how long after an agent start a persisted QUIC
// pair may go without an on-demand fetch before it is pruned (issue #1033;
// --east-west-quic-pair-fetch-window).
const DefaultQUICPairFetchWindow = time.Hour

// SetQUICPairFetchWindow sets the unfetched-pair prune window (issue #1033).
// d <= 0 disables the prune. Boot-time, before the manager starts.
func (c *SnapshotCache) SetQUICPairFetchWindow(d time.Duration) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	c.quicFetchWindow = d
}

// SetQUICIdleTimeout sets the `quic:` twins' pool idle timeout
// (--east-west-quic-idle-timeout, aether#1054); d <= 0 means
// config.DefaultQUICTwinIdleTimeout. Boot-time, before the manager starts.
// h1/h2 clusters keep config.UpstreamIdleTimeout whatever this is.
func (c *SnapshotCache) SetQUICIdleTimeout(d time.Duration) {
	c.quicIdleTimeout.Store(int64(d))
}

// PruneUnfetchedQUICPairs drops the persisted QUIC pairs that have had no
// on-demand fetch since this agent started, once the fetch window has elapsed
// (issue #1033). Called from the refresher's prune tick.
//
// Why it exists: the #1032 agent admitted a pair for every twin the proxy
// re-stated on a fresh stream, so the first talos deploy persisted the whole
// SAs x destinations fan-out on every node, and pairs prune otherwise only on
// removal evidence (source left the node, destination left the dependency set). The agent has
// no traffic signal of its own -- it makes no admin calls, and a served twin
// is never fetched again -- so "fetched on demand since start" is the only
// evidence of use it can see. A pair first used in this process is marked
// fetched and kept, and so is one whose twin the proxy re-subscribed on this
// agent's stream (ResumeQUICSubscriptions): removing a twin Envoy holds an
// on-demand subscription for would strand it, because Envoy never re-requests
// a subscribed name. What is left -- a restored pair whose twin the proxy
// holds only through the wildcard, or not at all -- is kept for the window,
// then pruned with its twin. Envoy drops that cluster outright (it has no
// subscription for it), so a pruned pair that still carries traffic opens one
// on its next request: one ODCDS round trip (~20 ms), no 503, re-admitted as
// real first use. The cost of a wrong prune is bounded and once per agent start.
//
// Logs one line per node with the count when it prunes anything.
func (c *SnapshotCache) PruneUnfetchedQUICPairs() {
	c.pruneUnfetchedQUICPairs(time.Now())
}

func (c *SnapshotCache) pruneUnfetchedQUICPairs(now time.Time) {
	c.depMu.Lock()
	window := c.quicFetchWindow
	if window <= 0 || now.Sub(c.quicStart) < window || len(c.quicPairs) == 0 {
		c.depMu.Unlock()
		return
	}
	var pruned []string
	for p := range c.quicPairs {
		if _, fetched := c.quicFetched[p]; fetched || c.quicLedger.Subscribed(p) {
			continue
		}
		delete(c.quicPairs, p)
		pruned = append(pruned, p.service+" <- "+p.source)
	}
	if len(pruned) > 0 {
		c.bumpDepGenLocked()
		c.markObservedDirtyLocked()
	}
	remaining := len(c.quicPairs)
	c.depMu.Unlock()

	if len(pruned) == 0 {
		return
	}
	slices.Sort(pruned)
	c.log.Info("pruned persisted east-west QUIC pairs with no on-demand fetch since agent start",
		"count", len(pruned), "remaining", remaining, "window", window, "pairs", capStrings(pruned, quicPairsLogCap))
	c.signalDependencyChange()
}

func has(set map[string]struct{}, k string) bool {
	_, ok := set[k]
	return ok
}

// capStrings bounds a list for a log line.
func capStrings(in []string, limit int) []string {
	if len(in) <= limit {
		return in
	}
	return in[:limit]
}

// QUICPairs returns the observed pairs as sorted twin names -- what an
// operator compares with the proxy's `quic:` cluster count. For tests and
// debugging.
func (c *SnapshotCache) QUICPairs() []string {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	out := make([]string, 0, len(c.quicPairs))
	for p := range c.quicPairs {
		out = append(out, proxy.QUICClusterName(p.service, c.meshDomain, p.source))
	}
	slices.Sort(out)
	return out
}

// quicPairsLocked encodes the observed pairs in (service, source) order for
// persistence. Caller must hold depMu.
func (c *SnapshotCache) quicPairsLocked() []*agentv1.ObservedQUICPair {
	return encodeQUICPairs(c.quicPairs)
}

// dormantQUICPairsLocked encodes the dormant pairs (issue #1036) the same way.
// Caller must hold depMu.
func (c *SnapshotCache) dormantQUICPairsLocked() []*agentv1.ObservedQUICPair {
	return encodeQUICPairs(c.quicLedger.Dormant())
}

func encodeQUICPairs(set map[quicPair]time.Time) []*agentv1.ObservedQUICPair {
	pairs := slices.SortedFunc(maps.Keys(set), compareQUICPairs)
	out := make([]*agentv1.ObservedQUICPair, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, agentv1.ObservedQUICPair_builder{
			Service:    p.service,
			Source:     p.source,
			ObservedAt: timestamppb.New(set[p]),
		}.Build())
	}
	return out
}

func compareQUICPairs(a, b quicPair) int {
	return cmp.Or(cmp.Compare(a.service, b.service), cmp.Compare(a.source, b.source))
}

// pairStrings renders pairs as sorted "<svc> <- <source>" strings for a log line.
func pairStrings(pairs []quicPair) []string {
	slices.SortFunc(pairs, compareQUICPairs)
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.service+" <- "+p.source)
	}
	return out
}

// DormantQUICPairs returns the dormant pairs (issue #1036) as sorted twin
// names. For tests and debugging.
func (c *SnapshotCache) DormantQUICPairs() []string {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	dormant := c.quicLedger.Dormant()
	out := make([]string, 0, len(dormant))
	for p := range dormant {
		out = append(out, proxy.QUICClusterName(p.service, c.meshDomain, p.source))
	}
	slices.Sort(out)
	return out
}

// admitStoredQUICPairs merges persisted pairs into quicPairs (union, never
// override). A malformed entry is skipped. Validation against the dependency
// set and the local pods is left to the snapshot-time prune: at restore the pod
// records may not be loaded yet. Returns how many pairs were admitted and
// skipped.
func (c *SnapshotCache) admitStoredQUICPairs(entries []*agentv1.ObservedQUICPair) (admitted, skipped int) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	for _, e := range entries {
		name := proxy.QUICClusterName(e.GetService(), c.meshDomain, e.GetSource())
		svc, src, ok := proxy.ParseQUICClusterName(name, c.meshDomain)
		if !ok || svc != e.GetService() || src != e.GetSource() {
			skipped++
			continue
		}
		p := quicPair{service: svc, source: src}
		if _, known := c.quicPairs[p]; known {
			continue
		}
		at := time.Now()
		if ts := e.GetObservedAt(); ts != nil {
			at = ts.AsTime()
		}
		c.quicPairs[p] = at
		admitted++
	}
	if admitted > 0 {
		c.bumpDepGenLocked()
	}
	if skipped > 0 {
		c.markObservedDirtyLocked()
	}
	return admitted, skipped
}

// admitStoredDormantQUICPairs restores persisted dormant pairs (issue #1036)
// into the ledger. A pair that is also an observed pair stays observed; a
// malformed entry is skipped. A restored dormant pair whose source is on the
// node once the pod records are loaded is republished by the first snapshot
// after that (reviveDormantQUICPairsLocked), before any request; one whose
// source is still away stays dormant until the proxy's fresh stream either
// re-subscribes it (kept) or does not (pruned). Returns how many were admitted
// and skipped.
func (c *SnapshotCache) admitStoredDormantQUICPairs(entries []*agentv1.ObservedQUICPair) (admitted, skipped int) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	for _, e := range entries {
		name := proxy.QUICClusterName(e.GetService(), c.meshDomain, e.GetSource())
		svc, src, ok := proxy.ParseQUICClusterName(name, c.meshDomain)
		if !ok || svc != e.GetService() || src != e.GetSource() {
			skipped++
			continue
		}
		p := quicPair{service: svc, source: src}
		if _, active := c.quicPairs[p]; active || c.quicLedger.IsDormant(p) {
			continue
		}
		at := time.Now()
		if ts := e.GetObservedAt(); ts != nil {
			at = ts.AsTime()
		}
		c.quicLedger.Park(p, at)
		admitted++
	}
	if admitted > 0 {
		c.bumpDepGenLocked()
	}
	if skipped > 0 {
		c.markObservedDirtyLocked()
	}
	return admitted, skipped
}
