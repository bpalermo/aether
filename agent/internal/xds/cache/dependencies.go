package cache

import (
	"context"
	"time"

	"github.com/bpalermo/aether/common/serviceref"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
)

// defaultObservedTTL is how long an observed (ODCDS-requested) dependency
// stays in the node dependency set without being re-requested. One-off calls
// therefore don't pin clusters forever; a continuously used undeclared
// upstream re-warms with one xDS round-trip after expiry — and shows up in
// the miss metric, the signal to promote it to a declared annotation.
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
func (c *SnapshotCache) ObserveDependency(ctx context.Context, service string) bool {
	if service == "" {
		return false
	}

	c.depMu.Lock()
	_, known := c.dependencySetLocked()[service]
	c.observedDeps[service] = time.Now()
	// Bump even on a pure timestamp refresh: the memoized expiry horizon
	// (depSetExpiry) may extend, and a stale horizon would expire this
	// entry from the served set too early.
	c.bumpDepGenLocked()
	c.depMu.Unlock()

	if known {
		return false
	}

	c.log.InfoContext(ctx, "observed undeclared upstream (ODCDS miss); adding to node dependency set",
		"service", service, "ttl", c.observedTTLValue().String())
	c.metrics.UpstreamMiss(ctx, service)
	c.signalDependencyChange()
	return true
}

// PruneObservedDependencies drops observed dependencies idle past the TTL and
// signals a dependency change when any were dropped. The refresher calls it
// periodically; the subsequent scoped reload removes the expired clusters
// (after the retention grace), and Envoy re-fetches via ODCDS on next use.
func (c *SnapshotCache) PruneObservedDependencies() {
	now := time.Now()
	ttl := c.observedTTLValue()

	c.depMu.Lock()
	expired := 0
	for svc, last := range c.observedDeps {
		if now.Sub(last) > ttl {
			delete(c.observedDeps, svc)
			expired++
		}
	}
	if expired > 0 {
		c.bumpDepGenLocked()
	}
	c.depMu.Unlock()

	if expired > 0 {
		c.log.Info("expired observed upstreams from node dependency set", "count", expired)
		c.signalDependencyChange()
	}
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
	for _, deps := range c.podDeps {
		if deps.service != "" {
			set[deps.service] = struct{}{}
		}
		for _, u := range deps.upstreams {
			set[u] = struct{}{}
		}
	}
	// Live observed dependencies, tracking the earliest wall-clock instant a
	// memoized entry expires (an entry is live while now <= last+ttl, so the
	// memo stays valid through that exact instant and rebuilds after).
	var nextExpiry time.Time
	for svc, last := range c.observedDeps {
		if now.Sub(last) <= ttl {
			set[svc] = struct{}{}
			if exp := last.Add(ttl); nextExpiry.IsZero() || exp.Before(nextExpiry) {
				nextExpiry = exp
			}
		}
	}
	// Service-based routing (proposal 023): a GAMMA route TARGET — the k8s Service an
	// HTTPRoute/GRPCRoute is attached to (parentRef) — is always in scope, so its
	// cap_http vhost builds even when the target Service has no ServiceAccount-backed
	// pods of its own (the versioned-fanout shape: an "echo" target routed to
	// echo-v1/echo-v2). Its backendRefs are unioned in by routeBackendsLocked below.
	// GAMMA routes are explicit, cluster-wide config (few), so global scope is fine.
	// Imported (peer-cluster) routes are in scope too (proposal 026): a route target
	// whose config arrives cross-cluster still needs its cap_http vhost + backends.
	eff := c.effectiveServiceRoutesLocked()
	for svc := range eff {
		set[svc] = struct{}{}
	}
	// GAMMA (proposal 018 Phase 2): a depended-on service's L7 rule backends must
	// also be resolvable, so union them in (their EDS clusters then generate).
	// L4 routes (proposal 018 Phase 3b): same principle for TCP/TLS/UDP backends.
	hasL7 := len(eff) > 0
	hasL4 := len(c.tcpServiceRoutes) > 0 || len(c.tlsServiceRoutes) > 0 || len(c.udpServiceRoutes) > 0
	if hasL7 || hasL4 {
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
	c.depSet = set
	c.depSetGen = c.depGen
	c.depSetTTL = ttl
	c.depSetExpiry = nextExpiry
	c.depSetValid = true
	return set
}

// observedCountLocked returns the number of live observed dependencies.
// Caller must hold depMu.
func (c *SnapshotCache) observedCountLocked() int {
	now := time.Now()
	ttl := c.observedTTLValue()
	n := 0
	for _, last := range c.observedDeps {
		if now.Sub(last) <= ttl {
			n++
		}
	}
	return n
}

// declaredCountLocked returns the number of distinct declared upstream
// services (excluding own services). Caller must hold depMu.
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
