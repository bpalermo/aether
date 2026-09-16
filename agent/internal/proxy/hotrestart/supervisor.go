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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	commonlog "aethermesh.dev/common/log"
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
	// supervisor SIGTERMs the previous epoch. Must exceed DrainTime. It also sets
	// the admin re-verify budget: the epoch-identity probe re-confirms on a fresh
	// connection every ParentShutdownTime/adminReverifyDivisor, so a cross-pod
	// takeover is diagnosed while the draining parent still lives (see
	// adminprobe.go).
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
	// The supervisor probes it two ways: /ready on a pooled connection for the
	// per-second liveness watchdog, and /server_info on a fresh connection
	// whenever the answer must identify the epoch (see adminprobe.go).
	AdminAddress string
	// HandoffDeadline overrides defaultHandoffDeadline (0 = default). Must be
	// comfortably larger than ParentShutdownTime plus worst-case xDS-gated init.
	HandoffDeadline time.Duration
	// AdminUnresponsiveDeadline overrides defaultAdminUnresponsiveDeadline
	// (0 = default).
	AdminUnresponsiveDeadline time.Duration
	// TerminationGrace is this pod's terminationGracePeriodSeconds, i.e. how
	// long after SIGTERM the kubelet SIGKILLs the container. The supervisor
	// cannot read it from the API, so the chart passes its own value. It bounds
	// the mid-handoff wait for a successor so a termination with no possible
	// successor still drains Envoy (see successorWaitBudget, issue #771).
	// 0 = unknown: wait indefinitely, the pre-#771 behavior.
	TerminationGrace time.Duration
	// ShutdownDrainImmediately skips the bounded wait for a surge successor on
	// SIGTERM and drains Envoy's listeners straight away (issue #795). It is an
	// escape hatch for a deployment where no replacement can overlap this pod —
	// a DaemonSet without maxSurge, a single-instance proxy — where the wait can
	// only ever burn the grace period before falling back to the same drain.
	//
	// Leave it false wherever a surge replacement exists: the wait is what makes
	// pod delete, node drain, eviction and preemption hitless, because the
	// replacement's Envoy hot-restarts ours instead of finding the node empty.
	ShutdownDrainImmediately bool
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

	// adminAuthoritative / adminFast are the two Envoy-admin probe clients; the
	// difference between them is a correctness invariant, see newAdminClients.
	adminAuthoritative *http.Client
	adminFast          *http.Client

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

// readyGateTime guards readyGate for concurrent access between Run
// (bind-collision retries) and watchLiveness. It is only ever written by
// initStartEpoch, under the same lock acquisition that publishes nextEpoch.
func (s *Supervisor) readyGateTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyGate
}

// readyGateBuffer is added to ParentShutdownTime when gating a cross-pod
// successor's readiness, to ensure the predecessor is fully gone first.
const readyGateBuffer = 3 * time.Second

// successorReadyGate is the gate value for a supervisor that has just selected a
// cross-pod successor epoch: the predecessor must be terminated by this Envoy's
// own parent-shutdown protocol before the pod may report Ready.
//
// It is applied by initStartEpoch INSIDE the critical section that publishes
// nextEpoch, never afterwards. Publishing the epoch first and the gate second
// left a window in which a watchLiveness tick could read the successor epoch E
// while the gate was still the previous (long-expired) one, see the
// PREDECESSOR's Envoy answering LIVE at E, and mark the pod Ready before this
// pod's Envoy had even been forked — #132's hazard re-entering through the
// bind-collision retry path, where retries can run 4.5 minutes against a 15s
// parent-shutdown-time so the old gate is certainly stale.
func (s *Supervisor) successorReadyGate() time.Time {
	return time.Now().Add(s.cfg.ParentShutdownTime + readyGateBuffer)
}

// New creates a Supervisor. metrics may be nil to disable instrumentation.
func New(cfg Config, log *slog.Logger, metrics *SupervisorMetrics) *Supervisor {
	authoritative, fast := newAdminClients()
	return &Supervisor{
		cfg:                cfg,
		log:                commonlog.Named(log, "proxy-supervisor"),
		metrics:            metrics,
		adminAuthoritative: authoritative,
		adminFast:          fast,
		children:           make(map[int]*exec.Cmd),
		childExited:        make(chan childExit, 8),
		done:               make(chan struct{}),
		watchdogFired:      make(chan error, 1),
	}
}

// Run starts Envoy at epoch 0 and supervises it until ctx is canceled (SIGTERM/
// SIGINT, which controller-runtime's signal handler maps to ctx.Done) or the
// newest epoch exits unexpectedly. A watched-config change or SIGHUP triggers a
// hot restart; SIGUSR1 is forwarded to the current child for log reopen.
func (s *Supervisor) Run(ctx context.Context) error {
	defer close(s.done)
	defer s.adminFast.CloseIdleConnections()

	s.logAdminReverifyBudget(ctx)

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

// shutdownProbeTimeout bounds the epoch-identity probe that selects the
// shutdown branch. The probe runs on a context DETACHED from the caller's (see
// handleShutdown), so it cannot inherit a deadline and needs its own:
// adminServerInfo applies a readyPollInterval timeout inside it, and this is
// the outer ceiling covering the dial and one retry-free round trip.
const shutdownProbeTimeout = 2 * readyPollInterval

// handleShutdown implements the ctx.Done case of the Run select. It resolves a
// SIGTERM onto exactly one of four branches, off ONE authoritative
// epoch-identity probe:
//
//	admin LIVE at a NEWER epoch  A successor already holds our listen sockets and
//	                             is still initializing. Do NOT signal Envoy — the
//	                             successor needs its hot-restart parent alive, and
//	                             killing it aborts the successor with errno 111.
//	                             Wait for its parent-shutdown protocol.  -> handoff
//	admin LIVE at OUR epoch      No handoff has begun. This is NOT "nobody is
//	                             coming": the DaemonSet creates the surge
//	                             replacement within ~1s of the deletionTimestamp
//	                             and its Envoy hot-restarts ours. Keep serving and
//	                             wait for it, bounded the same way. -> successor_wait
//	admin unreachable            The child is dead or its main thread is wedged:
//	                             nothing to drain, nobody to hand to. -> child_dead
//	the wait expires, or
//	--shutdown-drain-immediately Graceful drain: POST /drain_listeners?graceful,
//	                             wait --drain-time, then SIGTERM and reap.
//	                                                                -> drain_fallback
//
// The successor_wait arm is issue #795. Before #785 it existed by accident — the
// broken probe took the wait branch on every SIGTERM — and that accident is why
// `kubectl delete pod` of an aether-proxy had always been hitless (21-23s
// termination, Envoy drained, zero prober errors, measured on rev213). #785 fixed
// the probe, which correctly routed the no-handoff-yet case to an immediate
// s.shutdown(); s.shutdown() is a bare SIGTERM, Envoy does not drain on SIGTERM,
// and the supervisor then WON the race against its own replacement: 1.5-2.3s
// termination, 7.95s/8.11s with no Envoy on the node, 130/126 prober
// connection_errors per delete, and AetherProberLivenessErrors paging (rev214 F2).
// `kubectl delete pod` is also the code path of node drain, eviction and
// preemption, so the policy here is the pre-#785 behaviour made deliberate and
// bounded, with a real graceful drain — never a bare SIGTERM on a serving Envoy —
// as the fallback for the terminations where no successor can ever arrive.
func (s *Supervisor) handleShutdown(ctx context.Context) error {
	// The probe MUST run on a context detached from ctx. handleShutdown has
	// exactly one call site — `case <-lp.ctx.Done()` in restartLoop.run — so
	// ctx is ALREADY cancelled by the time we get here. context.WithTimeout on
	// an already-cancelled parent yields an already-cancelled child, and
	// http.Client.Do then fails with "context canceled" in microseconds
	// WITHOUT OPENING A SOCKET: the branch below read "not live at our epoch"
	// on every SIGTERM regardless of Envoy's actual state, so the
	// wait-for-successor path was taken unconditionally — including when no
	// successor existed and Envoy was never drained (issue #771; measured
	// on-cluster as exactly one server_info/unreachable per pod lifetime, at
	// its SIGTERM, while server_info/live was still incrementing).
	// Same trap, same idiom as common/telemetry/setup/lifecycle.go's
	// DetachedTimeout (issue #662): keep the values, drop the cancellation.
	probeCtx, cancelProbe := context.WithTimeout(context.WithoutCancel(ctx), shutdownProbeTimeout)
	defer cancelProbe()

	epoch := s.currentEpoch()
	// live: the admin answered LIVE and named OUR restart epoch.
	// reachable: the admin answered at all (see adminServerInfo). With no admin
	// address configured — unit-test supervisors only, never the chart — both
	// stay false and the mid-handoff arm is taken, exactly as before.
	var live, reachable bool
	if s.cfg.AdminAddress != "" {
		live, reachable = s.adminServerInfo(probeCtx, epoch)
	}

	switch {
	// StateDir is always set in production (the supervisor only runs in the
	// cross-pod configuration), and --shutdown-drain-immediately is the
	// operator's declaration that this deployment has no surge replacement.
	// Either way no successor can arrive, so there is nothing to wait for.
	case s.cfg.StateDir == "" || s.cfg.ShutdownDrainImmediately:
		s.log.InfoContext(ctx, "termination requested; draining listeners immediately (no successor expected)",
			"epoch", epoch, "drainImmediately", s.cfg.ShutdownDrainImmediately,
			"crossPodCoordination", s.cfg.StateDir != "")
		s.drainThenShutdown(ctx)

	// The admin did not answer at all: our Envoy is dead or wedged. A drain
	// request would go nowhere and there is no traffic left to protect.
	case s.cfg.AdminAddress != "" && !reachable:
		s.log.InfoContext(ctx, "termination requested, shutting down all envoy epochs",
			"epoch", epoch, "adminReachable", false)
		s.metrics.shutdownBranchTaken(shutdownBranchChildDead)
		s.shutdown()

	// Reachable but not LIVE at our epoch: a successor has taken the shared
	// admin port. Genuinely mid-handoff.
	case !live:
		s.awaitSuccessor(ctx, true)

	// LIVE at our own epoch: still the serving Envoy, no handoff yet.
	default:
		s.awaitSuccessor(ctx, false)
	}
	return nil
}

// awaitSuccessor keeps the child serving — never signalling it — while a
// successor takes over, bounded by successorWaitBudget. midHandoff says which
// state we entered from, which changes only the wording and the branch
// attribute: the wait, its bound and its fallback are identical, because in
// both cases the only safe way to end a live Envoy is a successor taking its
// sockets, and the only safe fallback is draining them ourselves.
func (s *Supervisor) awaitSuccessor(ctx context.Context, midHandoff bool) {
	budget := s.successorWaitBudget()
	epoch := s.currentEpoch()
	if midHandoff {
		s.log.InfoContext(ctx, "termination requested mid-handoff; waiting for successor to terminate our envoy",
			"epoch", epoch, "successorWaitBudget", budget)
	} else {
		s.log.InfoContext(ctx, "waiting for a successor before draining",
			"epoch", epoch, "successorWaitBudget", budget, "drainTime", s.cfg.DrainTime)
	}

	if s.awaitProtocolTermination(budget) {
		s.log.InfoContext(ctx, "successor took over during shutdown wait",
			"epoch", epoch, "midHandoff", midHandoff)
		if midHandoff {
			s.metrics.shutdownBranchTaken(shutdownBranchHandoff)
		} else {
			s.metrics.shutdownBranchTaken(shutdownBranchSuccessorWait)
		}
		return
	}

	s.logSuccessorWaitFallback(ctx, budget)
	s.log.InfoContext(ctx, "no successor within budget; draining listeners",
		"epoch", epoch, "successorWaitBudget", budget, "drainTime", s.cfg.DrainTime)
	s.drainThenShutdown(ctx)
}

// drainThenShutdown is the only safe way to end a still-serving Envoy with no
// successor to hand it to: ask it to drain its listeners gracefully over
// --drain-time-s, wait that out, and only then SIGTERM and reap.
//
// It is deliberately not s.shutdown(). SIGTERM is not a drain — Envoy's handler
// exits the server outright (0.33-0.74s, measured) — which is what turned every
// pod delete, node drain, eviction and preemption into an ~8s node-wide
// connection blackout once #785 stopped the accidental successor wait (#795).
//
// Envoy may well exit on its own at the end of its drain sequence; the wait
// reaps it if so, and the trailing shutdown() then has nothing left to signal.
func (s *Supervisor) drainThenShutdown(ctx context.Context) {
	start := time.Now()
	defer s.metrics.shutdownBranchTaken(shutdownBranchDrainFallback)

	s.mu.Lock()
	hadChildren := len(s.children) > 0
	s.mu.Unlock()

	// With no admin endpoint there is nothing to ask, and with no drain window
	// nothing to wait out: degenerate to the plain shutdown, which is what
	// coordination-less unit-test supervisors have always got.
	if s.cfg.AdminAddress != "" && s.cfg.DrainTime > 0 {
		drained := s.drainListeners(ctx)
		s.awaitDrain(s.cfg.DrainTime)
		s.log.InfoContext(ctx, "listeners drained; stopping envoy",
			"epoch", s.currentEpoch(), "drainAccepted", drained, "elapsed", time.Since(start))
	}

	s.terminateChildren()
	// Measured over the WHOLE fallback, graceful drain window included — and
	// recorded even when Envoy exited during its own drain sequence, which is
	// the outcome terminateChildren then has nothing left to report.
	if hadChildren {
		s.metrics.drainCompleted(time.Since(start).Seconds())
	}
}

// awaitDrain waits up to d for the children to exit on their own, reaping any
// that do. Envoy's graceful drain sequence ends by shutting the server down, so
// the common outcome is that this returns early with nothing left to SIGTERM.
func (s *Supervisor) awaitDrain(d time.Duration) {
	s.mu.Lock()
	pending := len(s.children)
	s.mu.Unlock()
	if pending == 0 {
		return
	}

	t := time.NewTimer(d)
	defer t.Stop()
	for pending > 0 {
		select {
		case exit := <-s.childExited:
			s.reap(exit.epoch)
			pending--
		case <-t.C:
			return
		}
	}
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
	if err := s.hotRestart(); err != nil {
		return true, fmt.Errorf("relaunching envoy after retry: %w", err), true
	}
	return true, nil, false
}

// reserveEpoch picks the restart epoch for the next launch: nextEpoch, advanced
// past any epoch whose child is STILL TRACKED.
//
// children is keyed by epoch, so reusing a key silently replaces the *exec.Cmd
// of a child that is still alive. That is reachable: resetEpochForRetry rewinds
// nextEpoch to 0 on every bind-collision retry, and if the heartbeat is stale
// initStartEpoch leaves it there — so an epoch-0 Envoy that is still draining
// gets its entry overwritten by the relaunch. The draining process is then
// orphaned (signalEpoch resolves epoch 0 to the new cmd, so shutdown() never
// signals it), awaitProtocolTermination's pending count undercounts it, and its
// eventual exit can take the container down non-zero while the new Envoy is
// healthy. A tracked child at epoch E also means E's base-id domain socket is
// still held, so launching E again would bind-collide anyway: E+1 is both the
// safe key and the correct hot-restart epoch to attach at.
func (s *Supervisor) reserveEpoch() int {
	s.mu.Lock()
	requested := s.nextEpoch
	epoch := requested
	for {
		if _, tracked := s.children[epoch]; !tracked {
			break
		}
		epoch++
	}
	s.mu.Unlock()

	if epoch != requested {
		s.log.Warn("restart epoch still has a tracked envoy; attaching above it instead of reusing the key",
			"requestedEpoch", requested, "epoch", epoch)
	}
	return epoch
}

// hotRestart forks a new Envoy child at the next restart epoch and schedules
// shutdown of the previous one after ParentShutdownTime.
func (s *Supervisor) hotRestart() error {
	epoch := s.reserveEpoch()

	cmd := s.buildEnvoyCmd(epoch)
	s.log.Info("starting envoy", "epoch", epoch, "args", cmd.Args)
	if err := cmd.Start(); err != nil {
		// nextEpoch is deliberately NOT advanced on a fork failure. A Start
		// error from handleDebounce is non-fatal ("keeping current epoch"), and
		// advancing here would leave currentEpoch() naming an epoch with no
		// child: both wedge watchdogs and the readiness hold are gated on
		// childTracked(epoch), so none of them could fire and the ready marker
		// would stay cleared forever while the old Envoy kept serving — exactly
		// the failure the watchdogs exist to prevent.
		return err
	}

	s.mu.Lock()
	s.children[epoch] = cmd
	s.nextEpoch = epoch + 1
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

// anyChildTracked reports whether ANY supervised Envoy is still tracked,
// regardless of epoch. It is the readiness hold's question (see onNotLiveEpoch):
// "is one of our Envoy processes still the thing serving this node?"
//
// Asking it per-epoch instead was a readiness flap. Between resetEpochForRetry
// and the relaunch — which includes initStartEpoch's up-to-8s heartbeat/admin
// re-probe loop — currentEpoch() is the rewound nextEpoch-1, i.e. -1, an epoch
// that never had a child. A supervisor that was Ready (epoch 0 LIVE) and then
// lost a just-forked epoch 1 to a bind collision therefore cleared its ready
// marker for the whole retry, and recorded a ready_transitions{ready=false},
// while its epoch-0 Envoy was still tracked and still serving every request on
// the node. That is a candidate mechanism for the unexplained proxy
// readiness-marker flap on w01/w05 in the 2026-09-03 soak.
func (s *Supervisor) anyChildTracked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.children) > 0
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

// terminationFallbackMargin is the head-room left between the end of the
// successor wait and the kubelet's SIGKILL, on top of the drain the fallback
// itself needs. It absorbs the scheduling jitter of a node under termination
// load; it is not a tuning knob.
const terminationFallbackMargin = 10 * time.Second

// successorWaitBudget returns how long the mid-handoff path may wait for a
// successor to terminate our Envoy before draining it ourselves, or 0 for
// "wait indefinitely".
//
// The wait must NOT be bounded by any "reasonable"-looking timer of its own:
// the successor's parent-shutdown timer starts only after its (xDS-gated,
// unbounded) init completes, and it keeps using the parent socket for stat
// merges right up to protocol-terminate, so cutting a healthy handoff short
// aborts the successor with errno 111 — the 2026-06-11 node data-plane gap.
// The only bound that is not arbitrary is this pod's own death sentence: the
// kubelet SIGKILLs us at terminationGracePeriodSeconds no matter what. Waiting
// right up to it is what issue #771 showed to be wrong for the cases where no
// successor can ever appear (node shutdown, scale-down, DaemonSet delete, a
// replacement stuck Pending): Envoy is never drained and dies with connections
// open.
//
// So: wait as long as the pod has, minus what draining will then cost
// (DrainTime + shutdownGrace, exactly what shutdown() budgets) minus a margin.
// A grace period too small to fit a drain leaves no safe cutoff at all, and
// falls back to waiting indefinitely rather than guaranteeing an errno-111
// abort — as does an unset TerminationGrace, which is how every pre-#771 chart
// and every unit test invokes the supervisor.
func (s *Supervisor) successorWaitBudget() time.Duration {
	if s.cfg.TerminationGrace <= 0 {
		return 0
	}
	budget := s.cfg.TerminationGrace - (s.cfg.DrainTime + shutdownGrace) - terminationFallbackMargin
	if budget <= 0 {
		return 0
	}
	return budget
}

// logSuccessorWaitFallback records, at WARN, that the successor wait expired,
// together with the epoch state observed at that moment. The probe runs on a
// context detached from the (already cancelled) caller's — see handleShutdown.
func (s *Supervisor) logSuccessorWaitFallback(ctx context.Context, budget time.Duration) {
	epoch := s.currentEpoch()
	var live, reachable bool
	if s.cfg.AdminAddress != "" {
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownProbeTimeout)
		defer cancel()
		live, reachable = s.adminServerInfo(probeCtx, epoch)
	}
	s.log.WarnContext(ctx,
		"no successor terminated our envoy within the termination-grace budget; draining it ourselves before the kubelet's SIGKILL",
		"epoch", epoch,
		"successorWaitBudget", budget,
		"adminLiveAtOurEpoch", live,
		"adminReachable", reachable,
		"childTracked", s.childTracked(epoch),
		"terminationGrace", s.cfg.TerminationGrace,
		"drainTime", s.cfg.DrainTime,
	)
}

// awaitProtocolTermination waits (without signaling) for the remaining children
// to exit via the successor's hot-restart parent-shutdown protocol, bounded by
// budget (0 = no bound; see successorWaitBudget for why the bound must be the
// pod's grace period and nothing shorter). It reports whether every child
// terminated within the budget; on false the caller must drain them itself.
func (s *Supervisor) awaitProtocolTermination(budget time.Duration) bool {
	s.mu.Lock()
	pending := len(s.children)
	s.mu.Unlock()

	// A nil channel blocks forever, which is exactly the unbounded behavior.
	var expired <-chan time.Time
	if budget > 0 {
		t := time.NewTimer(budget)
		defer t.Stop()
		expired = t.C
	}

	for pending > 0 {
		select {
		case exit := <-s.childExited:
			s.reap(exit.epoch)
			pending--
			s.log.Info("envoy epoch terminated by successor", "epoch", exit.epoch)
		case <-expired:
			return false
		}
	}
	return true
}

// shutdown SIGTERMs every tracked epoch and waits up to DrainTime+grace for them
// to exit, SIGKILLing any straggler.
//
// NOTE: SIGTERM is not a drain. Envoy exits on it immediately, so this is only
// correct for an Envoy that is no longer serving — one a successor has taken
// over from, one that is already dead, or one drainThenShutdown has just
// drained. Never call it on a live, serving Envoy (issue #795).
func (s *Supervisor) shutdown() {
	start := time.Now()
	if s.terminateChildren() {
		s.metrics.drainCompleted(time.Since(start).Seconds())
	}
}

// terminateChildren SIGTERMs every tracked epoch and waits up to
// DrainTime+grace for them to exit, SIGKILLing any straggler. It reads
// childExited directly because the main loop has stopped selecting on it.
// It reports whether there was anything left to terminate, which is what
// decides whether a drain duration is worth recording.
func (s *Supervisor) terminateChildren() bool {
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
		return false
	}

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
			return true
		}
	}
	return true
}
