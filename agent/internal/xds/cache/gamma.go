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
	if !equalGammaHeaderMutation(a.HeaderMutation, b.HeaderMutation) {
		return false
	}
	if !equalGammaRedirect(a.Redirect, b.Redirect) {
		return false
	}
	if !equalGammaURLRewrite(a.URLRewrite, b.URLRewrite) {
		return false
	}
	for i := range a.Matches {
		ma, mb := a.Matches[i], b.Matches[i]
		if ma.Prefix != mb.Prefix || ma.Exact != mb.Exact || ma.Regex != mb.Regex || len(ma.Headers) != len(mb.Headers) {
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

// equalGammaRedirect reports content equality for two *GammaRedirect values.
func equalGammaRedirect(a, b *proxy.GammaRedirect) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// equalGammaURLRewrite reports content equality for two *GammaURLRewrite values.
func equalGammaURLRewrite(a, b *proxy.GammaURLRewrite) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// equalGammaHeaderMutation compares two *GammaHeaderMutation values for content
// equality. Used by the gamma route equality check to avoid constant snapshot
// rebuilds when the reconciler allocates new (but equivalent) mutation structs.
func equalGammaHeaderMutation(a, b *proxy.GammaHeaderMutation) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a.SetRequest) != len(b.SetRequest) || len(a.AddRequest) != len(b.AddRequest) ||
		len(a.RemoveRequest) != len(b.RemoveRequest) || len(a.SetResponse) != len(b.SetResponse) ||
		len(a.AddResponse) != len(b.AddResponse) || len(a.RemoveResponse) != len(b.RemoveResponse) {
		return false
	}
	for i := range a.SetRequest {
		if a.SetRequest[i] != b.SetRequest[i] {
			return false
		}
	}
	for i := range a.AddRequest {
		if a.AddRequest[i] != b.AddRequest[i] {
			return false
		}
	}
	for i := range a.RemoveRequest {
		if a.RemoveRequest[i] != b.RemoveRequest[i] {
			return false
		}
	}
	for i := range a.SetResponse {
		if a.SetResponse[i] != b.SetResponse[i] {
			return false
		}
	}
	for i := range a.AddResponse {
		if a.AddResponse[i] != b.AddResponse[i] {
			return false
		}
	}
	for i := range a.RemoveResponse {
		if a.RemoveResponse[i] != b.RemoveResponse[i] {
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
