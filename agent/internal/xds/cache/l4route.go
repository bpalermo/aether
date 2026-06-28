package cache

import (
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
)

// SetTCPServiceRoutes replaces the TCPRoute-derived per-service L4 rules
// (proposal 018, Phase 3b). On change, it regenerates all per-pod capture
// listeners (which embed per-ClusterIP TCP floor chains) and triggers a full
// snapshot rebuild.
func (c *SnapshotCache) SetTCPServiceRoutes(routes map[string][]proxy.L4ServiceRoute) {
	c.depMu.Lock()
	changed := !equalL4ServiceRoutes(c.tcpServiceRoutes, routes)
	c.tcpServiceRoutes = routes
	c.depMu.Unlock()
	if !changed {
		return
	}
	// Regenerate per-pod capture listeners: they embed TCP floor chains whose
	// cluster specifier changes when TCPRoute rules arrive or change.
	if c.captureEnabled {
		c.regenerateAllCaptureListeners()
	}
	c.signalDependencyChange()
}

// SetTLSServiceRoutes replaces the TLSRoute-derived per-service SNI rules
// (proposal 018, Phase 3b). On change, regenerates capture listeners and signals
// a snapshot rebuild.
func (c *SnapshotCache) SetTLSServiceRoutes(routes map[string][]proxy.L4ServiceRoute) {
	c.depMu.Lock()
	changed := !equalL4ServiceRoutes(c.tlsServiceRoutes, routes)
	c.tlsServiceRoutes = routes
	c.depMu.Unlock()
	if !changed {
		return
	}
	if c.captureEnabled {
		c.regenerateAllCaptureListeners()
	}
	c.signalDependencyChange()
}

// SetUDPServiceRoutes replaces the UDPRoute-derived per-service UDP routes
// (proposal 018, Phase 3b). On change, regenerates all per-pod UDP capture
// listeners (each pod gets one UDP listener covering all services' UDP routes)
// and triggers a full snapshot rebuild so Envoy picks up the new listeners and
// their backend UDP clusters.
func (c *SnapshotCache) SetUDPServiceRoutes(routes map[string][]proxy.L4Backend) {
	c.depMu.Lock()
	changed := !equalUDPRoutes(c.udpServiceRoutes, routes)
	c.udpServiceRoutes = routes
	c.depMu.Unlock()
	if !changed {
		return
	}
	// Regenerate per-pod UDP capture listeners: the udp_proxy target cluster set
	// changes when UDPRoute rules arrive or change.
	if c.captureEnabled {
		c.regenerateAllUDPCaptureListeners()
	}
	c.signalDependencyChange()
}

// tcpServiceRoutesSnapshot returns a shallow copy of the TCPRoute rules for
// use during a snapshot rebuild (read once, outside the dep lock).
func (c *SnapshotCache) tcpServiceRoutesSnapshot() map[string][]proxy.L4ServiceRoute {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	out := make(map[string][]proxy.L4ServiceRoute, len(c.tcpServiceRoutes))
	for k, v := range c.tcpServiceRoutes {
		out[k] = v
	}
	return out
}

// tlsServiceRoutesSnapshot returns a shallow copy of the TLSRoute rules.
func (c *SnapshotCache) tlsServiceRoutesSnapshot() map[string][]proxy.L4ServiceRoute {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	out := make(map[string][]proxy.L4ServiceRoute, len(c.tlsServiceRoutes))
	for k, v := range c.tlsServiceRoutes {
		out[k] = v
	}
	return out
}

// udpServiceRoutesSnapshot returns a shallow copy of the UDPRoute backends.
func (c *SnapshotCache) udpServiceRoutesSnapshot() map[string][]proxy.L4Backend {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	out := make(map[string][]proxy.L4Backend, len(c.udpServiceRoutes))
	for k, v := range c.udpServiceRoutes {
		out[k] = v
	}
	return out
}

// l4RouteBackendsLocked appends the namespace-qualified "<ns>/<svc>" backend
// service keys of a service's L4 routes (TCP + TLS + UDP) to set (020 Part 1).
// Caller holds depMu.
func (c *SnapshotCache) l4RouteBackendsLocked(service string, set map[string]struct{}) {
	for _, r := range c.tcpServiceRoutes[service] {
		for _, b := range r.Backends {
			if b.Service != "" {
				set[b.Service] = struct{}{}
			}
		}
	}
	for _, r := range c.tlsServiceRoutes[service] {
		for _, b := range r.Backends {
			if b.Service != "" {
				set[b.Service] = struct{}{}
			}
		}
	}
	for _, b := range c.udpServiceRoutes[service] {
		if b.Service != "" {
			set[b.Service] = struct{}{}
		}
	}
}

// regenerateAllUDPCaptureListeners rebuilds every per-pod UDP capture listener
// in place. Called when UDPRoute rules change: the udp_proxy target cluster
// is derived from the route map, so a change requires regenerating all listeners.
func (c *SnapshotCache) regenerateAllUDPCaptureListeners() {
	c.listenerMu.Lock()
	for netns, entry := range c.listeners {
		if entry.cniPod == nil {
			continue
		}
		newUDP, err := c.generateUDPCaptureListener(entry.cniPod)
		if err != nil {
			c.log.Error("failed to regenerate UDP capture listener on UDPRoute change",
				"netns", netns, "pod", entry.cniPod.GetName(), "error", err)
			continue
		}
		entry.udpCapture = newUDP
		c.listeners[netns] = entry
	}
	c.listenerMu.Unlock()
}

// regenerateAllCaptureListeners rebuilds every per-pod capture listener in place.
// Called when TCPRoute or TLSRoute rules change: the per-ClusterIP floor chains
// are embedded in the per-pod capture listener, so they must be regenerated.
func (c *SnapshotCache) regenerateAllCaptureListeners() {
	c.listenerMu.Lock()
	for netns, entry := range c.listeners {
		if entry.cniPod == nil {
			continue
		}
		newCapture, err := c.generateCaptureListener(entry.cniPod)
		if err != nil {
			c.log.Error("failed to regenerate capture listener on L4-route change",
				"netns", netns, "pod", entry.cniPod.GetName(), "error", err)
			continue
		}
		entry.capture = newCapture
		c.listeners[netns] = entry
	}
	c.listenerMu.Unlock()
}

// equalL4ServiceRoutes reports whether two L4 service route maps are equal
// (same keys, same rules in order).
func equalL4ServiceRoutes(a, b map[string][]proxy.L4ServiceRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for svc, ra := range a {
		rb, ok := b[svc]
		if !ok || len(ra) != len(rb) {
			return false
		}
		for i := range ra {
			if !equalL4ServiceRoute(ra[i], rb[i]) {
				return false
			}
		}
	}
	return true
}

func equalL4ServiceRoute(a, b proxy.L4ServiceRoute) bool {
	if len(a.SNIHostnames) != len(b.SNIHostnames) || len(a.Backends) != len(b.Backends) {
		return false
	}
	for i := range a.SNIHostnames {
		if a.SNIHostnames[i] != b.SNIHostnames[i] {
			return false
		}
	}
	for i := range a.Backends {
		if a.Backends[i] != b.Backends[i] {
			return false
		}
	}
	return true
}

// equalUDPRoutes reports whether two UDP route maps are identical.
func equalUDPRoutes(a, b map[string][]proxy.L4Backend) bool {
	if len(a) != len(b) {
		return false
	}
	for svc, ra := range a {
		rb, ok := b[svc]
		if !ok || len(ra) != len(rb) {
			return false
		}
		for i := range ra {
			if ra[i] != rb[i] {
				return false
			}
		}
	}
	return true
}
