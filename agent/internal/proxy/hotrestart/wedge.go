package hotrestart

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Wedge hardening (issue #1058, from the #1050 deadlock).
//
// In #1050 the two Envoys of a hot restart deadlocked on each other's main
// thread over the hot-restart domain sockets: the parent parked in a blocking
// sendmsg forwarding QUIC datagrams to the child, the child parked in a
// blocking recvmsg waiting for the parent's stats. Envoy handles signals on
// the main dispatcher, so neither process could act on SIGTERM. The liveness
// watchdog fired after 30 s, SIGTERMed both epochs, and then sat out the full
// DrainTime + shutdownGrace (15 s) before its SIGKILL — 15 s of node-wide dead
// time for new connections on top of the watchdog's own 30 s, and a fresh
// epoch 0 cannot bind until both processes are gone.
//
// Two things follow, both here:
//
//   - When the watchdog fires and BOTH epochs of the node's hot-restart pair
//     have been silent on the admin for the watchdog's own bound, SIGTERM
//     cannot land, so the supervisor SIGKILLs at once (stopWedged).
//   - The silence itself is exported the moment it is diagnosable: a
//     hot-restart child that stops answering its admin for childSilentAfter
//     after it went LIVE is logged ("hot-restart child silent") and counted
//     (aether_supervisor_child_silent_total), long before the watchdog fires
//     (checkChildSilent).

// childSilentAfter is how long a hot-restart child's admin may stay dark,
// once the child is expected to serve, before it is reported silent. It is
// the 10 s of soak gate (c) in e2e/soak/README.md: a healthy successor's next
// line follows `starting workers` within a few seconds.
const childSilentAfter = 10 * time.Second

// noteAdminAnswer records that the node's admin answered as the given restart
// epoch just now. Every caller holds the answer's epoch identity: a decoded
// /server_info names its restart_epoch, and /ready is only ever sent on a
// connection adminProber has verified for its epoch.
func (s *Supervisor) noteAdminAnswer(epoch int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminHeard[epoch] = s.now()
	s.adminHeardLatest = epoch
	// Only the epochs adjacent to a handoff are ever consulted; keep the map
	// from growing across a pod's lifetime of in-pod restarts.
	for e := range s.adminHeard {
		if e < epoch-2 || e > epoch+2 {
			delete(s.adminHeard, e)
		}
	}
}

// epochSilence is one epoch of a handoff pair and how long it has been silent.
type epochSilence struct {
	epoch     int
	silentFor time.Duration
	silent    bool
}

// handoffSilence is the verdict stopWedged acts on.
type handoffSilence struct {
	epochs    []epochSilence // sorted by epoch
	allSilent bool           // at least two epochs, every one silent
}

// String renders the pair for the log line, e.g. "53=41.0s 54=31.0s".
func (h handoffSilence) String() string {
	parts := make([]string, 0, len(h.epochs))
	for _, e := range h.epochs {
		parts = append(parts, fmt.Sprintf("%d=%.1fs", e.epoch, e.silentFor.Seconds()))
	}
	return strings.Join(parts, " ")
}

// epochList renders the pair's epochs, e.g. "53,54".
func (h handoffSilence) epochList() string {
	parts := make([]string, 0, len(h.epochs))
	for _, e := range h.epochs {
		parts = append(parts, strconv.Itoa(e.epoch))
	}
	return strings.Join(parts, ",")
}

// handoffSilence decides whether every epoch of the node's current hot-restart
// pair has been silent for the admin watchdog's bound.
//
// The pair is what this supervisor can know about from the shared admin port:
//
//   - every epoch it still tracks (an in-pod hot restart tracks both);
//   - the cross-pod predecessor it attached to, until this pod has gone Ready
//     (handoffPeer) — the successor side of #1050;
//   - a newer epoch the admin last answered as (a surge successor holds the
//     admin port) — the parent side of #1050.
//
// An epoch is silent when the admin has not answered as it for
// adminUnresponsiveDeadline. Answers are attributed by epoch (see
// noteAdminAnswer), which is what makes the pair meaningful on ONE shared
// port: a handoff watchdog firing on a child that never went LIVE while the
// admin still answers at the parent's epoch finds the parent heard recently,
// so only one epoch is silent and the normal SIGTERM + grace applies. The
// newest tracked epoch that has never answered counts from its fork; an older
// epoch with no recorded answer is unknown, never silent.
//
// A hot-restart parent's admin is shut down by Envoy when its child starts
// ("shutting down admin due to child startup"), so a parent reads as silent
// from then on. That is safe: it only matters once the watchdog has already
// decided to stop every epoch, and a parent whose main thread is in fact
// running would exit on SIGTERM in under a second (0.33-0.74 s measured,
// #795) with its connections closed either way — SIGKILL costs it only its
// final log flush.
func (s *Supervisor) handoffSilence(now time.Time) handoffSilence {
	s.mu.Lock()
	defer s.mu.Unlock()

	newest := s.nextEpoch - 1
	members := make(map[int]struct{}, len(s.children)+2)
	for e := range s.children {
		members[e] = struct{}{}
	}
	if s.handoffPeer >= 0 {
		members[s.handoffPeer] = struct{}{}
	}
	if s.adminHeardLatest > newest {
		members[s.adminHeardLatest] = struct{}{}
	}

	bound := s.adminUnresponsiveDeadline()
	var h handoffSilence
	for e := range members {
		last, heard := s.adminHeard[e]
		if !heard && e == newest {
			last, heard = s.epochLaunched, !s.epochLaunched.IsZero()
		}
		es := epochSilence{epoch: e}
		if heard {
			es.silentFor = now.Sub(last)
			es.silent = es.silentFor >= bound
		}
		h.epochs = append(h.epochs, es)
	}
	slices.SortFunc(h.epochs, func(a, b epochSilence) int { return a.epoch - b.epoch })

	h.allSilent = len(h.epochs) >= 2
	for _, e := range h.epochs {
		h.allSilent = h.allSilent && e.silent
	}
	return h
}

// stopWedged stops every tracked epoch after the watchdog fired. When both
// epochs of the handoff pair are silent it SIGKILLs at once: their main
// threads are blocked (#1050), SIGTERM is handled on exactly that thread, and
// waiting DrainTime + shutdownGrace for it only extends the node's outage.
// Every other case keeps SIGTERM then grace.
func (s *Supervisor) stopWedged(ctx context.Context, silence handoffSilence) {
	if !silence.allSilent {
		s.shutdown()
		return
	}
	s.log.ErrorContext(ctx,
		"both hot-restart epochs silent; SIGKILLing now instead of SIGTERM + drain grace "+
			"(a blocked main thread cannot take SIGTERM)",
		"epochs", silence.epochList(),
		"silentFor", silence.String(),
		"bound", s.adminUnresponsiveDeadline().String(),
		"graceSkipped", (s.cfg.DrainTime + shutdownGrace).String())
	start := time.Now()
	if s.killChildren() {
		s.metrics.drainCompleted(time.Since(start).Seconds())
	}
}

// killChildren SIGKILLs every tracked epoch and reaps them, waiting at most
// shutdownGrace for the exits to be reported. It reads childExited directly
// because the main loop has stopped selecting on it, and reports whether there
// was anything to kill.
func (s *Supervisor) killChildren() bool {
	s.mu.Lock()
	pending := make(map[int]struct{}, len(s.children))
	for e := range s.children {
		pending[e] = struct{}{}
	}
	s.mu.Unlock()
	if len(pending) == 0 {
		return false
	}
	for e := range pending {
		s.log.Info("killing wedged envoy epoch", "epoch", e)
		s.signalEpoch(e, syscall.SIGKILL)
	}

	deadline := time.NewTimer(shutdownGrace)
	defer deadline.Stop()
	for len(pending) > 0 {
		select {
		case exit := <-s.childExited:
			delete(pending, exit.epoch)
			s.reap(exit.epoch)
		case <-deadline.C:
			for e := range pending {
				s.reap(e)
			}
			return true
		}
	}
	return true
}

// checkChildSilent reports a hot-restart child whose admin has gone dark after
// it was expected to serve: once per epoch, as the log line
// `hot-restart child silent` and aether_supervisor_child_silent_total.
//
// Where "expected to serve" starts is the #991 anchor, the child's first
// observed LIVE: Envoy flips LIVE in startWorkers(), the very step whose
// `starting workers` line soak gate (c) measures from, and it is the moment
// liveAnchoredReadyGate re-anchors on. A fork-anchored budget would instead
// count the xDS-gated init, which is unbounded and measured at 2.2-6 s under
// load (#991) with the successor's admin legitimately missing probes while it
// loads its first listener batch; that phase stays the handoff watchdog's.
//
// The check covers the hot-restart window, first LIVE through
// ParentShutdownTime + liveGateBuffer (when the parent is gone): a dark streak
// that STARTS inside it and lasts childSilentAfter counts, however long after
// the window it is reached. Epoch 0 is not a hot-restart child. An admin that
// answers at all — at our epoch or a successor's — is not silent.
func (s *Supervisor) checkChildSilent(ctx context.Context, epoch int, reachable bool, unreachableSince time.Time) {
	if epoch <= 0 || reachable || unreachableSince.IsZero() || s.childSilentEpoch == epoch {
		return
	}
	liveAt := s.epochFirstLive()
	if liveAt.IsZero() || !s.childTracked(epoch) {
		return
	}
	if windowEnd := liveAt.Add(s.cfg.ParentShutdownTime + liveGateBuffer); unreachableSince.After(windowEnd) {
		return
	}
	now := s.now()
	silentFor := now.Sub(unreachableSince)
	if silentFor < childSilentAfter {
		return
	}
	s.childSilentEpoch = epoch
	s.metrics.childSilentDetected()
	s.log.WarnContext(ctx, "hot-restart child silent",
		"epoch", epoch,
		"silentSeconds", silentFor.Round(100*time.Millisecond).Seconds(),
		"sinceLiveSeconds", now.Sub(liveAt).Round(100*time.Millisecond).Seconds(),
		"threshold", childSilentAfter.String(),
		"adminAddress", s.cfg.AdminAddress)
}

// Init-aware handoff watchdog (issue #1085).
//
// The handoff watchdog exists for one failure: a successor whose main thread
// is blocked in a hot-restart RPC against a dead parent (e2e 2026-06-10) —
// admin bound but never accepting, LIVE never reached. It used to count from
// the fork alone, which also catches a successor that is perfectly healthy
// but still in init because its xDS server, the node agent, is down: the
// proxy bootstrap sets initial_fetch_timeout: 0s on CDS and LDS precisely so
// such a successor WAITS for its first listeners instead of going LIVE empty
// and draining the parent. Firing on it would SIGTERM an in-pod parent that
// is serving the node's traffic — trading a bounded wait for an outage.
//
// A main thread that is waiting on xDS keeps answering the admin, and
// adminServerInfo attributes every answer to the epoch that gave it. So the
// deadline runs from the later of the fork and the successor's last answer at
// its own epoch: a wedged successor never answers (or stops answering) and
// still trips it HandoffDeadline later; one that answers is never killed for
// being slow.

// handoffProgressAt is the moment the handoff watchdog's deadline runs from:
// the successor's fork, or its admin's last answer at its own epoch if later.
func (s *Supervisor) handoffProgressAt(epoch int, launched time.Time) time.Time {
	s.mu.Lock()
	heard, ok := s.adminHeard[epoch]
	s.mu.Unlock()
	if ok && heard.After(launched) {
		return heard
	}
	return launched
}

// handoffProgressKind names the anchor handoffProgressAt picked, for the
// watchdog's error.
func handoffProgressKind(progress, launched time.Time) string {
	if progress.After(launched) {
		return "its last admin answer"
	}
	return "launch"
}

// noteHandoffWaitingOnInit logs, once per epoch, a successor that is still
// not LIVE HandoffDeadline after its fork but has been kept alive because its
// admin answers: the node agent's xDS is the likely hold-up, and the parent is
// still serving.
func (s *Supervisor) noteHandoffWaitingOnInit(ctx context.Context, epoch int, now, launched time.Time) {
	if s.handoffWaitEpoch == epoch || now.Sub(launched) <= s.handoffDeadline() {
		return
	}
	s.handoffWaitEpoch = epoch
	s.log.WarnContext(ctx, "hot-restart successor still initializing past the handoff deadline; "+
		"its admin answers, so it is waiting on xDS (node agent) rather than wedged, and the parent keeps serving",
		"epoch", epoch,
		"sinceForkSeconds", now.Sub(launched).Round(100*time.Millisecond).Seconds(),
		"handoffDeadline", s.handoffDeadline().String())
}
