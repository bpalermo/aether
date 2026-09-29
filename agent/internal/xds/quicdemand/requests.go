// Package quicdemand sorts the `quic:` twin names in the node proxy's delta
// CDS requests into the three things they can mean (issues #1020, #1033).
//
// An Envoy that reconnects on a fresh xDS stream (every agent restart) re-states
// its inventory in the stream's first CDS request. Two different inventories
// ride that request, and they must be treated differently:
//
//   - initial_resource_versions lists every cluster the proxy HOLDS, including
//     twins it received through the wildcard CDS subscription -- which is how
//     the pre-#1020 agent delivered its SAs x destinations fan-out, built up
//     front with no request behind any of them. A held twin is not demand. The
//     #1032 agent re-admitted every one as a pair (RestoreQUICTwin), so on its
//     first talos deploy (rev245, 2026-09-28) every node logged
//     `quic_clusters=N observed_pairs=N local_identities=N/2` and persisted the
//     fan-out for good. A held-only twin the agent does not serve is answered
//     absent: go-control-plane seeds the stream's returned-resource map from
//     initial_resource_versions, so it goes out in removed_resources on the
//     first response and Envoy drops the cluster. Envoy holds no on-demand
//     subscription for it, so the next request that routes to it opens one --
//     a real first use.
//
//   - resource_names_subscribe re-subscribes every name the proxy holds an
//     on-demand (ODCDS) subscription for. Each one exists because a request
//     routed to that twin at some point in this proxy's life -- it is demand
//     evidence -- and, decisively, Envoy never re-sends it: its ODCDS manager
//     keeps one singleton subscription per name for the life of the process and
//     answers every later on-demand request for the name with "already
//     subscribed, skipping" (od_cds_api_impl.cc, XdstpOdcdsSubscriptionsManager).
//     Answering such a name absent does not make Envoy drop the subscription;
//     it strands the pair: every request 503s at the on_demand timeout until the
//     proxy restarts (seen live in //test/mtlspool). So a re-subscribed twin is
//     served if the pair is valid.
//
// A named subscription on a LATER request of the stream, for a twin the proxy
// does not hold, is the on_demand HTTP filter asking for a twin a request just
// routed to: first use.
//
// It is its own package so the live gate (//test/mtlspool) runs the agent's
// classification rather than a copy of it.
package quicdemand

import (
	"slices"
	"sync"

	"aethermesh.dev/agent/internal/xds/proxy"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

// Classification is what one delta CDS request says about `quic:` twins.
// Every list is sorted and de-duplicated.
type Classification struct {
	// FirstUse are twins subscribed by name on a request after the stream's
	// first CDS request and not held: a request just routed to each. They
	// admit their pair; a refusal 503s the request and is counted.
	FirstUse []string
	// Resubscribed are twins the proxy holds a live on-demand subscription
	// for, re-stated in the stream's first CDS request (or subscribed while
	// held). Envoy never re-sends them, so a valid pair is admitted and served;
	// refusals are silent.
	Resubscribed []string
	// HeldOnly are twins the proxy holds (initial_resource_versions) without an
	// on-demand subscription: delivered by the wildcard, e.g. built up front by
	// an older agent. Never demand; answered absent unless the pair is already
	// known.
	HeldOnly []string
	// Fresh is set on the stream's first CDS request: the proxy's on-demand
	// subscriptions are exactly Resubscribed (Ledger.Restate, issue #1036).
	// A hot-restart child's first request is fresh and names none.
	Fresh bool
}

// Requests tracks, per xDS stream, whether the stream's first CDS request has
// been seen. Safe for concurrent use; one per xDS server.
type Requests struct {
	mu   sync.Mutex
	seen map[int64]struct{}
}

// NewRequests returns an empty tracker.
func NewRequests() *Requests {
	return &Requests{seen: make(map[int64]struct{})}
}

// Classify sorts the `quic:` names in one delta request. A non-CDS request
// classifies as empty and does not count as the stream's first CDS request.
func (r *Requests) Classify(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) Classification {
	if req.GetTypeUrl() != resourcev3.ClusterType {
		return Classification{}
	}
	r.mu.Lock()
	_, seen := r.seen[streamID]
	r.seen[streamID] = struct{}{}
	r.mu.Unlock()
	first := !seen

	held := req.GetInitialResourceVersions()
	subscribed := map[string]struct{}{}
	out := Classification{Fresh: first}
	for _, name := range req.GetResourceNamesSubscribe() {
		if !proxy.IsQUICClusterName(name) {
			continue
		}
		subscribed[name] = struct{}{}
		if _, isHeld := held[name]; first || isHeld {
			out.Resubscribed = append(out.Resubscribed, name)
		} else {
			out.FirstUse = append(out.FirstUse, name)
		}
	}
	for name := range held {
		if _, sub := subscribed[name]; !sub && proxy.IsQUICClusterName(name) {
			out.HeldOnly = append(out.HeldOnly, name)
		}
	}
	// Deterministic: map order is protocol-visible downstream (#135).
	out.FirstUse = sortedUnique(out.FirstUse)
	out.Resubscribed = sortedUnique(out.Resubscribed)
	out.HeldOnly = sortedUnique(out.HeldOnly)
	return out
}

// Close forgets a stream: a reconnect is a new stream, whose first CDS request
// is again a re-statement.
func (r *Requests) Close(streamID int64) {
	r.mu.Lock()
	delete(r.seen, streamID)
	r.mu.Unlock()
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	slices.Sort(in)
	return slices.Compact(in)
}
