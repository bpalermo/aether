package cache

import (
	"context"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/serviceref"
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
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		// Route-referenced extension filters live default-disabled in the per-pod
		// HTTP listeners' HCM chains; rule changes can change that union.
		c.regenerateAllHTTPListeners()
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
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		c.regenerateAllHTTPListeners()
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

// serviceRoutesSnapshot returns the effective (local ∪ imported) GAMMA rules
// with unavailable extension filters stripped, for use during a snapshot
// rebuild (read once, outside the cluster lock). Memoized on depGen (issue
// #540 — previously the merge + strip scan re-ran on EVERY snapshot): the
// returned map is shared across calls until the next mutation and must be
// treated as read-only (the depSet convention). Takes the write lock because
// a rebuild stores the memo.
func (c *SnapshotCache) serviceRoutesSnapshot() map[string][]proxy.GammaRoute {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	return c.effectiveStrippedRoutesLocked()
}

// effectiveStrippedRoutesLocked returns the effective GAMMA rules post-strip,
// memoized on the input generation counter (the #539 depGen idiom): while
// depGen is unchanged the previously built map is still exact and is returned
// as-is. Caller must hold depMu for writing (a rebuild stores the memo).
func (c *SnapshotCache) effectiveStrippedRoutesLocked() map[string][]proxy.GammaRoute {
	if c.effRoutesValid && c.effRoutesGen == c.depGen {
		return c.effRoutes
	}
	eff := c.effectiveServiceRoutesLocked()
	out := make(map[string][]proxy.GammaRoute, len(eff))
	for k, v := range eff {
		out[k] = c.stripUnavailableExtensions(v)
	}
	c.effRoutes = out
	c.effRoutesGen = c.depGen
	c.effRoutesValid = true
	return out
}

// stripUnavailableExtensions drops extension filters whose chain entry is not
// present on this node — today only ext_authz when the authz sidecar is disabled
// (proposal 027): a typed_per_filter_config naming an absent chain filter rejects
// the whole route config, so a stray policy must degrade to a no-op, not a NACK.
// Runs once per effective-routes memo rebuild (issue #540), so the Warn below
// fires per route mutation, not per snapshot.
func (c *SnapshotCache) stripUnavailableExtensions(rules []proxy.GammaRoute) []proxy.GammaRoute {
	if c.authzSidecar {
		return rules
	}
	needsStrip := false
	for _, r := range rules {
		for _, ef := range r.ExtensionFilters {
			if ef.Name == proxy.ExtAuthzFilterName {
				needsStrip = true
			}
		}
	}
	if !needsStrip {
		return rules
	}
	out := make([]proxy.GammaRoute, len(rules))
	copy(out, rules)
	for i := range out {
		var kept []proxy.ExtensionFilter
		for _, ef := range out[i].ExtensionFilters {
			if ef.Name == proxy.ExtAuthzFilterName {
				c.log.Warn("dropping extAuthz filter: the authz sidecar is not enabled on this cluster (proxy.authzSidecar.enabled)", "filter", ef.Name)
				continue
			}
			kept = append(kept, ef)
		}
		out[i].ExtensionFilters = kept
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
	// Not read by dependencySetLocked today; bumped under the blanket
	// "every depMu write bumps" rule (over-invalidation is harmless).
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		c.signalDependencyChange()
	}
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
	if !equalGammaMatches(a.Matches, b.Matches) {
		return false
	}
	for i := range a.Backends {
		if a.Backends[i] != b.Backends[i] {
			return false
		}
	}
	return true
}

// equalGammaMatches reports whether two GammaMatch slices are identical.
func equalGammaMatches(a, b []proxy.GammaMatch) bool {
	for i := range a {
		ma, mb := a[i], b[i]
		if ma.Prefix != mb.Prefix || ma.Exact != mb.Exact || ma.Regex != mb.Regex || len(ma.Headers) != len(mb.Headers) {
			return false
		}
		for j := range ma.Headers {
			if ma.Headers[j] != mb.Headers[j] {
				return false
			}
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
	return equalGammaHMutationLengths(a, b) &&
		equalGammaHeaderKVs(a.SetRequest, b.SetRequest) &&
		equalGammaHeaderKVs(a.AddRequest, b.AddRequest) &&
		equalGammaHeaderStrings(a.RemoveRequest, b.RemoveRequest) &&
		equalGammaHeaderKVs(a.SetResponse, b.SetResponse) &&
		equalGammaHeaderKVs(a.AddResponse, b.AddResponse) &&
		equalGammaHeaderStrings(a.RemoveResponse, b.RemoveResponse)
}

// equalGammaHMutationLengths reports whether two GammaHeaderMutation values have
// identical slice lengths for all six header mutation fields.
func equalGammaHMutationLengths(a, b *proxy.GammaHeaderMutation) bool {
	return len(a.SetRequest) == len(b.SetRequest) &&
		len(a.AddRequest) == len(b.AddRequest) &&
		len(a.RemoveRequest) == len(b.RemoveRequest) &&
		len(a.SetResponse) == len(b.SetResponse) &&
		len(a.AddResponse) == len(b.AddResponse) &&
		len(a.RemoveResponse) == len(b.RemoveResponse)
}

// equalGammaHeaderKVs reports whether two GammaHeaderKV slices are identical.
func equalGammaHeaderKVs(a, b []proxy.GammaHeaderKV) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalGammaHeaderStrings reports whether two string slices are identical element-by-element.
func equalGammaHeaderStrings(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
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
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		// INFO on change: the in-vivo debugging signal for "the filter never
		// applied" (2026-07-05: one agent silently held an empty map until restart).
		c.log.Info("service chain filters updated", "count", len(filters))
		c.regenerateAllHTTPListeners()
		c.signalDependencyChange()
	}
}

// SetImportedServiceChainFilters replaces the peer-cluster-imported service chain
// filters (proposal 026). Local wins on a per-service collision, like routes.
func (c *SnapshotCache) SetImportedServiceChainFilters(filters map[string]proxy.ExtensionFilter) {
	c.depMu.Lock()
	changed := !equalServiceChainFilters(c.importedServiceChainFilters, filters)
	c.importedServiceChainFilters = filters
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		c.log.Info("imported service chain filters updated", "count", len(filters))
		c.regenerateAllHTTPListeners()
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
	if !c.authzSidecar {
		for k, v := range out {
			if v.Name == proxy.ExtAuthzFilterName {
				c.log.Warn("dropping extAuthz service filter: the authz sidecar is not enabled on this cluster", "service", k)
				delete(out, k)
			}
		}
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

// regenerateAllHTTPListeners rebuilds every per-pod outbound + capture HTTP listener
// in place. Called when the escape-hatch extension-filter UNION may have changed
// (GAMMA rules or service chain filters, local or imported): the default-disabled
// HCM entries are embedded in the listeners, and typed_per_filter_config can only
// re-enable a filter already present in the chain — a stale chain silently disables
// the feature (2026-07-05 talos finding: the outbound HCM never carried the union).
func (c *SnapshotCache) regenerateAllHTTPListeners() {
	c.listenerMu.Lock()
	for netns, entry := range c.listeners {
		if entry.cniPod == nil {
			continue
		}
		extensionFilters := c.podExtensionHTTPFilters(entry.cniPod)
		newCapture, err := c.generateCaptureListener(entry.cniPod)
		if err != nil {
			c.log.Error("failed to regenerate capture listener on extension-union change",
				"netns", netns, "pod", entry.cniPod.GetName(), "error", err)
			continue
		}
		newOutbound, err := proxy.GenerateOutboundHTTPListener(entry.cniPod, c.meshDomain, c.emitStatsPod, extensionFilters)
		if err != nil {
			c.log.Error("failed to regenerate outbound listener on extension-union change",
				"netns", netns, "pod", entry.cniPod.GetName(), "error", err)
			continue
		}
		newInbound, err := proxy.NewInboundListener(entry.cniPod, c.trustDomain, c.emitStatsPod, !c.spireEnabled, proxy.WithoutSourceMetadata(extensionFilters), c.inboundFilterForPod(entry.cniPod))
		if err != nil {
			c.log.Error("failed to regenerate inbound listener on extension-union change",
				"netns", netns, "pod", entry.cniPod.GetName(), "error", err)
			continue
		}
		c.applyWaypointInboundServerNames(newInbound, entry.cniPod)
		entry.capture = newCapture
		entry.outbound = newOutbound
		entry.inbound = newInbound
		c.listeners[netns] = entry
	}
	c.listenerMu.Unlock()
	// Push the rebuilt listeners immediately (the dependency signal ALSO triggers a
	// registry reload, but that path only runs when the refresher is up — and prompt
	// convergence beats waiting a debounce for a pure listener change).
	if err := c.generateListenerSnapshot(context.Background()); err != nil {
		c.log.Error("failed to regenerate snapshot after extension-union change", "error", err)
	}
}

// SetServiceInboundFilters replaces the destination-side (INBOUND scope, 027 M3)
// filters, keyed by "<ns>/<svc>". Enabled on the target service's own pods'
// inbound listeners; requires the authz sidecar (dropped with a warning otherwise).
func (c *SnapshotCache) SetServiceInboundFilters(filters map[string]proxy.ExtensionFilter) {
	c.depMu.Lock()
	changed := !equalServiceChainFilters(c.serviceInboundFilters, filters)
	c.serviceInboundFilters = filters
	// Not read by dependencySetLocked today; bumped under the blanket
	// "every depMu write bumps" rule (over-invalidation is harmless).
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		c.log.Info("service inbound filters updated", "count", len(filters))
		c.regenerateAllHTTPListeners()
		c.signalDependencyChange()
	}
}

// inboundFilterForPod returns the INBOUND-scope filter for the pod's own service,
// nil when none (or when the authz sidecar is disabled — a TPFC naming an absent
// chain filter rejects the listener).
func (c *SnapshotCache) inboundFilterForPod(cniPod *cniv1.CNIPod) *proxy.ExtensionFilter {
	key := serviceref.New(cniPod.GetNamespace(), cniPod.GetServiceAccount()).Key()
	c.depMu.RLock()
	ef, ok := c.serviceInboundFilters[key]
	c.depMu.RUnlock()
	if !ok {
		return nil
	}
	if ef.Name == proxy.ExtAuthzFilterName && !c.authzSidecar {
		c.log.Warn("dropping INBOUND extAuthz filter: the authz sidecar is not enabled on this cluster", "service", key)
		return nil
	}
	return &ef
}
