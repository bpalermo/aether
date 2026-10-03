package ownership

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortDir returns a directory short enough for a Unix socket path (Bazel's
// TMPDIR is too long for the 108-byte sun_path).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "own")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// release drops the lock as a process exit would: flock locks belong to the
// open file description, so closing it is exactly what the kernel does when the
// owner dies.
func (n *Node) release() {
	if n.file != nil {
		_ = n.file.Close()
	}
}

func startAsync(t *testing.T, n *Node) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, n.Start(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel, done
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestFreeLockOwnsTheNodeImmediately is the delete-then-create roll and the
// plain restart (open question 4): nobody holds the lock, so the agent owns the
// node as soon as it starts, exactly as before, and no takeover step runs —
// there is nothing another agent can have changed.
func TestFreeLockOwnsTheNodeImmediately(t *testing.T) {
	dir := shortDir(t)
	n := New(filepath.Join(dir, "agent.lock"), []string{filepath.Join(dir, "xds.sock")}, nil)
	var ran atomic.Bool
	n.AddStep(Step{Name: "x", Run: func(context.Context) error { ran.Store(true); return nil }})

	n.Claim()
	assert.False(t, n.Contended())
	startAsync(t, n)

	require.Eventually(t, n.IsOwned, time.Second, time.Millisecond)
	assert.True(t, isClosed(n.Owned()))
	assert.False(t, ran.Load(), "an uncontended start runs no takeover step")
}

// TestStandbyTakesOverWhenTheOwnerExits is the surge roll: the lock is held,
// so this agent is a standby until the holder is gone; then every takeover
// step runs, in order, BEFORE ownership is announced (the sockets bind on
// Owned, so the steps' work is in the first snapshot the proxy sees).
func TestStandbyTakesOverWhenTheOwnerExits(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "agent.lock")

	old := New(lock, nil, nil)
	old.Claim()
	require.False(t, old.Contended())

	standby := New(lock, nil, nil)
	var (
		mu    sync.Mutex
		order []string
	)
	step := func(name string) Step {
		return Step{Name: name, Run: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			assert.False(t, standby.IsOwned(), "step %s ran after ownership was announced", name)
			order = append(order, name)
			return nil
		}}
	}
	standby.AddStep(step("merge state"))
	standby.AddStep(step("reconcile storage"))
	standby.Claim()
	require.True(t, standby.Contended(), "a held lock makes this agent a standby")
	startAsync(t, standby)

	// The old agent is alive: no amount of waiting makes the standby the owner.
	time.Sleep(150 * time.Millisecond)
	assert.False(t, standby.IsOwned())

	old.release()
	require.Eventually(t, standby.IsOwned, 2*time.Second, time.Millisecond)
	mu.Lock()
	assert.Equal(t, []string{"merge state", "reconcile storage"}, order)
	mu.Unlock()
}

// TestHandoffLatencyIsTheKernelsNotAPoll: the blocking flock wakes on the
// release itself. The whole point of a lock over an API watch is that this is
// milliseconds, so bound it.
func TestHandoffLatencyIsTheKernelsNotAPoll(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "agent.lock")
	old := New(lock, nil, nil)
	old.Claim()
	standby := New(lock, nil, nil)
	standby.Claim()
	startAsync(t, standby)
	time.Sleep(50 * time.Millisecond)

	released := time.Now()
	old.release()
	<-standby.Owned()
	assert.Less(t, time.Since(released), 200*time.Millisecond)
}

// TestAFailedStepDoesNotStrandTheNode: the old owner is gone; refusing to
// serve because a reconcile failed would leave the node with nobody serving.
func TestAFailedStepDoesNotStrandTheNode(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "agent.lock")
	old := New(lock, nil, nil)
	old.Claim()
	standby := New(lock, nil, nil)
	var second atomic.Bool
	standby.AddStep(Step{Name: "fails", Run: func(context.Context) error { return errors.New("boom") }})
	standby.AddStep(Step{Name: "still runs", Run: func(context.Context) error { second.Store(true); return nil }})
	standby.Claim()
	startAsync(t, standby)
	old.release()
	require.Eventually(t, standby.IsOwned, 2*time.Second, time.Millisecond)
	assert.True(t, second.Load())
}

// TestAStandbyStoppedBeforeTakeoverNeverOwns: a standby deleted while the old
// agent still runs (a roll rolled back, a stuck old pod) exits without ever
// announcing ownership — so it never binds a socket or writes a file.
func TestAStandbyStoppedBeforeTakeoverNeverOwns(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "agent.lock")
	old := New(lock, nil, nil)
	old.Claim()
	t.Cleanup(old.release)

	standby := New(lock, nil, nil)
	standby.Claim()
	cancel, done := startAsync(t, standby)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after its context ended")
	}
	assert.False(t, standby.IsOwned())
	assert.ErrorIs(t, standby.WaitOwned(canceled()), context.Canceled)
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestAServerWithoutTheLockStillHoldsTheNode is the version-skew row: the
// first surge roll onto this version replaces an agent that takes no lock. The
// lock is free, but that agent is serving the node sockets; unlinking and
// re-binding under it would leave the proxy and the CNI plugin on a dead path
// the moment it exits (its Close unlinks by name). So: standby until nothing
// answers, and the takeover steps run then.
func TestAServerWithoutTheLockStillHoldsTheNode(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "cni.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	n := New(filepath.Join(dir, "agent.lock"), []string{filepath.Join(dir, "xds.sock"), sock}, nil)
	n.PollInterval = 5 * time.Millisecond
	var ran atomic.Bool
	n.AddStep(Step{Name: "reconcile", Run: func(context.Context) error { ran.Store(true); return nil }})
	n.Claim()
	require.True(t, n.Contended())
	startAsync(t, n)

	time.Sleep(100 * time.Millisecond)
	assert.False(t, n.IsOwned(), "a live server on a node socket is an owner, lock or no lock")

	require.NoError(t, ln.Close()) // Go unlinks the path, as the old agent's exit does
	require.Eventually(t, n.IsOwned, time.Second, time.Millisecond)
	assert.True(t, ran.Load())
}

// TestAStaleSocketIsNotAServer: what a SIGKILLed agent leaves behind is a
// socket file nobody listens on. That is ECONNREFUSED, not an owner.
func TestAStaleSocketIsNotAServer(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "xds.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
	_, err = os.Stat(sock)
	require.NoError(t, err, "the stale socket file must still exist for this test")

	n := New(filepath.Join(dir, "agent.lock"), []string{sock}, nil)
	n.Claim()
	assert.False(t, n.Contended())
}

// TestNoLockPathMeansOwned keeps --node-lock="" the pre-041 behaviour.
func TestNoLockPathMeansOwned(t *testing.T) {
	n := New("", nil, nil)
	n.Claim()
	startAsync(t, n)
	require.Eventually(t, n.IsOwned, time.Second, time.Millisecond)
}

// TestStandbyReadiness is "Readiness meaning": the DaemonSet controller deletes
// the old pod once the new one is Ready, so a standby is Ready only once its
// first snapshot is complete — and the check stops mattering once it owns the
// node.
func TestStandbyReadiness(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "agent.lock")
	old := New(lock, nil, nil)
	old.Claim()

	standby := New(lock, nil, nil)
	standby.Claim()
	complete := make(chan struct{})
	check := standby.StandbyChecker(complete)

	err := check(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "first snapshot not complete")

	close(complete)
	assert.NoError(t, check(nil), "a complete standby is Ready: the roll may delete the old agent")

	startAsync(t, standby)
	old.release()
	require.Eventually(t, standby.IsOwned, 2*time.Second, time.Millisecond)
	assert.NoError(t, standby.StandbyChecker(make(chan struct{}))(nil), "an owner passes regardless")
}

// TestTwoStandbysOneOwner: however many agents queue on the lock, exactly one
// owns the node at a time (a roll that surges twice on one node, or a
// standby restarting in place).
func TestTwoStandbysOneOwner(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "agent.lock")
	old := New(lock, nil, nil)
	old.Claim()

	a, b := New(lock, nil, nil), New(lock, nil, nil)
	a.Claim()
	b.Claim()
	startAsync(t, a)
	startAsync(t, b)
	old.release()

	require.Eventually(t, func() bool { return a.IsOwned() || b.IsOwned() }, 2*time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.False(t, a.IsOwned() && b.IsOwned(), "two owners")
	winner, loser := a, b
	if b.IsOwned() {
		winner, loser = b, a
	}
	winner.release()
	require.Eventually(t, loser.IsOwned, 2*time.Second, time.Millisecond, "the second standby never took over")
}
