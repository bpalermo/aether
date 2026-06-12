package cache

import (
	"context"
	"time"

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
		service:   cniPod.GetServiceAccount(),
		upstreams: proxy.UpstreamsFromPod(cniPod),
	}

	c.depMu.Lock()
	before := c.dependencySetLocked()
	c.podDeps[netns] = deps
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
	c.depMu.Unlock()

	if known {
		return false
	}

	c.log.Info("observed undeclared upstream (ODCDS miss); adding to node dependency set",
		"service", service, "ttl", c.observedTTLValue().String())
	c.metrics.upstreamMiss(ctx, service)
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
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	return c.dependencySetLocked()
}

// dependencySetLocked builds the dependency set. Caller must hold depMu.
func (c *SnapshotCache) dependencySetLocked() map[string]struct{} {
	set := make(map[string]struct{}, len(c.podDeps)*4+len(c.observedDeps))
	for _, deps := range c.podDeps {
		if deps.service != "" {
			set[deps.service] = struct{}{}
		}
		for _, u := range deps.upstreams {
			set[u] = struct{}{}
		}
	}
	now := time.Now()
	ttl := c.observedTTLValue()
	for svc, last := range c.observedDeps {
		if now.Sub(last) <= ttl {
			set[svc] = struct{}{}
		}
	}
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
