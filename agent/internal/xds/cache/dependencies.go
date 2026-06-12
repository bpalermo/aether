package cache

import (
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
)

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

// DependencySet returns the node dependency set: the union of all local pods'
// declared upstreams and their own services. LoadClustersFromRegistry scopes
// the cluster/endpoint/route snapshot to this set (demand-scoped
// distribution, proposal 004).
func (c *SnapshotCache) DependencySet() map[string]struct{} {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	return c.dependencySetLocked()
}

// dependencySetLocked builds the dependency set. Caller must hold depMu.
func (c *SnapshotCache) dependencySetLocked() map[string]struct{} {
	set := make(map[string]struct{}, len(c.podDeps)*4)
	for _, deps := range c.podDeps {
		if deps.service != "" {
			set[deps.service] = struct{}{}
		}
		for _, u := range deps.upstreams {
			set[u] = struct{}{}
		}
	}
	return set
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
