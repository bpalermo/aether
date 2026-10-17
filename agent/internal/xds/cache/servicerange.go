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
// Phase 0 only stores it: nothing reads the range yet, so a change rebuilds
// nothing and the snapshot is unchanged. When the capture listener gains its
// Service-range branch (P0-3), a change here must do what
// SetCaptureTCPServices does: rebuild every pod's capture listener
// (rebuildPodListenersLocked, under listenerMu, after captureMu is released)
// and then signalDependencyChange, so the new branch reaches Envoy. Keep the
// early return on an equal range: it is what keeps a ServiceCIDR resync from
// rewriting every pod listener on the node.
func (c *SnapshotCache) SetServiceRanges(ranges []netip.Prefix) {
	canonical := servicerange.Canonicalize(ranges)
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	if slices.Equal(c.serviceRanges, canonical) {
		return
	}
	c.serviceRanges = canonical
	c.serviceRangesGen++
}

// ServiceRanges returns a copy of the Service range SetServiceRanges last
// recorded, nil when none is known.
func (c *SnapshotCache) ServiceRanges() []netip.Prefix {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	return slices.Clone(c.serviceRanges)
}
