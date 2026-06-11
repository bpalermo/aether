package hotrestart

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Cross-pod coordination for Strategy B (see docs/proposals/001_proxy-hot-restart.md).
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
// launches. With cross-pod coordination (StateDir set), it starts at E+1 only when
// the heartbeat file names epoch E AND the node's Envoy admin actually reports E
// LIVE — admin is ground truth, so a crashed predecessor (stale heartbeat, dead on
// admin) correctly resets to epoch 0 instead of attaching to a dead parent.
//
// The two signals must AGREE before a decision is made: the heartbeat is
// LIVE-gated by construction, so a fresh heartbeat means the predecessor was
// serving within the stale window — a single failed admin probe (transient
// timeout under node load) must not send us to epoch 0, which bind-collides with
// the live predecessor on the base-id domain socket (errno 98) and crash-loops.
// While the file is fresh but admin unconfirmed, re-probe; the wait is bounded by
// the heartbeat going stale. Without StateDir (Strategy A) this is a no-op.
func (s *Supervisor) initStartEpoch(ctx context.Context) {
	if s.cfg.StateDir == "" {
		return
	}
	for {
		epoch, hb, ok := s.readState()
		if !ok || time.Since(hb) >= predecessorStale {
			s.log.Info("no live predecessor; starting fresh at epoch 0", "statePresent", ok)
			s.metrics.predecessorFound(false)
			return
		}
		if s.adminLiveAtEpoch(ctx, epoch) {
			s.mu.Lock()
			s.nextEpoch = epoch + 1
			s.mu.Unlock()
			s.metrics.predecessorFound(true)
			s.log.Info("live predecessor confirmed; starting cross-pod hot restart",
				"predecessorEpoch", epoch, "startEpoch", epoch+1, "heartbeatAge", time.Since(hb).Round(time.Millisecond).String())
			return
		}
		s.log.V(1).Info("fresh heartbeat but admin not confirming predecessor; re-probing",
			"epoch", epoch, "heartbeatAge", time.Since(hb).Round(time.Millisecond).String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(initProbeInterval):
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
	if s.cfg.StateDir == "" {
		return
	}
	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		s.log.V(1).Error(err, "creating state dir")
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
	tmp := s.statePath() + ".tmp"
	data := fmt.Sprintf("%d %d\n", epoch, time.Now().UnixMilli())
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		s.log.V(1).Error(err, "writing state")
		return
	}
	if err := os.Rename(tmp, s.statePath()); err != nil {
		s.log.V(1).Error(err, "renaming state")
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
//   - handoff watchdog: a hot-restart epoch (N>0) still not LIVE
//     HandoffDeadline after launch, child still alive → wedged pre-LIVE.
//   - admin watchdog: admin unreachable (connect/timeout, NOT "answers with a
//     different epoch", which is the normal mid-handoff state) for
//     AdminUnresponsiveDeadline once some epoch has been LIVE → wedged
//     post-LIVE (parent died mid stats-merge).
func (s *Supervisor) watchLiveness(ctx context.Context) {
	if s.cfg.ReadyMarkerPath == "" && s.cfg.StateDir == "" {
		return
	}
	defer s.clearReady()
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
			live, reachable := s.adminProbe(ctx, epoch)
			if reachable {
				unreachableSince = time.Time{}
			} else if unreachableSince.IsZero() {
				unreachableSince = time.Now()
			}
			if live {
				everLive = true
				holding = false
				if launched, wasLive := s.epochProgress(); !wasLive {
					// First LIVE confirmation for this epoch: the handoff (or
					// initial start, epoch 0) completed.
					s.metrics.handoffCompleted(time.Since(launched).Seconds())
				}
				s.markEpochLive()
				s.writeState(epoch) // LIVE-gated heartbeat
				// Hold readiness until the cross-pod handoff is fully complete (the
				// predecessor has been terminated by this Envoy's parent-shutdown), so
				// the DaemonSet doesn't delete the old pod while we still need it.
				if !ready && !time.Now().Before(s.readyGateTime()) {
					s.setReady()
					ready = true
					s.metrics.readyTransition(true)
					s.log.Info("pod ready: envoy live at newest epoch", "epoch", epoch)
				}
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
			hold := ready && reachable && s.childTracked(epoch)
			if hold && !holding {
				holding = true
				s.log.Info("holding readiness: serving as hot-restart parent mid-handoff", "epoch", epoch)
			}
			if ready && !hold {
				s.clearReady()
				ready = false
				holding = false
				s.metrics.readyTransition(false)
				s.log.Info("pod not ready: envoy not live at newest epoch", "epoch", epoch)
			}

			launched, wasLive := s.epochProgress()
			if epoch > 0 && !wasLive && s.childTracked(epoch) && time.Since(launched) > s.handoffDeadline() {
				s.metrics.wedged(wedgeHandoffTimeout)
				s.fireWatchdog(fmt.Errorf(
					"hot-restart handoff watchdog: epoch %d not LIVE within %s of launch (parent likely died mid-handoff)",
					epoch, s.handoffDeadline()))
				return
			}
			if everLive && !reachable && s.childTracked(epoch) && time.Since(unreachableSince) > s.adminUnresponsiveDeadline() {
				s.metrics.wedged(wedgeAdminUnresponsive)
				s.fireWatchdog(fmt.Errorf(
					"admin watchdog: envoy admin %s unresponsive for %s with child alive at epoch %d",
					s.cfg.AdminAddress, s.adminUnresponsiveDeadline(), epoch))
				return
			}
		}
	}
}

func (s *Supervisor) setReady() {
	if err := os.WriteFile(s.cfg.ReadyMarkerPath, []byte("ready\n"), 0o644); err != nil {
		s.log.V(1).Error(err, "writing ready marker")
	}
}

func (s *Supervisor) clearReady() { _ = os.Remove(s.cfg.ReadyMarkerPath) }

// adminLiveAtEpoch reports whether the Envoy admin (the shared host-netns port)
// reports state LIVE at the given restart epoch. During a cross-pod handoff the
// predecessor answers admin at the old epoch until the new Envoy takes over.
func (s *Supervisor) adminLiveAtEpoch(ctx context.Context, epoch int) bool {
	live, _ := s.adminProbe(ctx, epoch)
	return live
}

// adminProbe distinguishes "admin answered but is not LIVE at epoch" (reachable,
// the normal mid-handoff state) from "admin did not answer at all" (connect
// failure or timeout — a wedged main thread leaves the admin socket bound but
// never accepting, so requests time out). The admin watchdog keys off reachable.
func (s *Supervisor) adminProbe(ctx context.Context, epoch int) (live, reachable bool) {
	ctx, cancel := context.WithTimeout(ctx, readyPollInterval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.cfg.AdminAddress+"/server_info", nil)
	if err != nil {
		return false, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct {
		State              string `json:"state"`
		CommandLineOptions struct {
			RestartEpoch int `json:"restart_epoch"`
		} `json:"command_line_options"`
	}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return false, true
	}
	return info.State == "LIVE" && info.CommandLineOptions.RestartEpoch == epoch, true
}
