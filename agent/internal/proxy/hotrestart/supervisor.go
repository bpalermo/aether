// Package hotrestart implements a supervisor that manages the aether-proxy Envoy
// process and performs Envoy hot restarts across restart epochs, replicating the
// behavior of Envoy's hot-restarter.py in Go.
//
// SPIKE: this package is the Strategy-A implementation for the proxy hot-restart
// spike (see docs/proposals/001_proxy-hot-restart.md). It supervises a single
// long-lived Envoy in one container and performs an in-place hot restart when the
// bootstrap config changes (fsnotify) or on SIGHUP. The cross-pod machinery for
// Strategy B (epoch coordination file, live-predecessor probe) is intentionally
// not here yet.
//
// Model: the supervisor is the proxy container's entrypoint (PID 1). It forks an
// Envoy child with --restart-epoch 0 and a fixed --base-id. On a hot-restart
// trigger it forks a new Envoy with the next epoch; Envoy's own shared-memory +
// abstract-domain-socket IPC transfers the listen-socket FDs and stats to the new
// process, the old process drains, and after --parent-shutdown-time-s the
// supervisor terminates it. The supervisor and both epochs share the container's
// /dev/shm and PID/IPC namespace, which is what makes the FD/stats handoff possible.
package hotrestart

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
)

const (
	// debounceDelay coalesces a flurry of triggers (config rewrites, repeated
	// SIGHUPs) into a single hot restart.
	debounceDelay = 500 * time.Millisecond
	// shutdownGrace is added to DrainTime as the deadline for children to exit on
	// SIGTERM before they are SIGKILLed.
	shutdownGrace = 5 * time.Second
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
	// StateDir, when set, enables Strategy B cross-pod coordination: a per-node
	// epoch heartbeat file on a shared hostPath. A surging successor pod reads it
	// to start at (live predecessor epoch + 1) and hot-restart across the pod
	// boundary. Empty = Strategy A only (always start at epoch 0).
	StateDir string
	// ReadyMarkerPath, when set, enables the readiness gate: the supervisor keeps a
	// pod-local marker present only while the node's Envoy admin reports LIVE at
	// this supervisor's newest epoch. An exec readiness probe checks the marker so
	// the DaemonSet keeps the old pod until the new one has taken over.
	ReadyMarkerPath string
	// AdminAddress is the Envoy admin host:port used for the readiness check.
	AdminAddress string
}

// childExit reports the termination of a supervised Envoy epoch.
type childExit struct {
	epoch int
	err   error
}

// Supervisor owns the Envoy process lifecycle and performs hot restarts.
type Supervisor struct {
	cfg Config
	log logr.Logger

	mu        sync.Mutex
	children  map[int]*exec.Cmd // keyed by restart epoch
	nextEpoch int

	childExited chan childExit
	done        chan struct{}

	// readyGate delays the pod's readiness until after a cross-pod handoff is fully
	// complete: the successor Envoy terminates the predecessor itself via
	// --parent-shutdown-time-s, so the pod must not report Ready (which lets the
	// DaemonSet delete the old pod) until that has elapsed — otherwise the old
	// Envoy is killed out from under the still-attached successor (errno 111).
	readyGate time.Time
}

// readyGateBuffer is added to ParentShutdownTime when gating a cross-pod
// successor's readiness, to ensure the predecessor is fully gone first.
const readyGateBuffer = 3 * time.Second

// New creates a Supervisor.
func New(cfg Config, log logr.Logger) *Supervisor {
	return &Supervisor{
		cfg:         cfg,
		log:         log.WithName("proxy-supervisor"),
		children:    make(map[int]*exec.Cmd),
		childExited: make(chan childExit, 8),
		done:        make(chan struct{}),
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

	// Strategy B: pick the start epoch from a confirmed-live predecessor (if any),
	// then maintain the readiness marker and the LIVE-gated node epoch heartbeat.
	// No-ops when StateDir / ReadyMarkerPath are unset (Strategy A).
	s.initStartEpoch(ctx)
	if s.cfg.StateDir != "" && s.nextEpoch > 0 {
		// Cross-pod successor: hold readiness until the predecessor has been
		// terminated by this Envoy's own parent-shutdown protocol.
		s.readyGate = time.Now().Add(s.cfg.ParentShutdownTime + readyGateBuffer)
	}
	go s.watchLiveness(ctx)

	if err := s.hotRestart(); err != nil {
		return fmt.Errorf("starting initial envoy epoch: %w", err)
	}

	debounce := time.NewTimer(debounceDelay)
	debounce.Stop()
	var debounceC <-chan time.Time

	arm := func(reason string) {
		s.log.V(1).Info("hot restart armed", "reason", reason, "debounce", debounceDelay)
		debounce.Reset(debounceDelay)
		debounceC = debounce.C
	}

	for {
		select {
		case <-ctx.Done():
			if s.supersededBySuccessor(s.currentEpoch()) {
				// A successor pod is mid-handoff with our Envoy as its hot-restart
				// parent. The DaemonSet may delete this pod the moment it turns
				// NotReady (it no longer counts against maxUnavailable), so this
				// SIGTERM arrives while the successor still needs the parent alive.
				// Do NOT signal Envoy: wait for the successor's parent-shutdown
				// protocol to terminate it, then exit. Falls back to a normal
				// shutdown if that doesn't happen in time.
				s.log.Info("termination requested mid-handoff; waiting for successor to terminate our envoy")
				s.awaitProtocolTermination()
				return nil
			}
			s.log.Info("termination requested, shutting down all envoy epochs")
			s.shutdown()
			return nil

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				arm("SIGHUP")
			case syscall.SIGUSR1:
				s.forwardToCurrent(syscall.SIGUSR1)
			}

		case <-trigger:
			arm("config change")

		case <-debounceC:
			debounceC = nil
			s.log.Info("performing hot restart")
			if err := s.hotRestart(); err != nil {
				s.log.Error(err, "hot restart failed; keeping current epoch")
			}

		case exit := <-s.childExited:
			if exit.epoch == s.currentEpoch() {
				if s.supersededBySuccessor(exit.epoch) {
					// A successor pod took over this node at a higher epoch and
					// terminated our Envoy via the hot-restart parent-shutdown. Don't
					// exit (restartPolicy=Always would relaunch us and climb epochs):
					// stay alive with no child until the DaemonSet deletes this pod.
					s.log.Info("newest epoch superseded by a successor pod; awaiting pod deletion", "epoch", exit.epoch)
					s.reap(exit.epoch)
					<-ctx.Done()
					return nil
				}
				// The newest epoch died unexpectedly: nothing left serving traffic.
				// Bail non-zero so Kubernetes recreates the pod (SIGCHLD-fatal).
				s.reap(exit.epoch)
				s.shutdown()
				return fmt.Errorf("envoy epoch %d exited unexpectedly: %w", exit.epoch, exit.err)
			}
			// An older epoch finished draining after a hot restart: expected. Reap it.
			s.reap(exit.epoch)
		}
	}
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
	s.mu.Unlock()

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
		s.log.Error(err, "config watcher disabled")
		return
	}
	defer func() { _ = w.Close() }()

	dir := filepath.Dir(s.cfg.ConfigPath)
	if err := w.Add(dir); err != nil {
		s.log.Error(err, "failed to watch config dir; watcher disabled", "dir", dir)
		return
	}
	s.log.Info("watching bootstrap config for changes", "dir", dir, "config", s.cfg.ConfigPath)

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
			s.log.Error(err, "config watch error")
		}
	}
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

// signalEpoch sends sig to the child for the given epoch, if still tracked.
func (s *Supervisor) signalEpoch(epoch int, sig syscall.Signal) {
	s.mu.Lock()
	cmd, ok := s.children[epoch]
	s.mu.Unlock()
	if !ok || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(sig); err != nil {
		s.log.V(1).Error(err, "failed to signal envoy epoch", "epoch", epoch, "signal", sig)
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
// exit via the successor's hot-restart parent-shutdown protocol, up to
// ParentShutdownTime plus grace; any straggler past the deadline gets a normal
// shutdown. Requires the pod's terminationGracePeriod to exceed that deadline.
func (s *Supervisor) awaitProtocolTermination() {
	s.mu.Lock()
	pending := len(s.children)
	s.mu.Unlock()
	if pending == 0 {
		return
	}

	deadline := time.NewTimer(s.cfg.ParentShutdownTime + shutdownGrace)
	defer deadline.Stop()

	for pending > 0 {
		select {
		case exit := <-s.childExited:
			s.reap(exit.epoch)
			pending--
			s.log.Info("envoy epoch terminated by successor", "epoch", exit.epoch)
		case <-deadline.C:
			s.log.Info("successor did not terminate our envoy in time; shutting down")
			s.shutdown()
			return
		}
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
