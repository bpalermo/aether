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
//
// The first call, whatever its range, closes ServiceRangeDelivered: after the
// rebuild below, so a waiter that goes on to serve the snapshot finds the
// listeners built with it.
func (c *SnapshotCache) SetServiceRanges(ranges []netip.Prefix) {
	defer c.markServiceRangeDelivered()
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

// markServiceRangeDelivered closes ServiceRangeDelivered, and
// CaptureInputsReady when the projection has arrived too.
func (c *SnapshotCache) markServiceRangeDelivered() {
	c.serviceRangeDeliveredOnce.Do(func() { close(c.serviceRangeDelivered) })
	c.maybeCloseCaptureInputsReady()
}

// ServiceRangeDelivered returns a channel closed once SetServiceRanges has
// been called, a nil range included. servicerange.Setup calls it once at
// start on the override and API-not-served paths; on the ServiceCIDR path the
// reconciler calls it on its first reconcile.
func (c *SnapshotCache) ServiceRangeDelivered() <-chan struct{} { return c.serviceRangeDelivered }

// CaptureInputsReady returns a channel closed once every input the capture
// listener is built from has been delivered at least once: the mesh-Service
// projection (CaptureProjected) and the Service range
// (ServiceRangeDelivered). The xDS server holds its first serve on it
// (bounded; AgentXdsServer.SetCaptureGate).
//
// The range is part of it because the listener's cap_passthrough_svc chain
// (proposal 044) exists only while a range is known. A restarted agent that
// served before its ServiceCIDR reconciler had run would publish a listener
// without that chain over the proxy's listener that has it: the chain is
// drained, and every connection on it closed when the drain ends.
func (c *SnapshotCache) CaptureInputsReady() <-chan struct{} { return c.captureInputsReady }

// maybeCloseCaptureInputsReady closes captureInputsReady once both inputs
// have been delivered. Called after each of them is.
func (c *SnapshotCache) maybeCloseCaptureInputsReady() {
	for _, ch := range []chan struct{}{c.captureProjected, c.serviceRangeDelivered} {
		select {
		case <-ch:
		default:
			return
		}
	}
	c.captureInputsReadyOnce.Do(func() { close(c.captureInputsReady) })
}

// serviceRangesGeneration returns the count of the range's content changes.
func (c *SnapshotCache) serviceRangesGeneration() uint64 {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	return c.serviceRangesGen
}

// ServiceRanges returns a copy of the Service range SetServiceRanges last
// recorded, nil when none is known.
func (c *SnapshotCache) ServiceRanges() []netip.Prefix {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	return slices.Clone(c.serviceRanges)
}
