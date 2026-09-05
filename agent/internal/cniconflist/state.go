package cniconflist

import (
	"context"
	"sync/atomic"
	"time"
)

// ChainStatus is the re-assert loop's published view of aether's presence in the
// node's active CNI conflist.
//
// Because the loop repairs SYNCHRONOUSLY inside a check, an unchained status is
// also an UNREPAIRABLE one: by the time it is published the loop has already
// tried and refused (the conflist carries no primary CNI plugin, or no
// known-good entry was ever observed) or the write failed. Nothing on this node
// will heal it on its own; only a fresh cni-install run will. That is what makes
// this state worth acting on rather than merely reporting — see #667.
type ChainStatus struct {
	// Observed is false until the loop's first check completes. Callers MUST
	// treat !Observed as "unknown" and never as "chained": that is the boot
	// window, the moment a node is least likely to be able to mesh anything.
	Observed bool
	// Chained is the result of the most recent check.
	Chained bool
	// Since is when the current Chained value was FIRST observed, so callers can
	// require the state to have persisted before acting on it. A competing
	// writer's in-place `cp -f` walks the file through a truncated state that
	// parses as garbage, and a single check landing in that window reads as
	// unchained; requiring a dwell keeps that blip from fencing a healthy node.
	// Zero when !Observed.
	Since time.Time
}

// ChainState publishes the conflist chaining state outside this package. The
// Reasserter implements it; consumers (the node taint gate, the agent's
// readiness check, the ghost sweep's eviction interlock) take this interface so
// they can be wired with a nil when the re-assert loop is switched off.
type ChainState interface {
	ChainStatus() ChainStatus
}

// chainStatePublisher holds the published chaining state. A pointer swap gives
// readers on other goroutines a consistent snapshot with no lock; nil means "not
// observed yet".
type chainStatePublisher struct {
	status atomic.Pointer[ChainStatus]
}

// ChainStatus returns the most recently published status, or the zero value
// (Observed false) when no check has completed yet.
func (p *chainStatePublisher) ChainStatus() ChainStatus {
	if s := p.status.Load(); s != nil {
		return *s
	}
	return ChainStatus{}
}

// publish records the chaining state, preserving Since across checks that do not
// change it so callers can require the state to have persisted.
func (p *chainStatePublisher) publish(chained bool) {
	if prev := p.status.Load(); prev != nil && prev.Chained == chained {
		return
	}
	p.status.Store(&ChainStatus{Observed: true, Chained: chained, Since: time.Now()})
}

// record publishes the chaining state to BOTH the gauge and the in-process
// ChainState, so the alerting signal and the node's scheduling behaviour can
// never disagree about whether this node is able to mesh a pod. Every path that
// concludes a check goes through here; there is deliberately no way to move one
// without the other.
func (r *Reasserter) record(ctx context.Context, chained bool) {
	r.metrics.chainedState(ctx, chained)
	r.publish(chained)
}
