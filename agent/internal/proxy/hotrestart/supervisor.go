// Package hotrestart implements a supervisor that manages the aether-proxy Envoy
// process and performs Envoy hot restarts across restart epochs, replicating the
// behavior of Envoy's hot-restarter.py in Go.
//
// SPIKE: this package is a throwaway-quality starting point for the proxy
// hot-restart spike (see docs/proposals/001_proxy-hot-restart.md). It is not yet
// wired into production and the edge cases marked TODO are deliberately unhandled.
//
// Model: the supervisor is the proxy container's entrypoint (PID 1). It forks an
// Envoy child with --restart-epoch 0 and a fixed --base-id. On a hot-restart
// trigger (SIGHUP, or a watched bootstrap-config change) it forks a new Envoy with
// the next epoch; Envoy's own shared-memory + abstract-domain-socket IPC transfers
// the listen-socket FDs and stats to the new process, the old process drains, and
// after --parent-shutdown-time-s the supervisor terminates it. The supervisor and
// both epochs share the container's /dev/shm and PID/IPC namespace, which is what
// makes the FD/stats handoff possible.
package hotrestart

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
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
	// WatchConfig enables an fsnotify watch on ConfigPath that self-triggers a hot
	// restart when the bootstrap config changes.
	//
	// TODO(spike): wire the fsnotify watcher; for now restarts are driven by SIGHUP.
	WatchConfig bool
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
}

// New creates a Supervisor.
func New(cfg Config, log logr.Logger) *Supervisor {
	return &Supervisor{
		cfg:         cfg,
		log:         log.WithName("proxy-supervisor"),
		children:    make(map[int]*exec.Cmd),
		childExited: make(chan childExit, 4),
		done:        make(chan struct{}),
	}
}

// Run starts Envoy at epoch 0 and supervises it until ctx is canceled (SIGTERM/
// SIGINT, which controller-runtime's signal handler maps to ctx.Done) or the
// newest epoch exits unexpectedly. SIGHUP triggers a hot restart; SIGUSR1 is
// forwarded to the current child for log reopen.
func (s *Supervisor) Run(ctx context.Context) error {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(sigCh)
	defer close(s.done)

	if err := s.hotRestart(); err != nil {
		return fmt.Errorf("starting initial envoy epoch: %w", err)
	}

	// TODO(spike): if cfg.WatchConfig, start an fsnotify watcher on ConfigPath and
	// fan its events into a hot restart (debounced), in addition to SIGHUP.

	for {
		select {
		case <-ctx.Done():
			s.log.Info("termination requested, shutting down all envoy epochs")
			s.terminateAll()
			return nil

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				s.log.Info("SIGHUP received, performing hot restart")
				if err := s.hotRestart(); err != nil {
					s.log.Error(err, "hot restart failed; keeping current epoch")
				}
			case syscall.SIGUSR1:
				s.forwardToCurrent(syscall.SIGUSR1)
			}

		case exit := <-s.childExited:
			if exit.epoch == s.currentEpoch() {
				// The newest epoch died unexpectedly: nothing left serving traffic.
				// Bail non-zero so Kubernetes recreates the pod (SIGCHLD-fatal).
				s.terminateAll()
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

	go func() {
		err := cmd.Wait()
		select {
		case s.childExited <- childExit{epoch: epoch, err: err}:
		case <-s.done:
		}
	}()

	if epoch > 0 {
		go s.scheduleParentShutdown(epoch - 1)
	}
	return nil
}

// scheduleParentShutdown SIGTERMs the given (draining) epoch after
// ParentShutdownTime, unless the supervisor is shutting down first.
func (s *Supervisor) scheduleParentShutdown(epoch int) {
	t := time.NewTimer(s.cfg.ParentShutdownTime)
	defer t.Stop()
	select {
	case <-t.C:
		s.log.Info("parent shutdown timer elapsed, terminating drained epoch", "epoch", epoch)
		s.signalEpoch(epoch, syscall.SIGTERM)
	case <-s.done:
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
		s.log.Error(err, "failed to signal envoy epoch", "epoch", epoch, "signal", sig)
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
	s.log.Info("reaped drained envoy epoch", "epoch", epoch)
}

// terminateAll SIGTERMs every tracked epoch. TODO(spike): wait for graceful exit
// with a deadline, then SIGKILL stragglers.
func (s *Supervisor) terminateAll() {
	s.mu.Lock()
	epochs := make([]int, 0, len(s.children))
	for e := range s.children {
		epochs = append(epochs, e)
	}
	s.mu.Unlock()
	for _, e := range epochs {
		s.signalEpoch(e, syscall.SIGTERM)
	}
}
