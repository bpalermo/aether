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

// Worker-count changes are not hot-restarted (issue #1136).
//
// A hot-restart child applies its listener socket options to the sockets it
// inherits, and for a QUIC listener those include the connection-ID steering
// program (SO_ATTACH_REUSEPORT_CBPF, `CID % concurrency`) built for the CHILD's
// worker count. A reuse-port BPF program belongs to the whole socket group, so
// from that moment the kernel steers the still-draining parent's datagrams by
// the child's count:
//
//   - 4 -> 2: a parent connection whose CID % 4 differs from CID % 2 (about
//     half) lands on a parent worker that does not own it and is reset
//     ("Mismatched worker index", then a stateless reset). Measured on talos
//     2026-10-02: 34 mismatched batches and 12 stateless resets fleet-wide.
//   - 2 -> 4: the child's sockets for workers 2-3 are new, not inherited, so
//     they are not paused, and about half of the parent's connections are
//     steered to a child worker that does not know them.
//
// #1133 made the transition crash-free; it cannot make it hitless, because the
// steering is per socket group. So when the live predecessor's worker count
// differs from the one the next Envoy would run with, the supervisor does not
// hot-restart: it drains the predecessor the way a pod termination does
// (POST /drain_listeners?graceful, wait --drain-time, then stop it), waits for
// it to be gone, and starts a NEW lineage at epoch 0 with no socket
// inheritance. The cost is a short data-plane gap on the node (the fresh
// Envoy's init) instead of resetting half the live HTTP/3 connections at an
// arbitrary point of the parent's drain. Counts that match — every restart
// except the one rollout that changes proxy.concurrency — hot-restart exactly
// as before. HotRestartOnConcurrencyChange forces the old behaviour.
//
// Only a cross-pod start can see a different count: an in-pod hot restart forks
// with this supervisor's own ExtraArgs, which never change for its lifetime.
// The check therefore lives where initStartEpoch confirms a live predecessor,
// which also covers a bind-collision retry re-running epoch detection.
//
// Trust. The predecessor is another supervisor's Envoy, so its answer cannot
// carry OUR identity. It is believed only when, on one admin connection, it
// reports LIVE at exactly the epoch the heartbeat named, carries an aether
// supervisor's --admin-address-path identity (#1127) and a positive
// concurrency. Anything else — unreachable, unparsable, no identity (a
// non-aether or pre-#1127 Envoy), another epoch — is "unknown", and unknown
// hot-restarts: the check must never block a normal roll. The drain then rides
// the very connection that was checked, and the final stop is sent only after
// the same identity and epoch are re-read on its own connection, so neither
// mutation can reach any other Envoy that took the shared admin port meanwhile.

// Handoff modes, the values of aether.supervisor.handoff_mode.
const (
	handoffModeHot             = "hot"
	handoffModeFreshAfterDrain = "fresh_after_drain"
)

// handoffModeValues is the closed set, used to seed the counter at zero.
var handoffModeValues = []string{handoffModeHot, handoffModeFreshAfterDrain}

const (
	// adminQuitPath stops an Envoy cleanly (exit status 0), the only way to end
	// another pod's Envoy: its process is in another PID namespace.
	adminQuitPath = "/quitquitquit"
	// predecessorGonePoll is how often the supervisor checks that a stopped
	// predecessor has released the admin port.
	predecessorGonePoll = 200 * time.Millisecond
	// predecessorExitSettle is waited after the admin stops answering: Envoy
	// closes its admin before its process (and its base-id domain socket and
	// shared memory) is gone. A launch that still collides is caught by the
	// bind-collision retry.
	predecessorExitSettle = 1 * time.Second
	// onlineCPUsPath lists the online CPUs. Envoy's --concurrency default is
	// std::thread::hardware_concurrency(), which both libstdc++ (get_nprocs)
	// and libc++ (sysconf(_SC_NPROCESSORS_ONLN)) derive from this list, not
	// from the process's affinity mask or cgroup.
	onlineCPUsPath = "/sys/devices/system/cpu/online"
)

// predecessorInfo is a trusted reading of the live predecessor's /server_info.
type predecessorInfo struct {
	identity    string
	epoch       int
	concurrency int
}

// adminServerInfoConcurrencyDoc extends adminServerInfoDoc with the worker
// count. Envoy reports the effective value (the hardware default included).
type adminServerInfoConcurrencyDoc struct {
	State              string `json:"state"`
	CommandLineOptions struct {
		RestartEpoch     int    `json:"restart_epoch"`
		AdminAddressPath string `json:"admin_address_path"`
		Concurrency      int    `json:"concurrency"`
	} `json:"command_line_options"`
}

// readPredecessor reads /server_info on conn and returns it only when it is
// trustworthy (see the file comment); reason says why not otherwise.
func readPredecessor(ctx context.Context, conn *adminConn, epoch int) (info predecessorInfo, reason string) {
	code, raw, err := conn.do(ctx, http.MethodGet, "/server_info", adminServerInfoBodyLimit)
	if err != nil {
		return info, "server_info unreadable: " + err.Error()
	}
	if code != http.StatusOK {
		return info, fmt.Sprintf("server_info answered HTTP %d", code)
	}
	var doc adminServerInfoConcurrencyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return info, "server_info unparsable: " + err.Error()
	}
	opts := doc.CommandLineOptions
	switch {
	case doc.State != adminLiveState:
		return info, "answering envoy is " + doc.State + ", not LIVE"
	case opts.RestartEpoch != epoch:
		return info, fmt.Sprintf("answering envoy is at epoch %d, not the predecessor's %d", opts.RestartEpoch, epoch)
	case !strings.HasPrefix(filepath.Base(opts.AdminAddressPath), adminIdentityPrefix):
		return info, "answering envoy carries no aether supervisor identity: " + adminIdentityLabel(opts.AdminAddressPath)
	case opts.Concurrency <= 0:
		return info, "answering envoy reports no concurrency"
	}
	return predecessorInfo{identity: opts.AdminAddressPath, epoch: epoch, concurrency: opts.Concurrency}, ""
}

// successorConcurrency is the --concurrency the next Envoy this supervisor
// forks will run with: the one --concurrency in ExtraArgs, else Envoy's
// default, the online CPU count.
func (s *Supervisor) successorConcurrency() (int, error) {
	n, explicit, err := concurrencyArg(s.cfg.ExtraArgs)
	if err != nil || explicit {
		return n, err
	}
	return s.onlineCPUs()
}

// concurrencyArg extracts --concurrency from an Envoy argv.
//
// A --concurrency given more than once is errRepeatedConcurrency, not "the
// last one wins": Envoy's parser does not keep the last, it refuses the command
// line ("PARSE ERROR: Argument: (--concurrency) Argument already set!"), so no
// Envoy ever runs with either value (issue #1375). The repeat is reported
// before any value is looked at. CheckExtraArgs turns it into a startup
// failure, so a supervisor started through the command never sees it here.
func concurrencyArg(args []string) (n int, explicit bool, err error) {
	var (
		v       string
		seen    int
		noValue bool
	)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == envoyFlagConcurrency:
			// The next argument is the value, whatever it looks like.
			i++
			if noValue = i >= len(args); !noValue {
				v = args[i]
			}
		case strings.HasPrefix(a, envoyFlagConcurrency+"="):
			v = strings.TrimPrefix(a, envoyFlagConcurrency+"=")
		default:
			continue
		}
		seen++
	}
	switch {
	case seen == 0:
		return 0, false, nil
	case seen > 1:
		return 0, false, fmt.Errorf("%w (%d times)", errRepeatedConcurrency, seen)
	case noValue:
		return 0, false, fmt.Errorf("--concurrency without a value")
	}
	parsed, perr := strconv.Atoi(v)
	if perr != nil || parsed <= 0 {
		return 0, false, fmt.Errorf("--concurrency %q is not a positive integer", v)
	}
	return parsed, true, nil
}

// readOnlineCPUs counts the CPUs in onlineCPUsPath ("0-3", "0,2-5", ...).
func readOnlineCPUs() (int, error) {
	raw, err := os.ReadFile(onlineCPUsPath)
	if err != nil {
		return 0, err
	}
	return parseCPUList(strings.TrimSpace(string(raw)))
}

func parseCPUList(list string) (int, error) {
	if list == "" {
		return 0, fmt.Errorf("empty cpu list")
	}
	total := 0
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return 0, fmt.Errorf("cpu list %q: %w", list, err)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return 0, fmt.Errorf("cpu list %q: %w", list, err)
			}
		}
		if b < a {
			return 0, fmt.Errorf("cpu list %q: descending range", list)
		}
		total += b - a + 1
	}
	return total, nil
}

// freshStartInsteadOfHotRestart is consulted by initStartEpoch once a live
// predecessor at epoch is confirmed. It returns true when it has drained and
// stopped that predecessor because its worker count differs from the next
// Envoy's, and the caller must start a fresh lineage at epoch 0; false means
// hot-restart at epoch+1 as before. Every path records the handoff mode.
func (s *Supervisor) freshStartInsteadOfHotRestart(ctx context.Context, epoch int) bool {
	probeCtx, cancel := context.WithTimeout(ctx, shutdownProbeTimeout)
	defer cancel()
	conn, err := dialAdmin(probeCtx, s.cfg.AdminAddress)
	if err != nil {
		return s.hotRestartAnyway(ctx, epoch, "predecessor admin unreachable for the worker-count check: "+err.Error())
	}
	defer func() { _ = conn.Close() }()

	pred, why := readPredecessor(probeCtx, conn, epoch)
	if why != "" {
		return s.hotRestartAnyway(ctx, epoch, why)
	}
	ours, err := s.successorConcurrency()
	if err != nil {
		return s.hotRestartAnyway(ctx, epoch, "successor worker count unknown: "+err.Error())
	}
	if ours == pred.concurrency {
		s.metrics.handoffMode(handoffModeHot)
		return false
	}
	if s.drainedPredecessor != "" && s.drainedPredecessor == pred.identity {
		// Already drained once and it is still LIVE (its stop failed): never
		// drain the same Envoy twice; hot-restart from it as before #1136.
		return s.hotRestartAnyway(ctx, epoch, "predecessor already drained once and still live")
	}
	if s.cfg.HotRestartOnConcurrencyChange {
		s.log.WarnContext(ctx, "envoy worker count changes across this handoff; hot-restarting anyway "+
			"(--hot-restart-on-concurrency-change): about half of the predecessor's live QUIC connections "+
			"will be mis-steered and reset (#1136)",
			"predecessorConcurrency", pred.concurrency, "successorConcurrency", ours,
			"predecessorEpoch", epoch, "predecessor", adminIdentityLabel(pred.identity))
		s.metrics.handoffMode(handoffModeHot)
		return false
	}

	s.log.WarnContext(ctx, "envoy worker count changes across this handoff; NOT hot-restarting: draining the "+
		"predecessor, then starting a fresh envoy at epoch 0 (a hot restart would re-steer the predecessor's "+
		"QUIC connections by the new count and reset about half of them, #1136)",
		"predecessorConcurrency", pred.concurrency, "successorConcurrency", ours,
		"predecessorEpoch", epoch, "predecessor", adminIdentityLabel(pred.identity),
		"drainTime", s.cfg.DrainTime)
	start := time.Now()
	// The drain rides the connection whose answer was just checked.
	status, body, err := conn.do(probeCtx, http.MethodPost, adminDrainPath, adminDrainBodyLimit)
	if err != nil || status != http.StatusOK {
		return s.hotRestartAnyway(ctx, epoch, fmt.Sprintf("predecessor did not accept the graceful drain (status %d, body %q, error %v)",
			status, strings.TrimSpace(string(body)), err))
	}
	s.metrics.handoffMode(handoffModeFreshAfterDrain)
	s.mu.Lock()
	s.drainedPredecessor = pred.identity
	// From here on this supervisor's Envoys are a new lineage that may share
	// epoch numbers with the predecessor's: readiness must come from an answer
	// that carries our own identity, never from a predecessor still answering
	// at the same epoch.
	s.readyRequiresOwnIdentity = true
	s.mu.Unlock()

	sleepCtx(ctx, s.cfg.DrainTime)
	stopped := s.stopPredecessor(ctx, pred)
	gone := s.awaitPredecessorGone(ctx, pred, s.cfg.DrainTime+shutdownGrace)
	if gone {
		sleepCtx(ctx, predecessorExitSettle)
	}
	s.log.InfoContext(ctx, "predecessor drained for a worker-count change; starting a fresh envoy lineage",
		"predecessorEpoch", epoch, "stopSent", stopped, "predecessorGone", gone,
		"elapsed", time.Since(start).Round(time.Millisecond).String())
	return true
}

// hotRestartAnyway logs why the worker-count check could not decide and
// records the default, a hot restart.
func (s *Supervisor) hotRestartAnyway(ctx context.Context, epoch int, reason string) bool {
	s.log.InfoContext(ctx, "worker-count check inconclusive; hot-restarting from the predecessor as usual",
		"predecessorEpoch", epoch, "reason", reason)
	s.metrics.handoffMode(handoffModeHot)
	return false
}

// stopPredecessor asks the drained predecessor to exit, but only after its
// identity and epoch are re-read on the connection that carries the request.
func (s *Supervisor) stopPredecessor(ctx context.Context, pred predecessorInfo) bool {
	reqCtx, cancel := context.WithTimeout(ctx, shutdownProbeTimeout)
	defer cancel()
	conn, err := dialAdmin(reqCtx, s.cfg.AdminAddress)
	if err != nil {
		s.log.InfoContext(ctx, "drained predecessor's admin is already gone; not sending a stop", "error", err)
		return false
	}
	defer func() { _ = conn.Close() }()
	again, why := readPredecessor(reqCtx, conn, pred.epoch)
	if why != "" || again.identity != pred.identity {
		s.log.WarnContext(ctx, "not stopping the drained predecessor: the admin no longer answers as it",
			"reason", why, "answeredBy", adminIdentityLabel(again.identity),
			"predecessor", adminIdentityLabel(pred.identity))
		return false
	}
	status, _, err := conn.do(reqCtx, http.MethodPost, adminQuitPath, adminDrainBodyLimit)
	if err != nil || status != http.StatusOK {
		s.log.WarnContext(ctx, "drained predecessor did not accept the stop", "status", status, "error", err)
		return false
	}
	return true
}

// awaitPredecessorGone waits up to budget until the admin no longer answers as
// the predecessor. It reports whether it saw it go.
func (s *Supervisor) awaitPredecessorGone(ctx context.Context, pred predecessorInfo, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if !s.adminAnswersAs(ctx, pred) {
			return true
		}
		if time.Now().After(deadline) {
			s.log.WarnContext(ctx, "drained predecessor still answers its admin; starting the fresh envoy anyway "+
				"(a base-id collision is retried in-process)", "waited", budget)
			return false
		}
		if !sleepCtx(ctx, predecessorGonePoll) {
			return false
		}
	}
}

// adminAnswersAs reports whether the node admin currently answers with pred's
// identity at all (any state: a shutting-down Envoy is still there).
func (s *Supervisor) adminAnswersAs(ctx context.Context, pred predecessorInfo) bool {
	reqCtx, cancel := context.WithTimeout(ctx, readyPollInterval)
	defer cancel()
	conn, err := dialAdmin(reqCtx, s.cfg.AdminAddress)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	code, raw, err := conn.do(reqCtx, http.MethodGet, "/server_info", adminServerInfoBodyLimit)
	if err != nil || code != http.StatusOK {
		// Answering, but not as anything identifiable: a shutting-down admin.
		return err == nil
	}
	var doc adminServerInfoDoc
	if json.Unmarshal(raw, &doc) != nil {
		return true
	}
	return doc.CommandLineOptions.AdminAddressPath == pred.identity
}

// sleepCtx sleeps for d or until ctx is done; it reports whether the full
// duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
