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
	heartbeatInterval = 2 * time.Second
	predecessorStale  = 8 * time.Second
	readyPollInterval = 1 * time.Second
	stateFileName     = "epoch"
)

// initStartEpoch decides the restart epoch for the first Envoy this supervisor
// launches. With cross-pod coordination (StateDir set), a live predecessor
// heartbeating epoch E means start at E+1 to hot-restart from it; otherwise start
// fresh at epoch 0. Without StateDir (Strategy A only) this is a no-op (epoch 0).
func (s *Supervisor) initStartEpoch() {
	if s.cfg.StateDir == "" {
		return
	}
	epoch, hb, ok := s.readState()
	if ok && time.Since(hb) < predecessorStale {
		s.mu.Lock()
		s.nextEpoch = epoch + 1
		s.mu.Unlock()
		s.log.Info("live predecessor found; starting cross-pod hot restart",
			"predecessorEpoch", epoch, "startEpoch", epoch+1, "heartbeatAge", time.Since(hb).Round(time.Millisecond).String())
		return
	}
	s.log.Info("no live predecessor; starting fresh at epoch 0", "statePresent", ok)
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
	if cur, hb, ok := s.readState(); ok && epoch < cur && time.Since(hb) < predecessorStale {
		return
	}
	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		s.log.V(1).Error(err, "creating state dir")
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

// heartbeat refreshes the coordination file so a surging successor pod can detect
// this node's live epoch and hot-restart from it.
func (s *Supervisor) heartbeat(ctx context.Context) {
	if s.cfg.StateDir == "" {
		return
	}
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			s.writeState(s.currentEpoch())
		}
	}
}

// watchReadiness maintains a pod-local readiness marker: present once the node's
// Envoy admin reports LIVE at this supervisor's newest epoch (for a cross-pod
// surge, only after the new Envoy has taken over the listen sockets and admin
// port). An exec readiness probe checks the marker.
func (s *Supervisor) watchReadiness(ctx context.Context) {
	if s.cfg.ReadyMarkerPath == "" {
		return
	}
	defer s.clearReady()
	t := time.NewTicker(readyPollInterval)
	defer t.Stop()
	ready := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			live := s.adminLiveAtEpoch(ctx, s.currentEpoch())
			switch {
			case live && !ready:
				s.setReady()
				ready = true
				s.log.Info("pod ready: envoy live at newest epoch", "epoch", s.currentEpoch())
			case !live && ready:
				s.clearReady()
				ready = false
				s.log.Info("pod not ready: envoy not live at newest epoch", "epoch", s.currentEpoch())
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

// supersededBySuccessor reports whether a successor pod has taken over this node
// at a higher epoch. When our newest Envoy exits during a cross-pod surge, it was
// terminated by the successor's takeover (not a crash), so the supervisor should
// shut down gracefully and let the pod be deleted rather than exit non-zero and
// restart-loop.
func (s *Supervisor) supersededBySuccessor(epoch int) bool {
	if s.cfg.StateDir == "" {
		return false
	}
	cur, hb, ok := s.readState()
	return ok && cur > epoch && time.Since(hb) < predecessorStale
}

// adminLiveAtEpoch reports whether the Envoy admin (the shared host-netns port)
// reports state LIVE at the given restart epoch. During a cross-pod handoff the
// predecessor answers admin at the old epoch until the new Envoy takes over.
func (s *Supervisor) adminLiveAtEpoch(ctx context.Context, epoch int) bool {
	ctx, cancel := context.WithTimeout(ctx, readyPollInterval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.cfg.AdminAddress+"/server_info", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct {
		State              string `json:"state"`
		CommandLineOptions struct {
			RestartEpoch int `json:"restart_epoch"`
		} `json:"command_line_options"`
	}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return false
	}
	return info.State == "LIVE" && info.CommandLineOptions.RestartEpoch == epoch
}
