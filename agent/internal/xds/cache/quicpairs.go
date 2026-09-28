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
	// not QUIC-enabled / not in the dependency set, or a source that is not a
	// local ServiceAccount. Nothing is recorded; the proxy's paused request
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
	QUICRefusedNotQUICService  = "destination_not_quic_enabled"
	QUICRefusedNotInDependency = "destination_not_in_dependency_set"
	QUICRefusedSourceNotOnNode = "source_not_local"
)

// Prune reasons, as logged.
const (
	quicPairPruneReasonSource = "source_left_node"
	quicPairPruneReasonDest   = "destination_not_quic_enabled"
	quicPairPruneReasonDepSet = "destination_left_dependency_set"

	// quicPairsLogCap bounds how many pairs one prune log line names.
	quicPairsLogCap = 50
)

// ObserveQUICTwin handles the node proxy's on-demand CDS request for a
// `quic:<svc>.<ns>.<domain>@<ns>/<sa>` twin (issue #1020).
//
// Every local ServiceAccount has a selection arm on a QUIC-enabled
// destination's routes, but the twin behind the arm is built only once that
// source has actually dialled the destination: the first request finds no
// such cluster and Envoy's on_demand filter asks for it by name. This
// validates the name -- the destination must be QUIC-enabled and in the
// dependency set, the source must be a ServiceAccount with a pod on this node
// -- records the pair in the persisted demand set and regenerates the
// snapshot, which then carries the twin and its own load assignment together
// (the #1008 rule), answering the subscription.
//
// The regeneration runs on its own goroutine: this is called from the xDS
// stream's request callback, and publishing a snapshot from inside it could
// block on the very stream whose request is being processed.
func (c *SnapshotCache) ObserveQUICTwin(ctx context.Context, name string) (QUICTwinDecision, string) {
	decision, reason := c.recordQUICPair(name)
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

// ResumeQUICSubscriptions admits the pairs behind twins the node proxy
// re-subscribed by name on a fresh xDS stream (issue #1033): names it holds a
// live on-demand subscription for, each opened by a request that routed to the
// twin. Envoy's ODCDS manager keeps such a subscription for the life of the
// process and never re-sends it, so a valid pair that is not served here is
// stranded -- every request 503s at the on_demand timeout -- rather than
// re-fetched. Same validation as ObserveQUICTwin, silent on refusal; admitted
// and known pairs are marked fetched, so the unfetched-pair prune keeps them.
// Returns how many pairs were new; those are published with one regeneration,
// off the caller's goroutine (see ObserveQUICTwin).
//
// A twin the proxy merely HOLDS (initial_resource_versions without a
// subscription) admits nothing: it is whatever an older agent generation
// built. #1032 admitted those too (RestoreQUICTwin), which on the first talos
// deploy (rev245) persisted every SAs x destinations twin as a pair.
func (c *SnapshotCache) ResumeQUICSubscriptions(ctx context.Context, names []string) int {
	added := 0
	for _, name := range names {
		if d, _ := c.recordQUICPair(name); d == QUICTwinAdded {
			added++
		}
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
func (c *SnapshotCache) recordQUICPair(name string) (QUICTwinDecision, string) {
	service, source, ok := proxy.ParseQUICClusterName(name, c.meshDomain)
	if !ok {
		return QUICTwinRefused, QUICRefusedMalformed
	}
	// Local identities first, under localMu alone: localMu and depMu never nest.
	local := c.localSourceSAKeys()
	if _, ok := local[source]; !ok {
		return QUICTwinRefused, QUICRefusedSourceNotOnNode
	}

	c.depMu.Lock()
	defer c.depMu.Unlock()
	if _, ok := c.quicServices[service]; !ok {
		return QUICTwinRefused, QUICRefusedNotQUICService
	}
	if _, ok := c.dependencySetLocked()[service]; !ok {
		return QUICTwinRefused, QUICRefusedNotInDependency
	}
	p := quicPair{service: service, source: source}
	c.quicFetched[p] = struct{}{}
	if _, known := c.quicPairs[p]; known {
		return QUICTwinKnown, ""
	}
	c.quicPairs[p] = time.Now()
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
// local identities and allow-list, then returns copies of the allow-list and
// the surviving pairs for one snapshot.
func (c *SnapshotCache) quicDemandSnapshot(identities []string) (map[string]struct{}, map[quicPair]struct{}) {
	local := sourceSAKeys(identities)

	c.depMu.Lock()
	pruned := c.pruneQUICPairsLocked(local)
	services := maps.Clone(c.quicServices)
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
	return services, pairs
}

// pruneQUICPairsLocked drops the pairs whose twin can no longer be built or
// selected: the destination is no longer QUIC-enabled or no longer in the
// dependency set, or -- once the local pod set is known -- the source
// ServiceAccount has no pod on this node. There is deliberately NO idle
// expiry: nothing in the xDS protocol tells the agent a twin stopped
// carrying traffic, and a wrongly pruned pair costs its next request one
// ODCDS round trip, so the rule prunes only on removal evidence. (The one
// time-based rule is the bounded post-start prune of persisted pairs that are
// never fetched, PruneUnfetchedQUICPairs, issue #1033.) Returns the
// pruned pairs as "<svc> <- <source>" strings. Caller must hold depMu for
// writing.
func (c *SnapshotCache) pruneQUICPairsLocked(local map[string]struct{}) []string {
	if len(c.quicPairs) == 0 {
		return nil
	}
	deps := c.dependencySetLocked()
	var pruned []string
	for p := range c.quicPairs {
		reason := ""
		switch {
		case !has(c.quicServices, p.service):
			reason = quicPairPruneReasonDest
		case !has(deps, p.service):
			reason = quicPairPruneReasonDepSet
		case c.localPodsSynced && !has(local, p.source):
			reason = quicPairPruneReasonSource
		default:
			continue
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

// PruneUnfetchedQUICPairs drops the persisted QUIC pairs that have had no
// on-demand fetch since this agent started, once the fetch window has elapsed
// (issue #1033). Called from the refresher's prune tick.
//
// Why it exists: the #1032 agent admitted a pair for every twin the proxy
// re-stated on a fresh stream, so the first talos deploy persisted the whole
// SAs x destinations fan-out on every node, and pairs prune otherwise only on
// removal evidence (source left the node, destination delisted). The agent has
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
		if _, fetched := c.quicFetched[p]; fetched {
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
	pairs := slices.SortedFunc(maps.Keys(c.quicPairs), func(a, b quicPair) int {
		return cmp.Or(cmp.Compare(a.service, b.service), cmp.Compare(a.source, b.source))
	})
	out := make([]*agentv1.ObservedQUICPair, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, agentv1.ObservedQUICPair_builder{
			Service:    p.service,
			Source:     p.source,
			ObservedAt: timestamppb.New(c.quicPairs[p]),
		}.Build())
	}
	return out
}

// admitStoredQUICPairs merges persisted pairs into quicPairs (union, never
// override). A malformed entry is skipped. Validation against the allow-list
// and the local pods is left to the snapshot-time prune: at restore the pod
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
