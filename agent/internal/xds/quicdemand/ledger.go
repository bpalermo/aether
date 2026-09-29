package quicdemand

import (
	"maps"
	"time"
)

// Ledger remembers the `quic:` twins the node proxy holds an on-demand (ODCDS)
// subscription for, and keeps a pair whose twin had to leave the snapshot
// DORMANT instead of forgetting it (issue #1036).
//
// The protocol fact it is built on: Envoy's ODCDS manager keeps one
// subscription per cluster name for the life of the process. When the control
// plane removes a cluster that was fetched on demand, Envoy drops the CLUSTER
// and keeps the SUBSCRIPTION; every later on-demand request for the name is
// "already subscribed, skipping" and 503s NC at the on_demand timeout, until
// the proxy restarts. So a pair whose source ServiceAccount left the node (or
// whose destination left the dependency set) cannot simply be forgotten: when the source
// comes back -- a Deployment roll does this constantly -- nothing will ever
// ask for the twin again. The same open subscription is also the cure: its
// delta watch is still on the stream, so the control plane can push the twin
// again at any time and Envoy takes it WITHOUT a new request. A dormant pair
// is republished the moment it is valid again.
//
// A dormant pair is dropped only when Envoy no longer holds its subscription,
// which the agent learns from the xDS streams themselves:
//
//   - A fresh stream (an agent restart, a reconnect, or a new proxy generation
//     after a hot restart -- the child has no ODCDS subscriptions) re-states its
//     process's subscriptions in its first CDS request (Restate).
//   - A stream that ends while another one is live belongs to a proxy
//     generation that exited, or to one about to reconnect and re-state (Close).
//
// Subscriptions are held PER STREAM (issue #1052). One Envoy process speaks one
// ADS stream at a time, and the node proxy's Node carries nothing that tells two
// generations apart (same --service-node, same bootstrap, no restart epoch), so
// the stream is the proxy generation as the agent sees it. Two streams are live
// at once exactly across a hot restart: the draining parent and the child. A
// pair is subscribed -- and may stay dormant -- only while a live stream
// re-subscribed or requested it. Before #1052 the ledger kept one process-wide
// set that every fresh stream REPLACED, so after an agent restart that landed
// mid hot restart the order of the two reconnects decided the outcome: the
// child's stream (naming nothing) pruned the dormant pairs, the parent's stream
// eleven seconds later (naming them) parked them again, and once the parent
// exited the ledger held dormant pairs no live proxy was subscribed to -- the
// republish-on-return premise broken (rev248 main-worker-03).
//
// When the LAST live stream ends nothing is concluded: the agent cannot tell a
// proxy that exited from one that is reconnecting. Its subscriptions are kept,
// orphaned, until the next fresh stream re-states (and replaces) them -- the
// pre-#1052 behaviour for a single stream. Dormant pairs restored from local
// storage at agent start are held by no stream until one re-subscribes them.
//
// K is the caller's pair key. Not safe for concurrent use: the caller guards
// it (the snapshot cache with depMu, the live gate with its own mutex).
type Ledger[K comparable] struct {
	// streams is each live stream's on-demand subscriptions: what it
	// re-subscribed on its first CDS request plus what it asked for since.
	streams map[int64]map[K]struct{}
	// orphaned is the subscriptions of the last stream to end, held until the
	// next fresh stream re-states them. Empty while any stream is live.
	orphaned map[K]struct{}
	dormant  map[K]time.Time
}

// NewLedger returns an empty ledger.
func NewLedger[K comparable]() *Ledger[K] {
	return &Ledger[K]{streams: map[int64]map[K]struct{}{}, orphaned: map[K]struct{}{}, dormant: map[K]time.Time{}}
}

// Subscribe records that the proxy generation on stream holds an on-demand
// subscription for k: it asked for the twin by name (first use, even a refused
// one -- the subscription stays open either way), or re-subscribed it on a
// fresh stream.
func (l *Ledger[K]) Subscribe(stream int64, k K) {
	subs, ok := l.streams[stream]
	if !ok {
		subs = map[K]struct{}{}
		l.streams[stream] = subs
	}
	subs[k] = struct{}{}
}

// Subscribed reports whether a live proxy generation holds an on-demand
// subscription for k (or, with no stream live, whether the last one did).
func (l *Ledger[K]) Subscribed(k K) bool {
	if _, ok := l.orphaned[k]; ok {
		return true
	}
	for _, subs := range l.streams {
		if _, ok := subs[k]; ok {
			return true
		}
	}
	return false
}

// Restate is a fresh stream's first CDS request: that stream's proxy process
// holds exactly resubscribed (quicdemand.Classification's Resubscribed). It
// replaces the orphaned subscriptions -- the stream re-stating them is the
// proxy that reconnected, or its successor -- and a dormant pair no live
// stream holds is pruned; the pruned keys are returned (unordered). A dormant
// pair another live generation still holds (the draining parent of a hot
// restart) is kept until that stream ends (Close).
func (l *Ledger[K]) Restate(stream int64, resubscribed []K) (pruned []K) {
	subs := make(map[K]struct{}, len(resubscribed))
	for _, k := range resubscribed {
		subs[k] = struct{}{}
	}
	l.streams[stream] = subs
	l.orphaned = map[K]struct{}{}
	return l.pruneUnheld()
}

// Close is the end of stream. If another stream is live, the ended one was a
// proxy generation that exited (the draining parent of a hot restart) or one
// whose process will re-state on a new stream: either way it vouches for no
// subscription any more, so its holdings are DROPPED -- not parked -- and a
// dormant pair no live stream holds is pruned; the pruned keys are returned
// (unordered). If it was the last live stream its subscriptions are orphaned
// instead, until the next fresh stream re-states them (Restate). A stream the
// ledger never saw is ignored.
func (l *Ledger[K]) Close(stream int64) (pruned []K) {
	subs, ok := l.streams[stream]
	if !ok {
		return nil
	}
	delete(l.streams, stream)
	if len(l.streams) == 0 {
		l.orphaned = subs
		return nil
	}
	return l.pruneUnheld()
}

// pruneUnheld drops the dormant pairs no live stream (nor the orphaned set)
// holds a subscription for, and returns them.
func (l *Ledger[K]) pruneUnheld() (pruned []K) {
	for k := range l.dormant {
		if !l.Subscribed(k) {
			delete(l.dormant, k)
			pruned = append(pruned, k)
		}
	}
	return pruned
}

// Retire is called when k's twin must leave the snapshot (its source left the
// node, or its destination left the dependency set). If the proxy
// holds a subscription for it, k is kept dormant (with at, its first-observed
// time, for persistence) and Retire reports true; otherwise it is forgotten:
// the proxy drops a cluster it holds without a subscription outright, and its
// next request opens one -- real first use.
func (l *Ledger[K]) Retire(k K, at time.Time) bool {
	if !l.Subscribed(k) {
		return false
	}
	l.dormant[k] = at
	return true
}

// Park records k dormant directly: a subscribed name whose pair is not valid
// right now (a refused first use, a re-subscription for a source not on the
// node), or a dormant pair restored from local storage. An existing entry
// keeps its time.
func (l *Ledger[K]) Park(k K, at time.Time) {
	if _, ok := l.dormant[k]; !ok {
		l.dormant[k] = at
	}
}

// IsDormant reports whether k is dormant.
func (l *Ledger[K]) IsDormant(k K) bool {
	_, ok := l.dormant[k]
	return ok
}

// Revive removes and returns (unordered, with their times) the dormant pairs
// valid reports can be served again. The caller republishes their twins in
// the next snapshot; the proxy's open subscription receives them with no
// request.
func (l *Ledger[K]) Revive(valid func(K) bool) map[K]time.Time {
	var out map[K]time.Time
	for k := range l.dormant {
		if !valid(k) {
			continue
		}
		if out == nil {
			out = map[K]time.Time{}
		}
		out[k], _ = l.Wake(k)
	}
	return out
}

// Wake removes k from the dormant set and returns its time, if it was dormant:
// the caller is serving k again.
func (l *Ledger[K]) Wake(k K) (time.Time, bool) {
	at, ok := l.dormant[k]
	if ok {
		delete(l.dormant, k)
	}
	return at, ok
}

// Dormant returns a copy of the dormant pairs and their times.
func (l *Ledger[K]) Dormant() map[K]time.Time {
	return maps.Clone(l.dormant)
}
