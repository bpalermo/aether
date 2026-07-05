package cache

import (
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
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

// SetImportedServiceRoutes replaces the GAMMA rules IMPORTED from peer clusters
// (proposal 026): config a remote cluster authored for a service, fetched from the
// registrar (ConfigImporter) and materialized read-only. Merged with local routes at
// read time — a LOCAL HTTPRoute for the same service wins (a cluster's own config is
// authoritative for itself). Signals a scoped-snapshot rebuild on change.
func (c *SnapshotCache) SetImportedServiceRoutes(routes map[string][]proxy.GammaRoute) {
	c.depMu.Lock()
	changed := !equalServiceRoutes(c.importedServiceRoutes, routes)
	c.importedServiceRoutes = routes
	c.depMu.Unlock()
	if changed {
		c.signalDependencyChange()
	}
}

// effectiveServiceRoutesLocked returns the merged GAMMA rules: imported (peer-cluster)
// routes overlaid by local routes (local wins on a per-service key collision). Caller
// holds depMu. Returns the local map directly when nothing is imported (no allocation).
func (c *SnapshotCache) effectiveServiceRoutesLocked() map[string][]proxy.GammaRoute {
	if len(c.importedServiceRoutes) == 0 {
		return c.serviceRoutes
	}
	out := make(map[string][]proxy.GammaRoute, len(c.serviceRoutes)+len(c.importedServiceRoutes))
	for k, v := range c.importedServiceRoutes {
		out[k] = v
	}
	for k, v := range c.serviceRoutes { // local overrides imported
		out[k] = v
	}
	return out
}

// serviceRoutesSnapshot returns a shallow copy of the effective (local ∪ imported)
// GAMMA rules for use during a snapshot rebuild (read once, outside the cluster lock).
func (c *SnapshotCache) serviceRoutesSnapshot() map[string][]proxy.GammaRoute {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	eff := c.effectiveServiceRoutesLocked()
	out := make(map[string][]proxy.GammaRoute, len(eff))
	for k, v := range eff {
		out[k] = v
	}
	return out
}

// SetRouteTargetPorts replaces the per-route-target real Service port map (proposal
// 023 M2). Keyed by the same "<ns>/<svc>" route-target key as SetServiceRoutes;
// fed by the gamma reconciler from each route's parentRef port. captureVhosts emits
// a "<svc>.<ns>.svc.cluster.local:<port>" domain per listed port so a client dialing
// the route target's REAL port host-matches its vhost. Signals a scoped-snapshot
// rebuild on change. A separate side-map (not folded into GammaRoute) keeps the
// rule-equality plumbing untouched and matches the captureAuthorities pattern.
func (c *SnapshotCache) SetRouteTargetPorts(ports map[string][]uint32) {
	c.depMu.Lock()
	changed := !equalRouteTargetPorts(c.routeTargetPorts, ports)
	c.routeTargetPorts = ports
	c.depMu.Unlock()
	if changed {
		c.signalDependencyChange()
	}
}

// routeTargetPortsSnapshot returns a shallow copy of the route-target port map for
// use during a snapshot rebuild (read once, outside the cluster lock).
func (c *SnapshotCache) routeTargetPortsSnapshot() map[string][]uint32 {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	out := make(map[string][]uint32, len(c.routeTargetPorts))
	for k, v := range c.routeTargetPorts {
		out[k] = v
	}
	return out
}

// equalRouteTargetPorts reports content equality of two route-target port maps
// (order-sensitive within each value; the reconciler emits a stable order).
func equalRouteTargetPorts(a, b map[string][]uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		for i := range va {
			if va[i] != vb[i] {
				return false
			}
		}
	}
	return true
}

// routeBackendsFrom appends the bare backend services of a service's GAMMA rules
// (from the given effective route map) to set. Used by dependencySetLocked over the
// merged local ∪ imported routes (caller holds depMu).
func routeBackendsFrom(routes map[string][]proxy.GammaRoute, service string, set map[string]struct{}) {
	for _, r := range routes[service] {
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

// SetServiceChainFilters replaces the service-wide ALWAYS-ON extension filters
// (proposal 025 M4 CHAIN scope), keyed by "<ns>/<svc>". Fed by the gamma reconciler
// from CHAIN-scope HTTPFilters with a Service targetRef; at most one per service.
// Enabled at each service's capture vhost (vhost-level typed_per_filter_config).
func (c *SnapshotCache) SetServiceChainFilters(filters map[string]proxy.ExtensionFilter) {
	c.depMu.Lock()
	changed := !equalServiceChainFilters(c.serviceChainFilters, filters)
	c.serviceChainFilters = filters
	c.depMu.Unlock()
	if changed {
		// INFO on change: the in-vivo debugging signal for "the filter never
		// applied" (2026-07-05: one agent silently held an empty map until restart).
		c.log.Info("service chain filters updated", "count", len(filters))
		c.signalDependencyChange()
	}
}

// SetImportedServiceChainFilters replaces the peer-cluster-imported service chain
// filters (proposal 026). Local wins on a per-service collision, like routes.
func (c *SnapshotCache) SetImportedServiceChainFilters(filters map[string]proxy.ExtensionFilter) {
	c.depMu.Lock()
	changed := !equalServiceChainFilters(c.importedServiceChainFilters, filters)
	c.importedServiceChainFilters = filters
	c.depMu.Unlock()
	if changed {
		c.log.Info("imported service chain filters updated", "count", len(filters))
		c.signalDependencyChange()
	}
}

// serviceChainFiltersSnapshot returns the effective (local ∪ imported, local wins)
// per-service chain filters for a snapshot rebuild.
func (c *SnapshotCache) serviceChainFiltersSnapshot() map[string]proxy.ExtensionFilter {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	if len(c.serviceChainFilters) == 0 && len(c.importedServiceChainFilters) == 0 {
		return nil
	}
	out := make(map[string]proxy.ExtensionFilter, len(c.serviceChainFilters)+len(c.importedServiceChainFilters))
	for k, v := range c.importedServiceChainFilters {
		out[k] = v
	}
	for k, v := range c.serviceChainFilters { // local overrides imported
		out[k] = v
	}
	return out
}

func equalServiceChainFilters(a, b map[string]proxy.ExtensionFilter) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av.Name != bv.Name || !equalAny(av.Config, bv.Config) {
			return false
		}
	}
	return true
}

// equalAny compares two Any configs by proto semantics (nil-safe).
func equalAny(a, b *anypb.Any) bool {
	return proto.Equal(a, b)
}
