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
// which the agent learns in exactly one way: a fresh xDS stream (an agent
// restart, or a new proxy generation after a hot restart -- the child has no
// ODCDS subscriptions) re-states the proxy's subscriptions in its first CDS
// request, and a dormant pair that request does not name is gone (Restate).
//
// K is the caller's pair key. Not safe for concurrent use: the caller guards
// it (the snapshot cache with depMu, the live gate with its own mutex).
type Ledger[K comparable] struct {
	subscribed map[K]struct{}
	dormant    map[K]time.Time
}

// NewLedger returns an empty ledger.
func NewLedger[K comparable]() *Ledger[K] {
	return &Ledger[K]{subscribed: map[K]struct{}{}, dormant: map[K]time.Time{}}
}

// Subscribe records that the proxy holds an on-demand subscription for k: it
// asked for the twin by name (first use, even a refused one -- the
// subscription stays open either way), or re-subscribed it on a fresh stream.
func (l *Ledger[K]) Subscribe(k K) {
	l.subscribed[k] = struct{}{}
}

// Subscribed reports whether the proxy holds an on-demand subscription for k.
func (l *Ledger[K]) Subscribed(k K) bool {
	_, ok := l.subscribed[k]
	return ok
}

// Restate is a fresh stream's first CDS request: the proxy's on-demand
// subscriptions are now exactly resubscribed (quicdemand.Classification's
// Resubscribed). A dormant pair it does not name has no subscription left --
// the proxy restarted, or this is a new generation's stream -- and is pruned;
// the pruned keys are returned (unordered).
func (l *Ledger[K]) Restate(resubscribed []K) (pruned []K) {
	l.subscribed = make(map[K]struct{}, len(resubscribed))
	for _, k := range resubscribed {
		l.subscribed[k] = struct{}{}
	}
	for k := range l.dormant {
		if _, ok := l.subscribed[k]; !ok {
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
	if _, ok := l.subscribed[k]; !ok {
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
