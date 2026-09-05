package cache

import (
	"context"
	"time"

	"aethermesh.dev/common/serviceref"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
)

// defaultObservedTTL is how long an observed (ODCDS-requested) dependency
// stays in the node dependency set without evidence it is still in use. A
// one-off call therefore doesn't pin a cluster forever; an idle undeclared
// upstream re-warms with one xDS round-trip after expiry — and shows up in
// the miss metric, the signal to promote it to a declared annotation.
//
// IDLE is the operative word (issue #682). The TTL used to be refreshed only
// by an ODCDS request, and an ODCDS request only happens on a MISS: once the
// cluster is warm the service's own vhost carries the traffic and the
// on_demand filter is never reached again. That made this a hard lifetime
// rather than an idle timeout — a node serving a cross-node upstream at 25 rps
// dropped it, on purpose, exactly one hour after it first asked for it. The
// live on-demand subscription set (onDemandSubs) closes that gap: an observed
// dependency the node's proxy still holds a subscription for is refreshed
// instead of expired.
const defaultObservedTTL = time.Hour

// podDependencies is one local pod's contribution to the node dependency set:
// the services the pod declares it consumes plus the pod's own service (a
// pod's service is trivially "depended on" by its inbound traffic).
type podDependencies struct {
	// service is the pod's own service name (its service account).
	service string
	// upstreams are the declared upstream services
	// (config.aether.io/upstreams annotation).
	upstreams []string
}

// setPodDependencies records a local pod's declared upstreams and own service
// into the node dependency set, keyed by the pod's network namespace. If the
// effective dependency set changed, a (coalesced) change signal is emitted so
// the registry refresher rebuilds the scoped cluster snapshot.
func (c *SnapshotCache) setPodDependencies(netns string, cniPod *cniv1.CNIPod) {
	deps := podDependencies{
		// 020 Part 1: the dependency set is keyed by the namespace-qualified
		// "<ns>/<svc>" service key (matching the registry / cluster map), so the
		// pod's own service is keyed by <ns>/<sa>, not the bare ServiceAccount.
		service:   serviceref.New(cniPod.GetNamespace(), cniPod.GetServiceAccount()).Key(),
		upstreams: proxy.UpstreamsFromPod(cniPod),
	}

	c.depMu.Lock()
	before := c.dependencySetLocked()
	c.podDeps[netns] = deps
	c.bumpDepGenLocked()
	after := c.dependencySetLocked()
	c.depMu.Unlock()

	if !equalSets(before, after) {
		c.signalDependencyChange()
	}
}

// removePodDependencies drops a local pod's contribution to the node
// dependency set and signals a change if the effective set shrank.
func (c *SnapshotCache) removePodDependencies(netns string) {
	c.depMu.Lock()
	before := c.dependencySetLocked()
	delete(c.podDeps, netns)
	c.bumpDepGenLocked()
	after := c.dependencySetLocked()
	c.depMu.Unlock()

	if !equalSets(before, after) {
		c.signalDependencyChange()
	}
}

// ObserveDependency records an on-demand (ODCDS) request for a service that
// is not in the node dependency set: the observed cold path. The service
// joins the set with a TTL'd membership (refreshed on re-request) and a
// change signal triggers the scoped reload that delivers the cluster,
// resuming the paused request. A request for an already-known service only
// refreshes its observation timestamp. Returns true when the dependency is
// new (a miss).
//
// The observer pairs this with TrackOnDemandCluster: the subscription the
// request arrived on is what keeps the entry alive past the idle TTL while the
// proxy is still routing to it (issue #682).
func (c *SnapshotCache) ObserveDependency(ctx context.Context, service string) bool {
	if known, recorded := c.recordObservation(service); !recorded || known {
		return false
	}

	c.log.InfoContext(ctx, "observed undeclared upstream (ODCDS miss); adding to node dependency set",
		"service", service, "ttl", c.observedTTLValue().String())
	c.metrics.UpstreamMiss(ctx, service)
	c.signalDependencyChange()
	return true
}

// RestoreDependency re-admits a service the node's proxy ALREADY holds a cluster
// for, as reported in the initial_resource_versions of the first delta request
// on a fresh xDS stream. Returns true when the dependency is new to this
// process. Same TTL'd membership as an observation, but it is deliberately NOT a
// miss: the proxy is not asking for something new, the agent is recovering
// demand it had already granted.
//
// Why this exists (issue #682, the ~20s post-restart echo outage on nodes with
// no local replica, 2026-09-05 rev194): the observed half of the dependency set
// is process-local memory, so a restarted agent's first snapshot legitimately
// drops every ODCDS-acquired upstream, and Envoy 503s (NC) on the next request.
// The cold path is supposed to repair that in one round trip — except a
// reconnecting Envoy never re-asks. On a fresh stream Envoy marks every
// requested resource "waiting for server", and its first request carries
// initial_resource_versions for the resources it HOLDS plus
// resource_names_subscribe for names newly added since — which, after
// markStreamFresh clears the pending-add set, is nothing. A name it is still
// waiting on appears in NEITHER field, and its on_demand filter dedupes every
// later re-subscribe for a name already in that state, so no ODCDS request
// reaches the new agent at all. On talos the outage ended only when Envoy's
// 15s init-fetch timeout tore that subscription state down and the next request
// re-subscribed from scratch: 14.05s (w01) / 14.67s (w03) of 503s, ~7 rounds of
// the 2s on-demand timeout, with the agent logging nothing.
//
// The held-resource inventory is the evidence the agent was missing: it is the
// proxy stating, in the protocol, exactly which clusters it is still running on.
// Restoring from it re-seeds the demand set in the first request of the stream —
// before the first snapshot push — so the upstream never leaves the snapshot.
// Restored entries are plain observations, NOT live-subscription pins: anything
// the proxy no longer routes to decays on the ordinary idle TTL, so demand
// scoping still shrinks after a restart.
func (c *SnapshotCache) RestoreDependency(ctx context.Context, service string) bool {
	if known, recorded := c.recordObservation(service); !recorded || known {
		return false
	}

	c.log.DebugContext(ctx, "restored observed upstream from the proxy's held clusters", "service", service)
	c.signalDependencyChange()
	return true
}

// recordObservation stamps an observation for service, reporting whether the
// service was already in the dependency set and whether anything was recorded
// at all (an empty name records nothing).
func (c *SnapshotCache) recordObservation(service string) (known, recorded bool) {
	if service == "" {
		return false, false
	}
	c.depMu.Lock()
	defer c.depMu.Unlock()
	_, known = c.dependencySetLocked()[service]
	c.observedDeps[service] = time.Now()
	// Bump even on a pure timestamp refresh: the memoized expiry horizon
	// (depSetExpiry) may extend, and a stale horizon would expire this
	// entry from the served set too early. Persist for the same reason: the
	// refresh moved the entry's deadline (issue #701).
	c.bumpDepGenLocked()
	c.markObservedDirtyLocked()
	return known, true
}

// TrackOnDemandCluster records a live on-demand (ODCDS) cluster subscription
// from the node's proxy: streamID identifies the delta stream, clusterName the
// subscribed resource, service the dependency key it resolves to. It is the
// observed-USE signal behind the idle TTL (issue #682) — see onDemandSubs.
// Callers record only names that passed the catalog gate, so a ghost service
// can never pin a dependency.
func (c *SnapshotCache) TrackOnDemandCluster(streamID int64, clusterName, service string) {
	if clusterName == "" || service == "" {
		return
	}
	c.depMu.Lock()
	defer c.depMu.Unlock()
	subs := c.onDemandSubs[streamID]
	if subs == nil {
		subs = make(map[string]string, 1)
		c.onDemandSubs[streamID] = subs
	}
	if subs[clusterName] == service {
		return
	}
	subs[clusterName] = service
	c.bumpDepGenLocked()
}

// UntrackOnDemandCluster drops one on-demand subscription (the proxy explicitly
// unsubscribed from the name). The service reverts to plain idle expiry unless
// another live subscription still names it.
func (c *SnapshotCache) UntrackOnDemandCluster(streamID int64, clusterName string) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	subs := c.onDemandSubs[streamID]
	if _, ok := subs[clusterName]; !ok {
		return
	}
	delete(subs, clusterName)
	if len(subs) == 0 {
		delete(c.onDemandSubs, streamID)
	}
	c.bumpDepGenLocked()
}

// CloseOnDemandStream drops every on-demand subscription held by a delta stream
// that ended. The proxy re-establishes the ones it still needs from real demand
// on the new stream; anything nobody asks for again ages out on the idle TTL,
// which is what keeps demand scoping honest across a proxy or agent restart.
func (c *SnapshotCache) CloseOnDemandStream(streamID int64) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	if _, ok := c.onDemandSubs[streamID]; !ok {
		return
	}
	delete(c.onDemandSubs, streamID)
	c.bumpDepGenLocked()
}

// OnDemandServices returns the services the node's proxy currently holds a live
// on-demand subscription for — the observed-use set that exempts an observed
// dependency from the idle TTL. External callers get a fresh map.
func (c *SnapshotCache) OnDemandServices() map[string]struct{} {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	inUse := c.onDemandServicesLocked()
	if inUse == nil {
		return map[string]struct{}{}
	}
	return inUse
}

// onDemandServicesLocked returns the services the node's proxy currently holds
// a live on-demand subscription for. The returned map is freshly built, so it
// is safe to hand to callers. Caller must hold depMu.
func (c *SnapshotCache) onDemandServicesLocked() map[string]struct{} {
	if len(c.onDemandSubs) == 0 {
		return nil
	}
	inUse := make(map[string]struct{}, len(c.onDemandSubs))
	for _, subs := range c.onDemandSubs {
		for _, svc := range subs {
			inUse[svc] = struct{}{}
		}
	}
	return inUse
}

// PruneObservedDependencies expires observed dependencies idle past the TTL and
// signals a dependency change when any were dropped. The refresher calls it
// periodically; the subsequent scoped reload removes the expired clusters
// (after the retention grace), and Envoy re-fetches via ODCDS on next use.
//
// An entry the node's proxy still holds a live on-demand subscription for is
// REFRESHED rather than expired (issue #682): that subscription is the agent's
// evidence the upstream is still in use, and dropping it would be
// unrecoverable — Envoy dedupes a re-subscribe for a name it already believes
// it is waiting on, so no ODCDS request would ever come back to re-warm it.
// Genuinely idle upstreams — no traffic, hence no live subscription once the
// stream turns over — still expire, which is the demand scoping.
func (c *SnapshotCache) PruneObservedDependencies() {
	now := time.Now()
	ttl := c.observedTTLValue()

	c.depMu.Lock()
	expired, refreshed := c.pruneObservedLocked(now, ttl)
	if expired > 0 || refreshed > 0 {
		c.bumpDepGenLocked()
		// Both change the persisted set: an expiry removes its entry, a
		// refresh moves its deadline (issue #701).
		c.markObservedDirtyLocked()
	}
	c.depMu.Unlock()

	if refreshed > 0 {
		// Info, not Debug: this is the only trace the in-use exemption leaves.
		// Its entire effect is that nothing happens, so at Debug an operator
		// cannot distinguish a working exemption from a demand set that never
		// aged (issue #682 deployment validation). It fires at most once per
		// prune tick, and only when an entry actually crossed its TTL.
		c.log.Info("refreshed in-use observed upstreams (live on-demand subscription)", "count", refreshed)
		c.metrics.UpstreamTTLRefreshed(context.Background(), int64(refreshed))
	}
	if expired > 0 {
		c.log.Info("expired observed upstreams from node dependency set", "count", expired)
		c.signalDependencyChange()
	}
}

// pruneObservedLocked applies the idle TTL to observedDeps, refreshing entries
// backed by a live on-demand subscription instead of expiring them. It returns
// how many entries were expired and how many refreshed. Caller must hold depMu
// for writing.
func (c *SnapshotCache) pruneObservedLocked(now time.Time, ttl time.Duration) (expired, refreshed int) {
	inUse := c.onDemandServicesLocked()
	for svc, last := range c.observedDeps {
		if now.Sub(last) <= ttl {
			continue
		}
		if _, live := inUse[svc]; live {
			c.observedDeps[svc] = now
			refreshed++
			continue
		}
		delete(c.observedDeps, svc)
		expired++
	}
	return expired, refreshed
}

// observedTTLValue returns the configured observed-dependency TTL (test hook).
func (c *SnapshotCache) observedTTLValue() time.Duration {
	if c.observedTTL > 0 {
		return c.observedTTL
	}
	return defaultObservedTTL
}

// DependencySet returns the node dependency set: the union of all local pods'
// declared upstreams, their own services, and live (non-expired) observed
// dependencies. LoadClustersFromRegistry scopes the cluster/endpoint/route
// snapshot to this set (demand-scoped distribution, proposal 004).
func (c *SnapshotCache) DependencySet() map[string]struct{} {
	// Write lock (not RLock): dependencySetLocked may rebuild and store the
	// memoized set. External callers get a copy — the memo is shared state.
	c.depMu.Lock()
	set := c.dependencySetLocked()
	out := make(map[string]struct{}, len(set))
	for svc := range set {
		out[svc] = struct{}{}
	}
	c.depMu.Unlock()
	return out
}

// dependencySetShared returns the memoized dependency set without DependencySet's
// defensive copy. The returned map is shared across callers until the next
// rebuild and must be treated as read-only (the depSet convention, same as
// effRoutes/routeDomains). For internal read-only consumers on the snapshot
// path, where the copy is pure overhead.
func (c *SnapshotCache) dependencySetShared() map[string]struct{} {
	// Write lock (not RLock): dependencySetLocked may rebuild and store the memo.
	c.depMu.Lock()
	defer c.depMu.Unlock()
	return c.dependencySetLocked()
}

// dependencyCounts returns the number of distinct declared upstream services and
// of live observed dependencies, for the snapshot-shape metric. Both are derived
// from the dependency-set inputs and recorded when the memo is built, so a
// snapshot rebuild costs a memo validity check instead of two full walks.
func (c *SnapshotCache) dependencyCounts() (declared, observed int) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	// Refresh through the memo so the counts obey the same validity rules as the
	// set itself — including the wall-clock expiry horizon, which is exactly when
	// the observed count could otherwise go stale.
	c.dependencySetLocked()
	return c.depDeclaredCount, c.depObservedCount
}

// bumpDepGenLocked invalidates the memoized dependency set. EVERY writer of
// ANY depMu-guarded field must call it after the write (rule of thumb: any
// write under depMu bumps, even for fields dependencySetLocked does not read
// today). Over-invalidation costs one recompute; a missed bump serves a stale
// dependency set — clusters silently missing from the scoped snapshot.
// Caller must hold depMu for writing.
func (c *SnapshotCache) bumpDepGenLocked() {
	c.depGen++
}

// dependencySetLocked returns the dependency set, memoized on the input
// generation counter (issue #539): while depGen is unchanged the previously
// built set is still exact and is returned as-is, EXCEPT that observed
// dependencies expire by wall clock at read time without any mutator running
// — so the memo also records the earliest observed-entry expiry
// (depSetExpiry) and rebuilds once that instant passes, keeping expiry
// semantics identical to the from-scratch build (PruneObservedDependencies
// merely deletes already-expired entries later). The returned map is shared
// across calls until the next rebuild and must be treated as read-only
// (DependencySet hands external callers a copy). Caller must hold depMu for
// writing (a rebuild stores the memo).
func (c *SnapshotCache) dependencySetLocked() map[string]struct{} {
	now := time.Now()
	ttl := c.observedTTLValue()
	if c.depSetValid && c.depSetGen == c.depGen && c.depSetTTL == ttl &&
		(c.depSetExpiry.IsZero() || !now.After(c.depSetExpiry)) {
		return c.depSet
	}
	set := make(map[string]struct{}, len(c.podDeps)*4+len(c.observedDeps)+len(c.staticDeps))
	c.populateStaticDepsLocked(set)
	c.populatePodDepsLocked(set)
	nextExpiry, observed := c.populateObservedDepsLocked(set, now, ttl)
	eff := c.effectiveServiceRoutesLocked()
	c.populateRouteDepsLocked(set, eff)
	c.depSet = set
	c.depSetGen = c.depGen
	c.depSetTTL = ttl
	c.depSetExpiry = nextExpiry
	// The snapshot-shape counts are functions of the same inputs, so they ride
	// the same memo: while it is valid no declared upstream changed (depGen) and
	// no observed entry crossed its TTL (depSetExpiry), which is precisely the
	// condition under which recomputing them would return the same numbers.
	c.depDeclaredCount = c.declaredCountLocked()
	c.depObservedCount = observed
	c.depSetValid = true
	return set
}

// populateStaticDepsLocked adds static, TCP-floor, and chain-filter services to set.
// Caller must hold depMu for writing.
func (c *SnapshotCache) populateStaticDepsLocked(set map[string]struct{}) {
	// Static (edge) dependencies: the explicitly exposed services.
	for svc := range c.staticDeps {
		set[svc] = struct{}{}
	}
	// TCP mesh services (capture floor): always in scope — their per-VIP floor
	// chains are emitted unconditionally on every capture listener, and a chain
	// whose tcp: cluster is missing dead-ends every connection (no ODCDS for
	// tcp_proxy). Explicit + cluster-wide + few, like GAMMA route targets.
	for _, svc := range c.captureTCPDeps {
		set[svc] = struct{}{}
	}
	// Services with a service-wide chain filter (025 M4): always in scope — the
	// filter is enabled at the service's capture vhost, and vhost emission is
	// dependency-gated. Without this, a chain-filtered service with NO GAMMA routes
	// and no declaring pod never gets its dedicated vhost, so the filter silently
	// never applies (requests fall to the ODCDS catch-all vhost, which carries no
	// typed_per_filter_config). Explicit + few, like GAMMA route targets.
	for svc := range c.serviceChainFilters {
		set[svc] = struct{}{}
	}
	for svc := range c.importedServiceChainFilters {
		set[svc] = struct{}{}
	}
}

// populatePodDepsLocked adds each pod's own service and declared upstreams to set.
// Caller must hold depMu for writing.
func (c *SnapshotCache) populatePodDepsLocked(set map[string]struct{}) {
	for _, deps := range c.podDeps {
		if deps.service != "" {
			set[deps.service] = struct{}{}
		}
		for _, u := range deps.upstreams {
			set[u] = struct{}{}
		}
	}
}

// populateObservedDepsLocked adds live observed dependencies to set and returns
// the earliest expiry instant for the memo plus how many entries were live.
// Caller must hold depMu for writing.
func (c *SnapshotCache) populateObservedDepsLocked(set map[string]struct{}, now time.Time, ttl time.Duration) (time.Time, int) {
	// Live observed dependencies, tracking the earliest wall-clock instant a
	// memoized entry expires (an entry is live while now <= last+ttl, so the
	// memo stays valid through that exact instant and rebuilds after).
	//
	// An entry the proxy holds a live on-demand subscription for never expires
	// here, and contributes no expiry horizon: expiry is read-time and
	// wall-clock driven, so without this the entry would leave the SERVED set
	// (and its cluster the snapshot) in the window between crossing the TTL and
	// the next PruneObservedDependencies tick refreshing it — the very drop this
	// exemption exists to prevent (issue #682).
	inUse := c.onDemandServicesLocked()
	var nextExpiry time.Time
	live := 0
	for svc, last := range c.observedDeps {
		if _, held := inUse[svc]; held {
			set[svc] = struct{}{}
			live++
			continue
		}
		if now.Sub(last) <= ttl {
			set[svc] = struct{}{}
			live++
			if exp := last.Add(ttl); nextExpiry.IsZero() || exp.Before(nextExpiry) {
				nextExpiry = exp
			}
		}
	}
	return nextExpiry, live
}

// populateRouteDepsLocked adds GAMMA route targets and their backends (L7 and L4)
// to set. Caller must hold depMu for writing.
func (c *SnapshotCache) populateRouteDepsLocked(set map[string]struct{}, eff map[string][]proxy.GammaRoute) {
	// Service-based routing (proposal 023): a GAMMA route TARGET — the k8s Service an
	// HTTPRoute/GRPCRoute is attached to (parentRef) — is always in scope, so its
	// cap_http vhost builds even when the target Service has no ServiceAccount-backed
	// pods of its own (the versioned-fanout shape: an "echo" target routed to
	// echo-v1/echo-v2). Its backendRefs are unioned in by routeBackendsLocked below.
	// GAMMA routes are explicit, cluster-wide config (few), so global scope is fine.
	// Imported (peer-cluster) routes are in scope too (proposal 026): a route target
	// whose config arrives cross-cluster still needs its cap_http vhost + backends.
	for svc := range eff {
		set[svc] = struct{}{}
	}
	// GAMMA (proposal 018 Phase 2): a depended-on service's L7 rule backends must
	// also be resolvable, so union them in (their EDS clusters then generate).
	// L4 routes (proposal 018 Phase 3b): same principle for TCP/TLS/UDP backends.
	hasL7 := len(eff) > 0
	hasL4 := len(c.tcpServiceRoutes) > 0 || len(c.tlsServiceRoutes) > 0 || len(c.udpServiceRoutes) > 0
	if !hasL7 && !hasL4 {
		return
	}
	base := make([]string, 0, len(set))
	for svc := range set {
		base = append(base, svc)
	}
	for _, svc := range base {
		if hasL7 {
			routeBackendsFrom(eff, svc, set)
		}
		if hasL4 {
			c.l4RouteBackendsLocked(svc, set)
		}
	}
}

// declaredCountLocked returns the number of distinct declared upstream
// services (excluding own services). Called from the memo rebuild, not per
// snapshot. Caller must hold depMu.
func (c *SnapshotCache) declaredCountLocked() int {
	declared := make(map[string]struct{})
	for _, deps := range c.podDeps {
		for _, u := range deps.upstreams {
			declared[u] = struct{}{}
		}
	}
	return len(declared)
}

// DependencyChanges returns a channel receiving a (coalesced) signal whenever
// the node dependency set changes — a pod was added or removed, or its
// declared upstreams differ. Consumers treat each receive as "the dependency
// set changed, rebuild the scoped snapshot".
func (c *SnapshotCache) DependencyChanges() <-chan struct{} {
	return c.depChanged
}

// signalDependencyChange performs a non-blocking, coalescing send on
// depChanged.
func (c *SnapshotCache) signalDependencyChange() {
	select {
	case c.depChanged <- struct{}{}:
	default:
	}
}

// equalSets reports whether two string sets hold the same members.
func equalSets(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
