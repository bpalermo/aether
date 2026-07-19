// Package hotrestart implements a supervisor that manages the aether-proxy Envoy
// process and performs Envoy hot restarts across restart epochs, replicating the
// behavior of Envoy's hot-restarter.py in Go (see
// docs/proposals/001_proxy-hot-restart.md).
//
// Model: the supervisor is the proxy container's entrypoint (PID 1). It forks an
// Envoy child with --restart-epoch 0 and a fixed --base-id. On a hot-restart
// trigger (a watched bootstrap-config change or SIGHUP) it forks a new Envoy with
// the next epoch; Envoy's own shared-memory + abstract-domain-socket IPC transfers
// the listen-socket FDs and stats to the new process, the old process drains, and
// after --parent-shutdown-time-s the supervisor terminates it.
//
// The handoff also works ACROSS the pod boundary during a surge upgrade: the
// overlapping old and new aether-proxy pods share the node's network namespace,
// /dev/shm (a hostPath) and the same --base-id, so the new pod's Envoy hot-restarts
// from the old pod's. The supervisors coordinate the per-node restart epoch through
// a heartbeat file on the shared StateDir and gate pod readiness (ReadyMarkerPath)
// so the DaemonSet keeps the predecessor until the successor has taken over.
package hotrestart

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/fsnotify/fsnotify"
)

const (
	// debounceDelay coalesces a flurry of triggers (config rewrites, repeated
	// SIGHUPs) into a single hot restart.
	debounceDelay = 500 * time.Millisecond
	// shutdownGrace is added to DrainTime as the deadline for children to exit on
	// SIGTERM before they are SIGKILLed.
	shutdownGrace = 5 * time.Second
	// defaultHandoffDeadline bounds how long a hot-restart epoch (N>0) may stay
	// not-LIVE after launch before the handoff watchdog declares it wedged. The
	// observed failure mode (e2e 2026-06-10): the parent Envoy dies between a
	// hot-restart RPC request and its reply, leaving the successor's main thread
	// blocked forever in recvmsg on the hot-restart domain socket — admin bound
	// but never accepting, pod NotReady forever, DaemonSet roll wedged.
	defaultHandoffDeadline = 2 * time.Minute
	// defaultAdminUnresponsiveDeadline bounds how long the Envoy admin endpoint
	// may be unreachable (connect/timeout failures, not "answers with another
	// epoch") once this supervisor has seen LIVE, before the admin watchdog
	// fires. Covers the same recvmsg wedge striking after LIVE (a parent dying
	// mid stats-merge). A reachable admin answering at a different epoch — the
	// normal mid-handoff state — never trips this.
	defaultAdminUnresponsiveDeadline = 30 * time.Second

	// Bind-collision retry. When epoch detection cannot
	// confirm a predecessor (stale heartbeat: its admin probes were timing out
	// under node load, so its LIVE-gated heartbeat stopped) but the predecessor's
	// Envoy is in fact still alive, a fresh epoch-0 launch loses the base-id
	// domain-socket bind race ("unable to bind domain socket ... errno=98") and
	// exits within milliseconds. Exiting the supervisor non-zero hands the retry
	// to the kubelet's CrashLoopBackOff (10→20→40→80s gaps; observed as a ~165s
	// node data-plane gap, e2e 2026-06-11) — instead, a newest epoch that dies
	// non-LIVE within bindCollisionWindow of launch is retried in-process on a
	// tight cadence: re-run epoch detection (the predecessor may have become
	// confirmable, or finally exited) and relaunch. The predecessor keeps
	// serving the node's traffic the whole time; the first attempt after it
	// exits binds cleanly. Bounded by maxBindCollisionRetries so a genuinely
	// broken Envoy (a bad bootstrap also dies fast) still surfaces as a pod
	// crash; the budget comfortably exceeds the predecessor pod's 180s
	// termination grace, the latest the collision can possibly resolve.
	bindCollisionWindow     = 5 * time.Second
	bindCollisionRetryPause = 3 * time.Second
	maxBindCollisionRetries = 90
	// maxCrashRetries bounds in-process retries of an epoch that died on a fatal
	// SIGNAL (SIGSEGV/SIGABRT) rather than a clean bind-collision exit. A crash is
	// not a transient socket race, so it gets a small budget: a deterministically
	// crashing Envoy (e.g. a CDS referencing a gone netns — talos worker-01,
	// 2026-06-19) surfaces as CrashLoopBackOff in ~15s instead of looping silently
	// for the full 4.5-min bind-collision budget while masquerading as a collision.
	maxCrashRetries = 5
)

// Config configures the Envoy hot-restart supervisor.
type Config struct {
	// EnvoyPath is the path to the Envoy binary.
	EnvoyPath string
	// ConfigPath is the Envoy bootstrap config (-c). A change to this file (when
	// WatchConfig is set) triggers a hot restart.
	ConfigPath string
	// BaseID is Envoy's --base-id, pinned so successive epochs find the same
	// shared-memory segment. Must be stable for the life of the container.
	BaseID uint32
	// DrainTime maps to Envoy --drain-time-s: how long the draining (old) epoch
	// takes to gracefully close connections.
	DrainTime time.Duration
	// ParentShutdownTime maps to Envoy --parent-shutdown-time-s and gates when the
	// supervisor SIGTERMs the previous epoch. Must exceed DrainTime.
	ParentShutdownTime time.Duration
	// ExtraArgs are appended to every Envoy invocation (e.g. -l, --service-cluster,
	// --service-node, --service-zone, --concurrency). Concurrency must stay constant
	// across epochs to avoid dropping accept-queue connections.
	ExtraArgs []string
	// WatchConfig enables an fsnotify watch on ConfigPath's directory that
	// self-triggers a hot restart when the bootstrap config changes (e.g. a
	// ConfigMap update propagated by the kubelet).
	WatchConfig bool
	// StateDir is the shared-hostPath dir holding the per-node epoch heartbeat
	// file. A surging successor pod reads it to start at (live predecessor epoch +
	// 1) and hot-restart across the pod boundary. Required.
	StateDir string
	// ReadyMarkerPath is the pod-local readiness marker the supervisor keeps
	// present only while the node's Envoy admin reports LIVE at this supervisor's
	// newest epoch. An exec readiness probe checks the marker so the DaemonSet
	// keeps the old pod until the new one has taken over. Required.
	ReadyMarkerPath string
	// AdminAddress is the Envoy admin host:port used for the readiness check.
	AdminAddress string
	// HandoffDeadline overrides defaultHandoffDeadline (0 = default). Must be
	// comfortably larger than ParentShutdownTime plus worst-case xDS-gated init.
	HandoffDeadline time.Duration
	// AdminUnresponsiveDeadline overrides defaultAdminUnresponsiveDeadline
	// (0 = default).
	AdminUnresponsiveDeadline time.Duration
}

// childExit reports the termination of a supervised Envoy epoch.
type childExit struct {
	epoch int
	err   error
}

// Supervisor owns the Envoy process lifecycle and performs hot restarts.
type Supervisor struct {
	cfg     Config
	log     *slog.Logger
	metrics *SupervisorMetrics // nil disables instrumentation

	mu        sync.Mutex
	children  map[int]*exec.Cmd // keyed by restart epoch
	nextEpoch int
	// epochLaunched / epochLive track the newest epoch's progress toward LIVE,
	// feeding the handoff watchdog: launched records the fork time, live whether
	// admin has confirmed LIVE at that epoch at least once.
	epochLaunched time.Time
	epochLive     bool

	childExited chan childExit
	done        chan struct{}
	// watchdogFired carries a fatal diagnosis from watchLiveness to Run: the
	// newest Envoy is wedged (handoff never LIVE, or admin unresponsive) and the
	// container must exit non-zero so Kubernetes recreates the pod.
	watchdogFired chan error

	// readyGate delays the pod's readiness until after a cross-pod handoff is fully
	// complete: the successor Envoy terminates the predecessor itself via
	// --parent-shutdown-time-s, so the pod must not report Ready (which lets the
	// DaemonSet delete the old pod) until that has elapsed — otherwise the old
	// Envoy is killed out from under the still-attached successor (errno 111).
	// Guarded by mu: bind-collision retries re-run epoch detection (and re-gate)
	// while watchLiveness is already polling.
	readyGate time.Time
}

// setReadyGate / readyGateTime guard readyGate for concurrent access between
// Run (bind-collision retries) and watchLiveness.
func (s *Supervisor) setReadyGate(t time.Time) {
	s.mu.Lock()
	s.readyGate = t
	s.mu.Unlock()
}

func (s *Supervisor) readyGateTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyGate
}

// gateReadinessIfSuccessor delays readiness when epoch detection selected a
// cross-pod successor epoch (>0): the predecessor must be terminated by this
// Envoy's own parent-shutdown protocol before the pod may report Ready.
func (s *Supervisor) gateReadinessIfSuccessor() {
	s.mu.Lock()
	successor := s.nextEpoch > 0
	s.mu.Unlock()
	if successor {
		s.setReadyGate(time.Now().Add(s.cfg.ParentShutdownTime + readyGateBuffer))
	}
}

// readyGateBuffer is added to ParentShutdownTime when gating a cross-pod
// successor's readiness, to ensure the predecessor is fully gone first.
const readyGateBuffer = 3 * time.Second

// New creates a Supervisor. metrics may be nil to disable instrumentation.
func New(cfg Config, log *slog.Logger, metrics *SupervisorMetrics) *Supervisor {
	return &Supervisor{
		cfg:           cfg,
		log:           commonlog.Named(log, "proxy-supervisor"),
		metrics:       metrics,
		children:      make(map[int]*exec.Cmd),
		childExited:   make(chan childExit, 8),
		done:          make(chan struct{}),
		watchdogFired: make(chan error, 1),
	}
}

// Run starts Envoy at epoch 0 and supervises it until ctx is canceled (SIGTERM/
// SIGINT, which controller-runtime's signal handler maps to ctx.Done) or the
// newest epoch exits unexpectedly. A watched-config change or SIGHUP triggers a
// hot restart; SIGUSR1 is forwarded to the current child for log reopen.
func (s *Supervisor) Run(ctx context.Context) error {
	defer close(s.done)

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(sigCh)

	trigger := make(chan struct{}, 1)
	if s.cfg.WatchConfig {
		go s.watchConfig(ctx, trigger)
	}

	// Pick the start epoch from a confirmed-live predecessor (if any), then
	// maintain the readiness marker and the LIVE-gated node epoch heartbeat.
	s.initStartEpoch(ctx)
	s.gateReadinessIfSuccessor()
	go s.watchLiveness(ctx)

	if err := s.hotRestart(); err != nil {
		return fmt.Errorf("starting initial envoy epoch: %w", err)
	}

	lp := &restartLoop{
		s:        s,
		ctx:      ctx,
		debounce: time.NewTimer(debounceDelay),
	}
	lp.debounce.Stop()
	return lp.run(sigCh, trigger)
}

// restartLoop holds the mutable state for the Run event loop, allowing the loop
// body to be split into helpers without passing every variable as a parameter.
type restartLoop struct {
	s            *Supervisor
	ctx          context.Context
	debounce     *time.Timer
	debounceC    <-chan time.Time
	bindRetries  int
	crashRetries int
}

// arm arms (or re-arms) the debounce timer for a hot-restart trigger.
func (lp *restartLoop) arm(reason string) {
	lp.s.log.DebugContext(lp.ctx, "hot restart armed", "reason", reason, "debounce", debounceDelay)
	lp.s.metrics.restartTriggered(reason)
	lp.debounce.Reset(debounceDelay)
	lp.debounceC = lp.debounce.C
}

// run is the event loop for the hot-restart supervisor.
func (lp *restartLoop) run(sigCh <-chan os.Signal, trigger <-chan struct{}) error {
	for {
		select {
		case <-lp.ctx.Done():
			return lp.s.handleShutdown(lp.ctx)

		case sig := <-sigCh:
			lp.handleSignal(sig)

		case <-trigger:
			lp.arm("config_change")

		case <-lp.debounceC:
			lp.debounceC = nil
			if err := lp.handleDebounce(); err != nil {
				return err
			}

		case err := <-lp.s.watchdogFired:
			// The newest Envoy is wedged (see watchLiveness): kill everything and
			// exit non-zero. Kubernetes recreates the pod; the fresh supervisor
			// re-probes the (now dead) predecessor and recovers at epoch 0.
			lp.s.log.ErrorContext(lp.ctx, "liveness watchdog fired; terminating for container restart", "error", err)
			lp.s.shutdown()
			return err

		case exit := <-lp.s.childExited:
			retErr, done := lp.handleChildExit(exit)
			if done {
				return retErr
			}
		}
	}
}

// handleSignal dispatches an OS signal received by Run.
func (lp *restartLoop) handleSignal(sig os.Signal) {
	switch sig {
	case syscall.SIGHUP:
		lp.arm("sighup")
	case syscall.SIGUSR1:
		lp.s.forwardToCurrent(syscall.SIGUSR1)
	}
}

// handleDebounce is called when the debounce timer fires. It defers or performs
// the hot restart, returning a non-nil error on fatal failure.
func (lp *restartLoop) handleDebounce() error {
	rearm, err := lp.s.handleDebounce(lp.ctx)
	if err != nil {
		return err
	}
	if rearm != "" {
		lp.arm(rearm)
	}
	return nil
}

// handleChildExit is called when a child process exits. Returns (err, done=true)
// when the supervisor should exit.
func (lp *restartLoop) handleChildExit(exit childExit) (retErr error, done bool) {
	retErr, done, rearm := lp.s.handleChildExit(lp.ctx, exit, &lp.bindRetries, &lp.crashRetries)
	if rearm != "" {
		lp.arm(rearm)
	}
	return retErr, done
}

// handleShutdown implements the ctx.Done case of the Run select: if this
// supervisor is in the mid-handoff window (Envoy not LIVE at our epoch), it
// waits for the successor's parent-shutdown protocol to terminate our Envoy
// before returning; otherwise it signals all children and drains.
func (s *Supervisor) handleShutdown(ctx context.Context) error {
	// If our Envoy no longer answers admin LIVE at our own epoch, its
	// sockets have (very likely) been transferred to a surging successor
	// that is still initializing — the DaemonSet deletes this pod the moment
	// it turns NotReady, which is exactly that window. Do NOT signal Envoy
	// (the successor still needs its hot-restart parent alive, even before
	// reaching LIVE — killing it aborts the successor with errno 111): wait
	// for the successor's parent-shutdown protocol to terminate it, with a
	// deadline fallback. The check cannot rely on the successor having
	// published its epoch, since it does so only once LIVE.
	//
	// StateDir is always set in production (the supervisor only runs in the
	// cross-pod configuration); the guard keeps unit-test supervisors that
	// run without coordination state on the plain shutdown path.
	if s.cfg.StateDir != "" && !s.adminLiveAtEpoch(ctx, s.currentEpoch()) {
		s.log.InfoContext(ctx, "termination requested mid-handoff; waiting for successor to terminate our envoy")
		s.awaitProtocolTermination()
		return nil
	}
	s.log.InfoContext(ctx, "termination requested, shutting down all envoy epochs")
	s.shutdown()
	return nil
}

// handleDebounce implements the debounceC case of the Run select: defers the
// restart if the current epoch is not yet LIVE, validates the config, then
// performs the hot restart. Returns a non-empty rearm reason if the caller
// should re-arm the debounce, or a non-nil error on fatal failure.
func (s *Supervisor) handleDebounce(ctx context.Context) (rearmReason string, err error) {
	// Defer the restart while the current epoch is still initializing:
	// forking epoch N+1 against a not-yet-LIVE N makes Envoy exit with
	// "previous envoy process is still initializing", which the main loop
	// treats as a fatal newest-epoch death (container restart, brief node
	// data-plane gap). Re-arm and retry once N is LIVE. Skipped when no
	// admin address is configured.
	if s.cfg.AdminAddress != "" && !s.adminLiveAtEpoch(ctx, s.currentEpoch()) {
		s.log.DebugContext(ctx, "current epoch not yet live; deferring hot restart", "epoch", s.currentEpoch())
		return "deferred_not_live", nil
	}
	// Pre-validate the changed bootstrap ON THIS NODE before forking
	// the new epoch. Every supervisor sees a ConfigMap change at the
	// same time (fsnotify), so a config that fails at runtime takes
	// down every node's data plane simultaneously, bypassing all
	// rollout safety — observed 2026-06-11 (rev 64): a resource
	// monitor that validated fine in docker was fatal in the
	// privileged pod environment, and the fleet-wide simultaneous hot
	// restart turned it into a 13-minute cluster outage. envoy
	// --mode validate executes bootstrap initialization in the same
	// environment as the real fork, so it catches exactly that
	// class. On failure: keep the current epoch serving, count it,
	// and wait for the next config change.
	if err := s.validateConfig(ctx); err != nil {
		s.metrics.configValidationFailed()
		s.log.ErrorContext(ctx, "changed bootstrap config failed node-local validation; KEEPING current epoch (hot restart skipped)", "error", err)
		return "", nil
	}
	s.log.InfoContext(ctx, "performing hot restart")
	if err := s.hotRestart(); err != nil {
		s.log.ErrorContext(ctx, "hot restart failed; keeping current epoch", "error", err)
	}
	return "", nil
}

// handleChildExit implements the childExited case of the Run select loop.
// It returns (err, done=true) when the supervisor should exit, or
// (nil, false) to continue; rearmReason is non-empty if the arm function
// should be called (bind-collision retry re-arms via continue, so the
// caller must call arm before continuing the loop).
func (s *Supervisor) handleChildExit(ctx context.Context, exit childExit, bindRetries, crashRetries *int) (retErr error, done bool, rearmReason string) {
	if exit.epoch != s.currentEpoch() {
		// An older epoch finished draining after a hot restart: expected. Reap it.
		s.metrics.childExited(exitDrained)
		s.reap(exit.epoch)
		return nil, false, ""
	}
	if exit.err == nil {
		s.metrics.childExited(exitSuccessorTerminated)
		// Clean exit (status 0) of our newest epoch: that's the
		// successor's hot-restart parent-shutdown protocol terminating us
		// (a crash would be non-zero/signaled). The signal
		// is deliberate process state, not the shared epoch file — the
		// successor publishes its epoch only once LIVE, which may be
		// after it terminates us. Don't exit (restartPolicy=Always would
		// relaunch and collide): await deletion by the DaemonSet.
		s.log.InfoContext(ctx, "newest epoch terminated cleanly by successor; awaiting pod deletion", "epoch", exit.epoch)
		s.reap(exit.epoch)
		<-ctx.Done()
		return nil, true, ""
	}
	if retried, retErr, done := s.retryBindCollision(ctx, exit, bindRetries, crashRetries); retried {
		return retErr, done, ""
	}
	// The newest epoch died unexpectedly: nothing left serving traffic.
	// Bail non-zero so Kubernetes recreates the pod (SIGCHLD-fatal).
	s.metrics.childExited(exitUnexpected)
	s.reap(exit.epoch)
	s.shutdown()
	return fmt.Errorf("envoy epoch %d exited unexpectedly: %w", exit.epoch, exit.err), true, ""
}

// retryBindCollision handles the bind-collision / crash-on-launch retry logic
// for a newest epoch that died non-LIVE within seconds of launch. Returns
// (retried=true, err, done) when the case was handled (either retried or
// budget exhausted non-retried), (false, nil, false) when this was not a
// quick non-live exit and the caller should fall through.
func (s *Supervisor) retryBindCollision(ctx context.Context, exit childExit, bindRetries, crashRetries *int) (retried bool, retErr error, done bool) {
	// A fresh epoch that died non-LIVE within seconds: either a base-id
	// bind collision (a predecessor still holds the domain socket; exits
	// errno 98, no signal) or a genuine Envoy crash (a fatal SIGNAL).
	// Retry epoch detection in-process — a predecessor keeps serving
	// meanwhile — instead of exiting into CrashLoopBackOff. A crash gets
	// a much smaller budget so a deterministically crashing Envoy
	// surfaces fast instead of masquerading as a collision for minutes.
	if !s.quickNonLiveExit() {
		return false, nil, false
	}
	crash := isCrashSignal(exit.err)
	attempt, budget := *bindRetries+1, maxBindCollisionRetries
	if crash {
		attempt, budget = *crashRetries+1, maxCrashRetries
	}
	if attempt > budget {
		return false, nil, false
	}
	if crash {
		*crashRetries++
	} else {
		*bindRetries++
	}
	s.metrics.childExited(exitBindCollision)
	s.reap(exit.epoch)
	kind := "suspected base-id bind collision with a live predecessor"
	if crash {
		kind = "envoy crashed on launch (fatal signal)"
	}
	s.log.InfoContext(ctx, kind+"; retrying epoch detection in-process",
		"epoch", exit.epoch, "attempt", attempt, "budget", budget, "crashSignal", crash, "error", exit.err.Error())
	select {
	case <-ctx.Done():
		s.shutdown()
		return true, nil, true
	case <-time.After(bindCollisionRetryPause):
	}
	s.resetEpochForRetry()
	s.initStartEpoch(ctx)
	s.gateReadinessIfSuccessor()
	if err := s.hotRestart(); err != nil {
		return true, fmt.Errorf("relaunching envoy after retry: %w", err), true
	}
	return true, nil, false
}

// hotRestart forks a new Envoy child at the next restart epoch and schedules
// shutdown of the previous one after ParentShutdownTime.
func (s *Supervisor) hotRestart() error {
	s.mu.Lock()
	epoch := s.nextEpoch
	s.nextEpoch++
	s.mu.Unlock()

	cmd := s.buildEnvoyCmd(epoch)
	s.log.Info("starting envoy", "epoch", epoch, "args", cmd.Args)
	if err := cmd.Start(); err != nil {
		return err
	}

	s.mu.Lock()
	s.children[epoch] = cmd
	s.epochLaunched = time.Now()
	s.epochLive = false
	s.mu.Unlock()
	s.metrics.epochStarted(epoch)

	// The node epoch is published to the shared state file only once this Envoy is
	// confirmed LIVE (by watchLiveness), never at launch — so a failed handoff does
	// not advance the epoch and cause a restart to climb against a dead parent.

	go func() {
		err := cmd.Wait()
		select {
		case s.childExited <- childExit{epoch: epoch, err: err}:
		case <-s.done:
		}
	}()

	// Do NOT externally terminate the previous epoch: Envoy coordinates parent
	// shutdown itself over the hot-restart IPC socket, driven by the new epoch's
	// --parent-shutdown-time-s. Killing the parent out from under that protocol
	// makes the new epoch's sendmsg to the parent fail (errno 111) and Envoy
	// aborts. The old epoch exits on its own; Run reaps it as a non-newest exit.
	return nil
}

// watchConfig watches the directory holding ConfigPath and emits a trigger on any
// change. Watching the directory (not the file) survives the atomic symlink swap
// the kubelet uses to update ConfigMap mounts. Coalescing is handled downstream by
// the debounce timer.
func (s *Supervisor) watchConfig(ctx context.Context, trigger chan<- struct{}) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		s.log.ErrorContext(ctx, "config watcher disabled", "error", err)
		return
	}
	defer func() { _ = w.Close() }()

	dir := filepath.Dir(s.cfg.ConfigPath)
	if err := w.Add(dir); err != nil {
		s.log.ErrorContext(ctx, "failed to watch config dir; watcher disabled", "error", err, "dir", dir)
		return
	}
	s.log.InfoContext(ctx, "watching bootstrap config for changes", "dir", dir, "config", s.cfg.ConfigPath)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case _, ok := <-w.Events:
			if !ok {
				return
			}
			select {
			case trigger <- struct{}{}:
			default: // a trigger is already pending; the debounce will coalesce.
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			s.log.ErrorContext(ctx, "config watch error", "error", err)
		}
	}
}

// configValidateTimeout bounds the node-local `envoy --mode validate` run. A
// hung validation must not wedge the trigger loop; validation of this
// bootstrap takes well under a second normally.
const configValidateTimeout = 30 * time.Second

// validateConfig runs `envoy --mode validate` against the (changed) bootstrap
// in the exact environment the real fork would use — same binary, same
// container, same cgroup/namespace context — so environment-dependent
// bootstrap failures (the class that docker-side validation cannot catch)
// are detected before the serving epoch is put at risk. ExtraArgs are passed
// through because the config may reference --service-cluster/--service-node.
func (s *Supervisor) validateConfig(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, configValidateTimeout)
	defer cancel()

	args := append([]string{"--mode", "validate", "-c", s.cfg.ConfigPath}, s.cfg.ExtraArgs...)
	out, err := exec.CommandContext(ctx, s.cfg.EnvoyPath, args...).CombinedOutput()
	if err != nil {
		tail := out
		if len(tail) > 2048 {
			tail = tail[len(tail)-2048:]
		}
		return fmt.Errorf("envoy --mode validate: %w; output tail: %s", err, string(tail))
	}
	return nil
}

// buildEnvoyCmd constructs the Envoy invocation for a given restart epoch.
func (s *Supervisor) buildEnvoyCmd(epoch int) *exec.Cmd {
	args := []string{
		"-c", s.cfg.ConfigPath,
		"--base-id", strconv.FormatUint(uint64(s.cfg.BaseID), 10),
		"--restart-epoch", strconv.Itoa(epoch),
		"--drain-time-s", strconv.Itoa(int(s.cfg.DrainTime.Seconds())),
		"--parent-shutdown-time-s", strconv.Itoa(int(s.cfg.ParentShutdownTime.Seconds())),
	}
	args = append(args, s.cfg.ExtraArgs...)

	cmd := exec.Command(s.cfg.EnvoyPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// currentEpoch returns the highest (newest) epoch started so far.
func (s *Supervisor) currentEpoch() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextEpoch - 1
}

// epochProgress returns the newest epoch's launch time and whether it has been
// confirmed LIVE at least once.
func (s *Supervisor) epochProgress() (launched time.Time, live bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epochLaunched, s.epochLive
}

// quickNonLiveExit reports whether the newest epoch died without ever reaching
// LIVE and within bindCollisionWindow of launch — the bind-collision signature
// (a base-id domain-socket bind failure exits in milliseconds, long before any
// xDS-gated initialization could fail).
func (s *Supervisor) quickNonLiveExit() bool {
	launched, live := s.epochProgress()
	return !live && time.Since(launched) < bindCollisionWindow
}

// resetEpochForRetry rewinds epoch selection so a bind-collision retry re-runs
// initStartEpoch from scratch (attach at E+1 if the predecessor has become
// confirmable, else epoch 0 again once the socket is free).
func (s *Supervisor) resetEpochForRetry() {
	s.mu.Lock()
	s.nextEpoch = 0
	s.mu.Unlock()
}

// markEpochLive records that the newest epoch has been confirmed LIVE.
func (s *Supervisor) markEpochLive() {
	s.mu.Lock()
	s.epochLive = true
	s.mu.Unlock()
}

// childTracked reports whether the child for the given epoch is still tracked
// (started and not yet reaped).
func (s *Supervisor) childTracked(epoch int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.children[epoch]
	return ok
}

func (s *Supervisor) handoffDeadline() time.Duration {
	if s.cfg.HandoffDeadline > 0 {
		return s.cfg.HandoffDeadline
	}
	return defaultHandoffDeadline
}

func (s *Supervisor) adminUnresponsiveDeadline() time.Duration {
	if s.cfg.AdminUnresponsiveDeadline > 0 {
		return s.cfg.AdminUnresponsiveDeadline
	}
	return defaultAdminUnresponsiveDeadline
}

// fireWatchdog delivers a fatal wedge diagnosis to Run (at most one is ever
// consumed; extra fires are dropped).
func (s *Supervisor) fireWatchdog(err error) {
	select {
	case s.watchdogFired <- err:
	default:
	}
}

// signalEpoch sends sig to the child for the given epoch, if still tracked.
func (s *Supervisor) signalEpoch(epoch int, sig syscall.Signal) {
	s.mu.Lock()
	cmd, ok := s.children[epoch]
	s.mu.Unlock()
	if !ok || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(sig); err != nil {
		s.log.Error("failed to signal envoy epoch", "error", err, "epoch", epoch, "signal", sig)
	}
}

// forwardToCurrent forwards sig to the newest epoch (e.g. SIGUSR1 log reopen).
func (s *Supervisor) forwardToCurrent(sig syscall.Signal) {
	s.signalEpoch(s.currentEpoch(), sig)
}

// reap removes a finished epoch from tracking.
func (s *Supervisor) reap(epoch int) {
	s.mu.Lock()
	delete(s.children, epoch)
	s.mu.Unlock()
	s.log.Info("reaped envoy epoch", "epoch", epoch)
}

// awaitProtocolTermination waits (without signaling) for the remaining children to
// exit via the successor's hot-restart parent-shutdown protocol. It deliberately
// imposes NO deadline of its own: the successor's timers start only after its
// (xDS-gated, unbounded) init completes, and the successor keeps using the parent
// socket (stat merges) right up to protocol-terminate — killing the parent at any
// "reasonable" cutoff aborts the successor with errno 111. The kubelet's SIGKILL
// at the pod's terminationGracePeriod is the real, and only safe, hard stop.
func (s *Supervisor) awaitProtocolTermination() {
	s.mu.Lock()
	pending := len(s.children)
	s.mu.Unlock()

	for pending > 0 {
		exit := <-s.childExited
		s.reap(exit.epoch)
		pending--
		s.log.Info("envoy epoch terminated by successor", "epoch", exit.epoch)
	}
}

// shutdown SIGTERMs every tracked epoch and waits up to DrainTime+grace for them
// to exit, SIGKILLing any straggler. It reads childExited directly because the
// main loop has stopped selecting on it.
func (s *Supervisor) shutdown() {
	s.mu.Lock()
	pending := make(map[int]struct{}, len(s.children))
	for e := range s.children {
		pending[e] = struct{}{}
	}
	s.mu.Unlock()

	for e := range pending {
		s.signalEpoch(e, syscall.SIGTERM)
	}
	if len(pending) == 0 {
		return
	}
	start := time.Now()
	defer func() { s.metrics.drainCompleted(time.Since(start).Seconds()) }()

	deadline := time.NewTimer(s.cfg.DrainTime + shutdownGrace)
	defer deadline.Stop()

	for len(pending) > 0 {
		select {
		case exit := <-s.childExited:
			delete(pending, exit.epoch)
			s.reap(exit.epoch)
		case <-deadline.C:
			for e := range pending {
				s.log.Info("drain deadline elapsed, killing envoy epoch", "epoch", e)
				s.signalEpoch(e, syscall.SIGKILL)
				s.reap(e)
			}
			return
		}
	}
}
