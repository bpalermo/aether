// Package ownership decides which node agent owns its node (proposal 041).
//
// A node has exactly one agent serving its proxy (xds.sock) and its CNI plugin
// (cni.sock), and writing the node-local state files. A delete-then-create roll
// guarantees that by having no overlap at all, and pays for it with the whole
// pod replacement and startup inside the window the proxy has no ADS stream
// (#1123: 7.6-15.8 s on talos). A surge roll (maxSurge: 1) starts the new agent
// BESIDE the old one, so "who owns the node" needs an answer that is not the
// pod lifecycle:
//
//   - The owner holds an exclusive flock(2) on a lock file under /run/aether
//     (the hostPath both pods mount) for its whole life. The kernel releases it
//     the instant the process dies — SIGTERM, OOM kill, segfault alike — so
//     there is no lease to expire and no API round trip in the handoff.
//   - A successor that finds the lock taken is a STANDBY. It builds everything
//     a first serve needs (identity, registry watch, listeners from storage,
//     capture projection, client certificates) but binds no node socket and
//     writes no node file. Its goroutine blocks on the lock from process start.
//   - On acquiring the lock the standby runs its takeover steps (reconciling
//     what the old owner did during the overlap), then announces ownership:
//     the sockets bind and the held-back writers start.
//
// A lock that is free at start means there was no overlap — a plain restart or
// a delete-then-create roll — and the agent owns the node immediately, as it
// always has.
package ownership

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	commonlog "aethermesh.dev/common/log"
)

// DefaultPollInterval is how often a standby that holds the lock re-checks the
// node sockets for a live server it does not know about (see liveServer).
const DefaultPollInterval = 25 * time.Millisecond

// dialTimeout bounds one liveness dial of a node socket.
const dialTimeout = 100 * time.Millisecond

// Step is one piece of work a standby does between acquiring the node lock and
// announcing ownership. Steps run in registration order, only on a contended
// claim (a free lock at start means there is nothing another agent changed).
type Step struct {
	// Name is how the step is logged.
	Name string
	// Run does the work. An error is logged and the takeover continues: the old
	// owner is gone, and a node nobody serves is worse than a partial reconcile
	// (every step here also has a slower repair path of its own).
	Run func(ctx context.Context) error
}

// Node is this agent's claim on its node. The zero value is not usable; build
// it with New, call Claim at process start, and register it with the manager
// (it is a controller-runtime Runnable).
type Node struct {
	lockPath string
	sockets  []string
	log      *slog.Logger

	// PollInterval overrides DefaultPollInterval when positive (tests).
	PollInterval time.Duration

	mu    sync.Mutex
	steps []Step

	// file holds the lock for the life of the process. Kept on the struct so
	// the descriptor (and with it the flock) is never closed by a finalizer.
	file *os.File

	claimed   time.Time
	contended atomic.Bool
	locked    chan struct{}
	owned     chan struct{}
	ownedFlag atomic.Bool
}

// New returns a Node that locks lockPath and, before announcing ownership,
// waits until no live server answers on any of sockets (see liveServer). An
// empty lockPath disables the lock: the node is owned as soon as Start runs,
// which is the pre-041 behaviour.
func New(lockPath string, sockets []string, log *slog.Logger) *Node {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Node{
		lockPath: lockPath,
		sockets:  sockets,
		log:      commonlog.Named(log, "ownership"),
		locked:   make(chan struct{}),
		owned:    make(chan struct{}),
	}
}

// Claim tries the lock once without blocking. Call it at process start, before
// local storage is loaded: a claim that succeeds there proves no other agent
// owned the node while the storage was read, which is what lets an uncontended
// start skip the takeover reconcile.
//
// When the lock is taken, Claim marks the node contended and starts a
// goroutine that blocks on it; Start announces ownership once it is held. When
// the lock cannot be used at all (no lock file), Claim logs and proceeds as if
// it held it: the live-server check in Start still keeps this agent from
// binding over a serving one, and refusing to start would take the node's CNI
// and xDS down outright.
func (n *Node) Claim() {
	n.claimed = time.Now()
	if n.lockPath == "" {
		close(n.locked)
		return
	}
	f, err := openLockFile(n.lockPath)
	if err != nil {
		n.log.Error("cannot open the node lock; proceeding without it (the live-socket check still guards the handoff)",
			"path", n.lockPath, "error", err)
		n.contendedIfServing()
		close(n.locked)
		return
	}
	n.file = f
	switch err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		n.contendedIfServing()
		close(n.locked)
		return
	case errors.Is(err, syscall.EWOULDBLOCK):
		n.contended.Store(true)
		n.log.Info("another agent owns this node; starting as a standby: building everything, binding nothing until its lock is released",
			"lock", n.lockPath)
		go n.waitForLock()
	default:
		n.log.Error("cannot take the node lock; proceeding without it (the live-socket check still guards the handoff)",
			"path", n.lockPath, "error", err)
		n.contendedIfServing()
		close(n.locked)
	}
}

// contendedIfServing marks the node contended when one of its sockets has a
// live server although the lock was free: an agent that predates the lock
// (the first surge roll onto this version) still owns the node.
func (n *Node) contendedIfServing() {
	if path := n.liveServer(); path != "" {
		n.contended.Store(true)
		n.log.Info("an agent that holds no node lock is serving this node; starting as a standby until it stops",
			"socket", path)
	}
}

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// flock applies how to f, retrying the EINTR a blocking flock gets from the Go
// runtime's own preemption signals.
func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// waitForLock blocks until the kernel hands this process the lock. It cannot
// be cancelled, and need not be: a standby told to stop exits, which ends the
// goroutine with the process.
func (n *Node) waitForLock() {
	if err := flock(n.file, syscall.LOCK_EX); err != nil {
		n.log.Error("waiting for the node lock failed; proceeding without it (the live-socket check still guards the handoff)",
			"path", n.lockPath, "error", err)
	}
	close(n.locked)
}

// AddStep registers a takeover step. Call before the manager starts.
func (n *Node) AddStep(step Step) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.steps = append(n.steps, step)
}

// Contended reports whether this agent started while another one owned (or
// was serving) the node, i.e. whether it is, or was, a standby.
func (n *Node) Contended() bool { return n.contended.Load() }

// Owned returns a channel closed once this agent owns the node: it holds the
// lock, no other server answers on the node sockets, and every takeover step
// has run.
func (n *Node) Owned() <-chan struct{} { return n.owned }

// IsOwned reports whether Owned has closed.
func (n *Node) IsOwned() bool { return n.ownedFlag.Load() }

// WaitOwned blocks until this agent owns the node or ctx ends; it returns
// ctx's error in the latter case. It is the bind gate of every node socket.
func (n *Node) WaitOwned(ctx context.Context) error {
	select {
	case <-n.owned:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NeedLeaderElection reports false: every node has its own owner.
func (n *Node) NeedLeaderElection() bool { return false }

// Start completes the claim: it waits for the lock, then for the node sockets
// to have no live server, runs the takeover steps if the claim was contended,
// and announces ownership. It returns once the node is owned (or ctx ends).
func (n *Node) Start(ctx context.Context) error {
	if n.claimed.IsZero() {
		n.Claim()
	}
	select {
	case <-n.locked:
	case <-ctx.Done():
		return nil
	}
	contended := n.contended.Load()
	if contended {
		n.log.InfoContext(ctx, "node lock acquired: the previous owner is gone; taking the node over",
			"standby", time.Since(n.claimed).Round(time.Millisecond).String())
	}
	if !n.waitForNoLiveServer(ctx) {
		return nil
	}
	took := time.Now()
	if contended {
		n.runSteps(ctx)
		if ctx.Err() != nil {
			return nil
		}
	}
	n.ownedFlag.Store(true)
	close(n.owned)
	if contended {
		n.log.InfoContext(ctx, "this agent owns the node; binding its sockets and starting its writers",
			"takeover", time.Since(took).Round(time.Millisecond).String(),
			"standby", time.Since(n.claimed).Round(time.Millisecond).String())
	}
	return nil
}

func (n *Node) runSteps(ctx context.Context) {
	n.mu.Lock()
	steps := append([]Step(nil), n.steps...)
	n.mu.Unlock()
	for _, step := range steps {
		started := time.Now()
		err := step.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			n.log.ErrorContext(ctx, "takeover step failed; continuing (the node is ours either way)", "step", step.Name, "error", err)
			continue
		}
		n.log.InfoContext(ctx, "takeover step done", "step", step.Name, "took", time.Since(started).Round(time.Millisecond).String())
	}
}

// waitForNoLiveServer polls until no node socket has a live server, reporting
// false if ctx ended first. Holding the lock already proves no lock-aware
// agent is serving; this covers the one that predates the lock (the first
// surge roll onto this version) and anything else that bound a node socket
// outside the protocol. Unlinking a live server's socket would strand the
// proxy and the CNI plugin on a dead path the moment that server exits.
func (n *Node) waitForNoLiveServer(ctx context.Context) bool {
	poll := n.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	var logged bool
	for {
		path := n.liveServer()
		if path == "" {
			return true
		}
		if !logged {
			n.log.InfoContext(ctx, "a server still answers on a node socket; waiting for it to stop before binding", "socket", path)
			logged = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
}

// liveServer returns the first node socket a server accepts a connection on,
// or "" when none does. A missing path (ENOENT) or a stale file nobody listens
// on (ECONNREFUSED, what a killed agent leaves) is not a server.
func (n *Node) liveServer() string {
	for _, path := range n.sockets {
		conn, err := net.DialTimeout("unix", path, dialTimeout)
		if err != nil {
			continue
		}
		_ = conn.Close()
		return path
	}
	return ""
}

// StandbyChecker returns the readiness check that gives a surge roll its
// meaning (proposal 041, "Readiness meaning"): the DaemonSet controller deletes
// the old agent once the new one is Ready, so a standby must report Ready only
// when it could serve the node the moment it gets it — its first snapshot built
// and every first-serve gate passed, which is what complete being closed means.
// Once this agent owns the node the check always passes and the agent's other
// checks carry the verdict, exactly as before.
func (n *Node) StandbyChecker(complete <-chan struct{}) func(*http.Request) error {
	return func(*http.Request) error {
		if n.IsOwned() {
			return nil
		}
		select {
		case <-complete:
			return nil
		default:
			return fmt.Errorf("standby since %s: first snapshot not built yet (identity, registry, capture projection, client certificates)",
				time.Since(n.claimed).Round(time.Second))
		}
	}
}
