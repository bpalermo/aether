package hotrestart

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aethermesh.dev/common/file"
)

// Cross-pod hot-restart coordination (see docs/proposals/001_proxy-hot-restart.md).
//
// When two aether-proxy pods overlap on a node during a surge upgrade, they share
// the host network namespace, /dev/shm and the same --base-id, so an Envoy in the
// new pod can hot-restart from the Envoy in the old pod exactly as in the in-pod
// case. Two things differ:
//
//  1. Epoch selection: the new pod must launch Envoy at (old epoch + 1) to attach
//     to the predecessor. The supervisors coordinate the per-node epoch through a
//     small heartbeat file on a shared hostPath (StateDir).
//  2. Readiness gating: the new pod must report NotReady until its Envoy has taken
//     over, so the DaemonSet (maxUnavailable=0) keeps the old pod alive until the
//     handoff completes. Readiness is decided per-pod from the supervisor's own
//     in-memory epoch versus what the node's Envoy admin reports, recorded in a
//     pod-local marker file consumed by an exec readiness probe.

const (
	predecessorStale  = 8 * time.Second
	readyPollInterval = 1 * time.Second
	stateFileName     = "epoch"
)

// initProbeInterval is the delay between admin re-probes in initStartEpoch while
// the heartbeat file is fresh but the admin has not yet confirmed the predecessor.
const initProbeInterval = 500 * time.Millisecond

// initStartEpoch decides the restart epoch for the first Envoy this supervisor
// launches. It starts at E+1 only when the heartbeat file names epoch E AND the
// node's Envoy admin actually reports E LIVE — admin is ground truth, so a crashed
// predecessor (stale heartbeat, dead on admin) correctly resets to epoch 0 instead
// of attaching to a dead parent.
//
// The two signals must AGREE before a decision is made: the heartbeat is
// LIVE-gated by construction, so a fresh heartbeat means the predecessor was
// serving within the stale window — a single failed admin probe (transient
// timeout under node load) must not send us to epoch 0 beside the live
// predecessor. While the file is fresh but admin unconfirmed, re-probe; the wait
// is bounded by the heartbeat going stale.
//
// A stale heartbeat is not, on its own, "no predecessor" either (issue #1680):
// it stops when the supervisor that writes it stalls, while its Envoy goes on
// serving. Epoch 0 is chosen only when, in addition, no process holds a
// hot-restart socket of the base id (see envoyWithoutHeartbeat).
//
// The error is envoyWithoutHeartbeat's: an Envoy has held the base id for
// defaultUnheardEnvoyWait with no supervisor reporting it LIVE.
func (s *Supervisor) initStartEpoch(ctx context.Context) error {
	return s.initStartEpochFrom(ctx, anyPredecessor)
}

// anyPredecessor is initStartEpochFrom's "whichever epoch the node's heartbeat
// names".
const anyPredecessor = -1

// initStartEpochFrom is initStartEpoch restricted to one predecessor epoch
// (only >= 0). A superseded supervisor that answers a terminating successor's
// request (superseded.go) may hot-restart from the epoch that asked and from no
// other. When the heartbeat names another epoch it returns at once, having
// probed, drained and gated nothing: the worker-count check below drains and
// stops the Envoy it compares itself with, and that must never reach an Envoy
// nobody asked to have replaced. The caller reads the outcome from nextEpoch.
// Restricted, it never waits on an Envoy without a heartbeat: its caller makes
// that check itself (relaunchSuperseded).
func (s *Supervisor) initStartEpochFrom(ctx context.Context, only int) error {
	var unheard unheardEnvoy
	for {
		decided, err := s.chooseStartEpoch(ctx, only, &unheard)
		if decided || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(initProbeInterval):
		}
	}
}

// chooseStartEpoch is one pass of initStartEpochFrom over the heartbeat and
// the admin. decided false means "ask again".
func (s *Supervisor) chooseStartEpoch(ctx context.Context, only int, unheard *unheardEnvoy) (decided bool, err error) {
	epoch, hb, ok := s.readState()
	if !ok || time.Since(hb) >= predecessorStale {
		return s.chooseWithoutHeartbeat(ctx, only, ok, unheard)
	}
	if only != anyPredecessor && epoch != only {
		return true, nil
	}
	if s.adminPeerLiveAtEpoch(ctx, epoch) {
		return s.startFromPredecessor(ctx, epoch, hb), nil
	}
	s.log.DebugContext(ctx, "fresh heartbeat but admin not confirming predecessor; re-probing",
		"epoch", epoch, "heartbeatAge", time.Since(hb).Round(time.Millisecond).String())
	return false, nil
}

// chooseWithoutHeartbeat decides for a stale or absent heartbeat: epoch 0,
// unless an Envoy holds the base id (unheard.go), which the restricted form
// leaves to its caller.
func (s *Supervisor) chooseWithoutHeartbeat(ctx context.Context, only int, statePresent bool, unheard *unheardEnvoy) (decided bool, err error) {
	if only == anyPredecessor {
		if held, err := s.envoyWithoutHeartbeat(ctx, unheard); err != nil || held {
			return false, err
		}
	}
	s.startFresh(ctx, statePresent)
	return true, nil
}

// startFresh records the decision "no predecessor: epoch 0". nextEpoch is
// already 0 (a new supervisor, or resetEpochForRetry).
func (s *Supervisor) startFresh(ctx context.Context, statePresent bool) {
	// A retry that falls back to a fresh start has no successor epoch
	// to re-anchor; drop any earlier attempt's so a later in-pod epoch
	// that happens to reuse the number is never gated.
	s.mu.Lock()
	s.gatedEpoch = -1
	s.handoffPeer = -1
	s.mu.Unlock()
	s.log.InfoContext(ctx, "no live predecessor; starting fresh at epoch 0", "statePresent", statePresent)
	s.metrics.predecessorFound(false)
}

// startFromPredecessor records the decision for a predecessor confirmed LIVE at
// epoch: a cross-pod hot restart at epoch+1, or, when its worker count differs,
// a fresh lineage after it has been drained and stopped (issue #1136).
//
// It reports false when neither could be decided and the epoch has to be chosen
// again: the predecessor was drained and an Envoy still holds the base id.
func (s *Supervisor) startFromPredecessor(ctx context.Context, epoch int, hb time.Time) (decided bool) {
	if s.freshStartInsteadOfHotRestart(ctx, epoch) {
		if socket, held := s.baseIDHeldAfter(ctx, shutdownGrace); held {
			// The drained predecessor did not exit (its stop failed, or it was
			// hot-restarted in place meanwhile). A fresh epoch 0 beside it
			// would replace its shared memory (unheard.go): choose again, which
			// attaches to whatever is LIVE (it is never drained twice).
			s.log.WarnContext(ctx, "an envoy still holds the base id after the predecessor was drained; "+
				"not starting a fresh envoy at epoch 0 beside it, choosing the epoch again",
				"predecessorEpoch", epoch, "heldSocket", strings.TrimRight(socket, "\x00"))
			return false
		}
		// The predecessor ran another worker count and has been
		// drained and stopped (issue #1136): start a new lineage
		// at epoch 0, ungated, exactly like a node with no
		// predecessor.
		s.mu.Lock()
		s.nextEpoch = 0
		s.gatedEpoch = -1
		s.handoffPeer = -1
		s.mu.Unlock()
		s.metrics.predecessorFound(false)
		return true
	}
	// Publish the successor epoch and its readiness gate in ONE
	// critical section. A watchLiveness tick that reads the new epoch
	// must never be able to see a stale gate: it would find the
	// PREDECESSOR's Envoy answering LIVE at exactly that epoch and
	// mark the pod Ready before this pod's Envoy was forked (#132's
	// hazard; see successorReadyGate).
	s.mu.Lock()
	s.nextEpoch = epoch + 1
	s.readyGate = s.successorReadyGate()
	s.gatedEpoch = epoch + 1
	s.handoffPeer = epoch
	gate := s.readyGate
	s.mu.Unlock()
	s.metrics.predecessorFound(true)
	s.log.InfoContext(ctx, "live predecessor confirmed; starting cross-pod hot restart",
		"predecessorEpoch", epoch, "startEpoch", epoch+1, "heartbeatAge", time.Since(hb).Round(time.Millisecond).String(),
		"readyGateIn", gate.Sub(s.now()).Round(time.Millisecond).String(), "readyGateAnchor", "fork")
	return true
}

// baseIDHeldAfter reports whether a process still holds a hot-restart socket
// of the base id once within has passed, returning as soon as none does.
func (s *Supervisor) baseIDHeldAfter(ctx context.Context, within time.Duration) (socket string, held bool) {
	deadline := time.Now().Add(within)
	for {
		if socket, held = s.envoyHoldsBaseID(); !held || time.Now().After(deadline) {
			return socket, held
		}
		if !sleepCtx(ctx, predecessorGonePoll) {
			return socket, held
		}
	}
}

func (s *Supervisor) statePath() string { return filepath.Join(s.cfg.StateDir, stateFileName) }

// readState parses "<epoch> <unixMillis>" from the coordination file.
func (s *Supervisor) readState() (epoch int, heartbeat time.Time, ok bool) {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return 0, time.Time{}, false
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, time.Time{}, false
	}
	e, err1 := strconv.Atoi(parts[0])
	ms, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, time.Time{}, false
	}
	return e, time.UnixMilli(ms), true
}

// writeState atomically records this supervisor's newest epoch with a fresh
// heartbeat. It never downgrades against a fresh higher epoch: during a surge
// overlap the successor pod owns a higher epoch, and the draining predecessor must
// not clobber it back down (which would corrupt successor detection and the next
// pod's epoch selection). A stale higher epoch (crashed predecessor) is
// overwritten so a fresh node can reset to 0.
func (s *Supervisor) writeState(epoch int) {
	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		s.log.Error("creating state dir", "error", err)
		return
	}

	// flock the read-check-write: during a surge overlap the draining pod's
	// heartbeat and the successor's first write can interleave so the file briefly
	// regresses to the lower epoch, misleading a third starter. Best-effort — a
	// lock failure falls back to the unlocked (pre-existing) behavior.
	if lock, err := os.OpenFile(s.statePath()+".lock", os.O_CREATE|os.O_RDWR, 0o644); err == nil {
		if flockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); flockErr == nil {
			defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
		}
		defer func() { _ = lock.Close() }()
	}

	if cur, hb, ok := s.readState(); ok && epoch < cur && time.Since(hb) < predecessorStale {
		return
	}
	// common/file, not a fixed dest+".tmp": StateDir is a shared hostPath and
	// the flock above is explicitly best-effort ("a lock failure falls back to
	// the unlocked behavior"), so on that fallback two overlapping supervisor
	// pods write the SAME temp path and the rename can publish the other
	// process's half-written bytes — into the file that decides the next pod's
	// restart epoch. os.CreateTemp gives each writer its own name.
	data := fmt.Sprintf("%d %d\n", epoch, time.Now().UnixMilli())
	if err := file.AtomicWrite(s.statePath(), []byte(data), 0o644); err != nil {
		s.log.Error("writing state", "error", err)
	}
}

// watchLiveness drives both the readiness marker and the node epoch heartbeat off
// a single ground-truth signal: the node's Envoy admin reporting LIVE at this
// supervisor's newest epoch. Crucially, the epoch is published to the shared state
// file ONLY while LIVE — a launched-but-not-yet-LIVE (or failed) epoch is never
// recorded, so a crash-restart re-reads the still-current predecessor epoch and
// retries N+1 rather than climbing N+1, N+2, … against dead parents.
// It also hosts the two wedge watchdogs (see docs/proposals/001, e2e findings
// 2026-06-10): Envoy's hot-restart RPCs are blocking recvmsg on a datagram
// socket with no timeout, so a parent dying between request and reply leaves
// the successor's main thread blocked forever — child process alive and
// workers serving, but admin never accepting and LIVE never reached. Nothing
// inside Envoy recovers from that; the supervisor must diagnose it and exit
// non-zero so Kubernetes recreates the pod.
//
//   - handoff watchdog: a hot-restart epoch (N>0) still not LIVE, child
//     still alive, and its admin silent at its own epoch for HandoffDeadline
//     (from launch, or from its last answer) → wedged pre-LIVE. A successor
//     that keeps answering is waiting on xDS, not wedged (issue #1085; see
//     handoffProgressAt).
//   - admin watchdog: admin unreachable (connect/timeout, NOT "answers with a
//     different epoch", which is the normal mid-handoff state) for
//     AdminUnresponsiveDeadline once some epoch has been LIVE → wedged
//     post-LIVE (parent died mid stats-merge).
//
// The per-tick signal itself comes from adminProber: /server_info while the
// epoch is unverified, the cheap /ready once it has been confirmed ours (#646).
func (s *Supervisor) watchLiveness(ctx context.Context) {
	defer s.clearReady()
	prober := newAdminProber(s)
	listeners := newListenerWatch(s)
	t := time.NewTicker(readyPollInterval)
	defer t.Stop()
	ready := false
	everLive := false
	holding := false
	var unreachableSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			epoch := s.currentEpoch()
			live, reachable := prober.probe(ctx, epoch)
			if reachable {
				unreachableSince = time.Time{}
			} else if unreachableSince.IsZero() {
				unreachableSince = s.now()
			}
			if live {
				everLive = true
				holding = false
				ready = s.onLiveTick(ctx, listeners, epoch, ready)
				continue
			}
			// Hold readiness while this supervisor's Envoy is still the serving
			// hot-restart parent: admin reachable but answering at another (or
			// not-yet-LIVE) epoch with our newest child alive is the normal
			// mid-handoff state — a surging successor pod attaching to us, or our
			// own in-pod restart still initializing. Dropping Ready here lets the
			// DaemonSet (maxUnavailable=0 counts only Ready pods) delete this pod
			// immediately, and the kubelet's grace-period SIGKILL then cuts the
			// parent from under the successor's hot-restart protocol — observed as
			// errno-111 aborts and node data-plane gaps when a proxy roll
			// coincided with pod churn (e2e 2026-06-11). The wedge watchdogs below
			// still run: a successor stuck pre-LIVE or an unreachable admin ends
			// the hold via container restart, and the child exiting ends it here.
			ready, holding = s.onNotLiveEpoch(ctx, epoch, ready, reachable, holding, unreachableSince)
			s.checkChildSilent(ctx, epoch, reachable, unreachableSince)
			if s.checkWedgeWatchdogs(ctx, epoch, everLive, reachable, unreachableSince) {
				return
			}
		}
	}
}

// onLiveTick handles a tick where admin reports LIVE at the current epoch.
// LIVE is not serving: an Envoy asked to drain keeps answering LIVE with its
// listeners closed (issue #1679), so the listener watch is asked first.
// Returns the updated ready state.
func (s *Supervisor) onLiveTick(ctx context.Context, listeners *listenerWatch, epoch int, ready bool) bool {
	if listeners.stopped(ctx, epoch, ready) {
		return s.onListenersStopped(ctx, epoch, ready)
	}
	return s.onLiveEpoch(ctx, epoch, ready)
}

// onListenersStopped handles a tick where admin reports LIVE at the current
// epoch and the listener watch found that Envoy's listeners stopped
// (listenersstopped.go): it keeps the heartbeat, because a new pod that
// hot-restarts from this Envoy binds the listeners again, and the pod is not
// Ready. Returns the updated ready state.
func (s *Supervisor) onListenersStopped(ctx context.Context, epoch int, ready bool) bool {
	s.markEpochLive()
	s.writeState(epoch)
	if ready {
		s.clearReady()
		s.metrics.readyTransition(false)
		s.log.WarnContext(ctx, "pod not ready: envoy is live at newest epoch but has stopped its listeners", "epoch", epoch)
	}
	return false
}

// onLiveEpoch handles a tick where admin reports LIVE at the current epoch:
// records handoff completion, publishes the heartbeat, and gates readiness.
// Returns the updated ready state.
func (s *Supervisor) onLiveEpoch(ctx context.Context, epoch int, ready bool) bool {
	now := s.now()
	if launched, wasLive := s.epochProgress(); !wasLive {
		// First LIVE confirmation for this epoch: the handoff (or
		// initial start, epoch 0) completed.
		s.metrics.handoffCompleted(now.Sub(launched).Seconds())
		// Envoy's parent-shutdown timer starts here (startWorkers), not at
		// the fork: re-anchor a cross-pod successor's gate on this moment so
		// the pod cannot go Ready while the predecessor is still serving
		// (issue #991).
		if gate, anchor, changed := s.liveAnchoredReadyGate(epoch, now); changed && !ready {
			s.log.InfoContext(ctx, "successor ready gate anchored",
				"epoch", epoch, "anchor", anchor,
				"sinceFork", now.Sub(launched).Round(time.Millisecond).String(),
				"readyGateIn", gate.Sub(now).Round(time.Millisecond).String(),
				"readyGateAfterFork", gate.Sub(launched).Round(time.Millisecond).String(),
				"parentShutdownTime", s.cfg.ParentShutdownTime.String())
		}
	}
	s.markEpochLive()
	s.writeState(epoch) // LIVE-gated heartbeat
	// Hold readiness until the cross-pod handoff is fully complete (the
	// predecessor has been terminated by this Envoy's parent-shutdown), so
	// the DaemonSet doesn't delete the old pod while we still need it.
	if !ready && !now.Before(s.readyGateTime()) {
		s.setReady()
		ready = true
		// The gate passed while LIVE: any cross-pod predecessor has been
		// terminated by our Envoy's parent-shutdown, so it is no longer part
		// of a handoff pair (see handoffSilence).
		s.mu.Lock()
		s.handoffPeer = -1
		s.mu.Unlock()
		s.metrics.readyTransition(true)
		s.log.InfoContext(ctx, "pod ready: envoy live at newest epoch", "epoch", epoch)
	}
	return ready
}

// onNotLiveEpoch handles a tick where admin does not report LIVE at the current
// epoch: manages the readiness-hold logic for the mid-handoff parent state.
// unreachableSince is when the current streak of unreachable admin probes began
// (zero while reachable). Returns the updated ready and holding states.
func (s *Supervisor) onNotLiveEpoch(ctx context.Context, epoch int, ready, reachable, holding bool, unreachableSince time.Time) (bool, bool) {
	// anyChildTracked, not childTracked(epoch): during a bind-collision retry
	// currentEpoch() names a rewound epoch that never had a child while an
	// earlier one is still tracked and still serving the node. See
	// anyChildTracked.
	//
	// An unreachable admin does not end the hold on its own (issue #991).
	// Mid-handoff the shared admin port belongs to the SUCCESSOR, whose main
	// thread is busy applying its first LDS/SDS batch before startWorkers; a
	// single /server_info over readyPollInterval then reads as unreachable, and
	// dropping the marker on it let the DaemonSet delete this pod 5-6s later,
	// before the successor had taken over. The hold is carried through the
	// unreachable streak for exactly the admin watchdog's bound
	// (adminUnresponsiveDeadline): a genuinely wedged admin still ends it, at
	// the same moment checkWedgeWatchdogs restarts the container.
	adminAnswering := reachable ||
		(!unreachableSince.IsZero() && s.now().Sub(unreachableSince) < s.adminUnresponsiveDeadline())
	hold := ready && adminAnswering && s.anyChildTracked()
	if hold && !holding {
		holding = true
		s.log.InfoContext(ctx, "holding readiness: serving as hot-restart parent mid-handoff", "epoch", epoch)
	}
	switch {
	case hold && !reachable && !s.holdingUnreachable:
		s.holdingUnreachable = true
		s.log.InfoContext(ctx, "holding readiness through unreachable admin: successor likely initializing",
			"epoch", epoch, "unreachableFor", s.now().Sub(unreachableSince).Round(time.Millisecond).String(),
			"holdBound", s.adminUnresponsiveDeadline().String())
	case reachable || !hold:
		s.holdingUnreachable = false
	}
	if ready && !hold {
		s.clearReady()
		ready = false
		holding = false
		s.metrics.readyTransition(false)
		s.log.InfoContext(ctx, "pod not ready: envoy not live at newest epoch", "epoch", epoch)
	}
	return ready, holding
}

// checkWedgeWatchdogs fires the watchdog if the handoff or admin-unresponsive
// watchdog thresholds are exceeded. Returns true if the caller should return
// (watchdog fired).
func (s *Supervisor) checkWedgeWatchdogs(ctx context.Context, epoch int, everLive, reachable bool, unreachableSince time.Time) bool {
	launched, wasLive := s.epochProgress()
	if epoch > 0 && !wasLive && s.childTracked(epoch) {
		now := s.now()
		progress := s.handoffProgressAt(epoch, launched)
		if now.Sub(progress) > s.handoffDeadline() {
			s.metrics.wedged(wedgeHandoffTimeout)
			s.fireWatchdog(fmt.Errorf(
				"hot-restart handoff watchdog: epoch %d not LIVE and its admin silent for %s since %s (parent likely died mid-handoff)",
				epoch, s.handoffDeadline(), handoffProgressKind(progress, launched),
			))
			return true
		}
		s.noteHandoffWaitingOnInit(ctx, epoch, now, launched)
	}
	// The streak is charged to this epoch from its launch at the earliest. A
	// superseded supervisor has no Envoy and watches a silent admin for as
	// long as it takes: that silence is what makes it start one again (see
	// superseded.go). Charged to the Envoy it then forks, the old streak fired
	// this watchdog on the first tick, before that Envoy had opened its admin,
	// and the supervisor killed what it had just started.
	if unreachableSince.Before(launched) {
		unreachableSince = launched
	}
	if everLive && !reachable && s.childTracked(epoch) && s.now().Sub(unreachableSince) > s.adminUnresponsiveDeadline() {
		s.metrics.wedged(wedgeAdminUnresponsive)
		s.fireWatchdog(fmt.Errorf(
			"admin watchdog: envoy admin %s unresponsive for %s with child alive at epoch %d",
			s.cfg.AdminAddress, s.adminUnresponsiveDeadline(), epoch,
		))
		return true
	}
	return false
}

// setReady publishes the pod-local readiness marker.
//
// A plain os.WriteFile is correct here and deliberately not an atomic write:
// the marker is a pod-local emptyDir file whose only reader (//agent/cmd/
// proxy-ready, via readymarker.Check) tests for EXISTENCE and never reads a
// byte, so there is no torn read to prevent, no second writer to collide with,
// and nothing to make durable across a crash — the marker is meant to vanish
// with the pod. Going through common/file would add a temp file and an fsync to
// the hottest path in the supervisor for no observable gain.
func (s *Supervisor) setReady() {
	if err := os.WriteFile(s.cfg.ReadyMarkerPath, []byte("ready\n"), 0o644); err != nil {
		s.log.Error("writing ready marker", "error", err)
	}
}

func (s *Supervisor) clearReady() { _ = os.Remove(s.cfg.ReadyMarkerPath) }

// adminLiveAtEpoch reports whether THIS supervisor's Envoy answers the admin
// (the shared host-netns port) LIVE at the given restart epoch. During a
// cross-pod handoff the predecessor answers admin at the old epoch until the
// new Envoy takes over.
//
// Deliberately authoritative and unpooled: every caller (handleDebounce, a
// terminating supervisor asking for a successor) is making an epoch-identity
// decision. handleShutdown calls adminServerInfo directly instead — the same
// probe on the same client — because it must also tell "answered at another
// epoch" (a successor took over) apart from "did not answer at all" (our Envoy
// is dead or wedged), which this wrapper collapses into a single false.
func (s *Supervisor) adminLiveAtEpoch(ctx context.Context, epoch int) bool {
	live, _ := s.adminServerInfo(ctx, epoch)
	return live
}
