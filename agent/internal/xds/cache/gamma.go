package cache

import (
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
)

// SetServiceRoutes replaces the GAMMA east-west L7 rules (HTTPRoute parentRef=
// Service, proposal 018 Phase 2). A depended-on service's rule backends enter the
// dependency set (so their EDS clusters generate) and its outbound vhost is
// enriched. Signals a scoped-snapshot rebuild on change.
func (c *SnapshotCache) SetServiceRoutes(routes map[string][]proxy.GammaRoute) {
	c.depMu.Lock()
	changed := !equalServiceRoutes(c.serviceRoutes, routes)
	c.serviceRoutes = routes
	c.depMu.Unlock()
	if changed {
		c.signalDependencyChange()
	}
}

// serviceRoutesSnapshot returns a shallow copy of the GAMMA rules for use during a
// snapshot rebuild (read once, outside the cluster lock).
func (c *SnapshotCache) serviceRoutesSnapshot() map[string][]proxy.GammaRoute {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	out := make(map[string][]proxy.GammaRoute, len(c.serviceRoutes))
	for k, v := range c.serviceRoutes {
		out[k] = v
	}
	return out
}

// routeBackendsLocked appends the bare backend services of a service's GAMMA rules
// to set. Caller holds depMu (it reads c.serviceRoutes).
func (c *SnapshotCache) routeBackendsLocked(service string, set map[string]struct{}) {
	for _, r := range c.serviceRoutes[service] {
		for _, b := range r.Backends {
			if b.Service != "" {
				set[b.Service] = struct{}{}
			}
		}
	}
}

func equalServiceRoutes(a, b map[string][]proxy.GammaRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for svc, ra := range a {
		rb, ok := b[svc]
		if !ok || len(ra) != len(rb) {
			return false
		}
		for i := range ra {
			if !equalGammaRoute(ra[i], rb[i]) {
				return false
			}
		}
	}
	return true
}

func equalGammaRoute(a, b proxy.GammaRoute) bool {
	if len(a.Matches) != len(b.Matches) || len(a.Backends) != len(b.Backends) {
		return false
	}
	if timeoutNanos(a) != timeoutNanos(b) {
		return false
	}
	for i := range a.Matches {
		ma, mb := a.Matches[i], b.Matches[i]
		if ma.Prefix != mb.Prefix || ma.Exact != mb.Exact || len(ma.Headers) != len(mb.Headers) {
			return false
		}
		for j := range ma.Headers {
			if ma.Headers[j] != mb.Headers[j] {
				return false
			}
		}
	}
	for i := range a.Backends {
		if a.Backends[i] != b.Backends[i] {
			return false
		}
	}
	return true
}

func timeoutNanos(r proxy.GammaRoute) int64 {
	if r.Timeout == nil {
		return 0
	}
	return r.Timeout.AsDuration().Nanoseconds()
}
