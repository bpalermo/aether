package hotrestart

import (
	"context"
	"time"
)

// A replacement that was started is finished (issue #1679).
//
// The worker-count path (#1136, concurrencychange.go) is the one handoff in
// which this supervisor takes something from another pod's Envoy BEFORE its own
// Envoy exists: it asks the predecessor to drain its listeners, waits the drain
// time, stops it, and only then forks. Envoy has no way to take a drain back.
// So from the moment the drain is accepted, the node keeps its listeners only
// if this supervisor goes on to start its Envoy.
//
// A termination request used to cut that short. A roll that changes the worker
// count and is reverted while the surge pod starts deletes the surge pod, and
// its SIGTERM arrived during the drain-time wait: with its context cancelled
// the supervisor failed to stop the predecessor, read the failure as "gone",
// started nothing and exited. The kept pod's Envoy had closed its listeners
// and stayed LIVE on the admin, so the kept pod stayed Ready and the node had
// no data plane until that pod was deleted by hand.
//
// The invariant: a supervisor that has asked a predecessor to drain owns the
// node's listeners until its own Envoy has them. A termination requested in
// between is held back (deferTermination), for replacementFinishBudget at
// most, until that Envoy is LIVE; everything then proceeds as for any pod
// terminated while it serves (handleShutdown: it waits for a successor, asks
// for one, and a kept pod takes the node back). A termination requested BEFORE
// the drain is sent is obeyed at once and nothing is drained.

// replacementLiveAllowance is what replacementFinishBudget adds, on top of the
// drain and the predecessor's exit, for the fresh Envoy to reach LIVE.
const replacementLiveAllowance = 30 * time.Second

// replacementPoll is how often a held termination checks whether the
// replacement has finished.
const replacementPoll = 100 * time.Millisecond

// deferTermination returns the context the supervisor runs on. It is cancelled
// when ctx is (the termination request), except while a replacement this
// supervisor started is unfinished: then only once it has finished, or after
// replacementFinishBudget. stop releases the goroutine that does it.
func (s *Supervisor) deferTermination(ctx context.Context) (run context.Context, stop context.CancelFunc) {
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if ctx.Err() != nil {
		// Requested before the supervisor ran at all: recorded before anything
		// else it does, not whenever the goroutine below is first scheduled.
		s.noteTerminationRequested()
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-run.Done():
			return
		}
		// Order matters: beginReplacement sets replacing only when it finds no
		// request recorded, under the lock this records it under. So either
		// beginReplacement sees the request and drains nothing, or the request
		// sees the replacement and waits for it.
		s.noteTerminationRequested()
		s.awaitReplacement(run)
		cancel()
	}()
	return run, cancel
}

// noteTerminationRequested records when this pod's termination was requested,
// once.
func (s *Supervisor) noteTerminationRequested() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminationAt.IsZero() {
		s.terminationAt = time.Now()
	}
}

// awaitReplacement blocks while a replacement is unfinished, for
// replacementFinishBudget at most, or until run is done.
func (s *Supervisor) awaitReplacement(run context.Context) {
	if !s.replacementUnfinished() {
		return
	}
	budget := s.replacementFinishBudget()
	s.log.WarnContext(run, "termination requested while replacing a predecessor that was asked to drain its listeners "+
		"(worker-count change, #1136); finishing the replacement first: stopping now would leave the node with an envoy "+
		"that serves nothing",
		"budget", budget.String())
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	t := time.NewTicker(replacementPoll)
	defer t.Stop()
	start := time.Now()
	for {
		select {
		case <-run.Done():
			return
		case <-deadline.C:
			s.endReplacement()
			s.log.ErrorContext(run, "the replacement of the drained predecessor did not finish in its budget; "+
				"terminating anyway", "budget", budget.String())
			return
		case <-t.C:
			if !s.replacementUnfinished() {
				s.log.InfoContext(run, "replacement finished: this pod's envoy is LIVE; proceeding with the termination",
					"heldFor", time.Since(start).Round(time.Millisecond).String())
				return
			}
		}
	}
}

// replacementFinishBudget bounds how long a termination is held back for an
// unfinished replacement: the rest of the drain, the predecessor's stop and
// exit (awaitPredecessorGone's own bound), and the fresh Envoy's start. It
// never exceeds what the pod has before the drain fallback of its own shutdown
// would no longer fit in its termination grace (successorWaitBudget).
func (s *Supervisor) replacementFinishBudget() time.Duration {
	budget := 2*s.cfg.DrainTime + shutdownGrace + predecessorExitSettle + replacementLiveAllowance
	if limit := s.cfg.TerminationGrace - (s.cfg.DrainTime + shutdownGrace) - terminationFallbackMargin; s.cfg.TerminationGrace > 0 && limit > 0 && limit < budget {
		budget = limit
	}
	return budget
}

// beginReplacement marks the start of a replacement, just before the
// predecessor is asked to drain. It reports false, and marks nothing, when the
// termination of this pod has already been requested: nothing may be drained
// then.
func (s *Supervisor) beginReplacement() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.terminationAt.IsZero() {
		return false
	}
	s.replacing = true
	return true
}

// endReplacement marks the replacement finished (this supervisor's Envoy is
// LIVE), abandoned before the predecessor accepted the drain, or out of time.
func (s *Supervisor) endReplacement() {
	s.mu.Lock()
	s.replacing = false
	s.mu.Unlock()
}

func (s *Supervisor) replacementUnfinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replacing
}

// terminationRequestedFor returns how long ago this pod's termination was
// requested (0: it has not been, or the supervisor is not run through Run).
func (s *Supervisor) terminationRequestedFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminationAt.IsZero() {
		return 0
	}
	return time.Since(s.terminationAt)
}
