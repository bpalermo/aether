package cache

import (
	"net/netip"
	"slices"

	"aethermesh.dev/agent/internal/servicerange"
)

// SetServiceRanges implements servicerange.Sink: it records the cluster's IPv4
// Service range (proposal 044), from the ServiceCIDR objects or the
// --service-cidrs override. The value is canonicalized here too (masked,
// sorted, deduplicated, IPv4 only), so a re-delivery of the same range in
// another order is no change. Nil means no range is known.
//
// The capture listener's Service-range branch (proposal 044, Phase 0) is
// built from it, so a change does what SetCaptureTCPServices does: rebuild
// every pod's capture listener (rebuildPodListenersLocked, under listenerMu,
// after captureMu is released) and then signalDependencyChange, so the new
// branch reaches the proxy. The early return on an equal range is what keeps
// a ServiceCIDR resync from rewriting every pod listener on the node.
func (c *SnapshotCache) SetServiceRanges(ranges []netip.Prefix) {
	canonical := servicerange.Canonicalize(ranges)
	c.captureMu.Lock()
	if slices.Equal(c.serviceRanges, canonical) {
		c.captureMu.Unlock()
		return
	}
	// Replaced, never mutated in place: generateCaptureListener hands the
	// slice on after releasing captureMu.
	c.serviceRanges = canonical
	c.serviceRangesGen++
	c.captureMu.Unlock()

	if !c.captureEnabled {
		return
	}
	c.log.Info("service range changed: rebuilding capture listeners", "ranges", canonical)
	shared := c.extensionHTTPFilters()
	c.listenerMu.Lock()
	c.rebuildPodListenersLocked("service range change", shared)
	c.listenerMu.Unlock()
	c.signalDependencyChange()
}

// ServiceRanges returns a copy of the Service range SetServiceRanges last
// recorded, nil when none is known.
func (c *SnapshotCache) ServiceRanges() []netip.Prefix {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	return slices.Clone(c.serviceRanges)
}
