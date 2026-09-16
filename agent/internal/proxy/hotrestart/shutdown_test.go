package hotrestart

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireShell skips a test that needs the /bin/sh Envoy stub.
func requireShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}
}

// newShutdownSupervisor builds a production-shaped supervisor for the
// handleShutdown branch tests: StateDir set (so the cross-pod branch is
// reachable), a real admin address, and one live, tracked Envoy child at epoch
// 0 started through the real hotRestart path.
func newShutdownSupervisor(t *testing.T, f *fakeAdminServer) *Supervisor {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          1 * time.Second,
		ParentShutdownTime: 1 * time.Second,
		StateDir:           t.TempDir(),
		ReadyMarkerPath:    filepath.Join(t.TempDir(), "ready"),
		AdminAddress:       f.addr(),
	}, slog.New(slog.DiscardHandler), nil)

	require.NoError(t, s.hotRestart())
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 1 },
		5*time.Second, 20*time.Millisecond, "stub envoy did not start")
	require.Equal(t, 0, s.currentEpoch())
	t.Cleanup(func() { s.signalEpoch(0, syscall.SIGKILL) })
	return s
}

// cancelledCtx returns a context in exactly the state handleShutdown's single
// call site hands it: already cancelled by the signal handler.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestHandleShutdownProbesOnDetachedCtxAndDrains is the issue #771 regression
// test. The loop context is already cancelled (its only real state), Envoy is
// LIVE at OUR epoch and no successor exists: the probe must still reach the
// admin endpoint and the drain branch must be taken.
//
// Before the fix the probe failed in microseconds without opening a socket, so
// this always fell into the wait-for-successor branch and blocked until the
// kubelet's SIGKILL, with Envoy never signalled.
func TestHandleShutdownProbesOnDetachedCtxAndDrains(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0) // LIVE at our own epoch: NOT mid-handoff
	s := newShutdownSupervisor(t, f)
	before := f.serverInfoHits.Load()

	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("handleShutdown blocked: the drain branch was not taken")
	}

	assert.Greater(t, f.serverInfoHits.Load(), before,
		"the branch decision was made without the probe ever reaching the admin endpoint")
	assert.False(t, s.childTracked(0), "the drain branch must SIGTERM and reap the child")
}

// TestHandleShutdownWaitsForGenuineSuccessor is the control: the same cancelled
// context, but admin now answers LIVE at a NEWER epoch — a real cross-pod
// takeover. The supervisor must make that call off a real probe and then leave
// its Envoy alone, because the successor is still using it as its hot-restart
// parent (killing it aborts the successor with errno 111).
func TestHandleShutdownWaitsForGenuineSuccessor(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 1) // a successor has taken the admin port
	s := newShutdownSupervisor(t, f)
	before := f.serverInfoHits.Load()

	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	select {
	case <-done:
		t.Fatal("handleShutdown returned while a successor still needs our envoy as its parent")
	case <-time.After(2 * time.Second):
	}
	assert.Greater(t, f.serverInfoHits.Load(), before,
		"the mid-handoff decision was made without a real probe")
	assert.True(t, s.childTracked(0), "our envoy must not be signalled mid-handoff")

	// The successor's parent-shutdown protocol terminates our Envoy.
	s.signalEpoch(0, syscall.SIGTERM)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("handleShutdown did not return after the successor terminated our envoy")
	}
	assert.False(t, s.childTracked(0))
}

// TestSuccessorWaitBudget pins the arithmetic that decides when the mid-handoff
// wait gives up: the pod's whole grace period, minus what shutdown() will then
// spend draining, minus the margin. 0 means "wait indefinitely".
func TestSuccessorWaitBudget(t *testing.T) {
	budget := func(grace, drain time.Duration) time.Duration {
		s := New(Config{TerminationGrace: grace, DrainTime: drain}, slog.New(slog.DiscardHandler), nil)
		return s.successorWaitBudget()
	}

	// Deployed values (charts/aether/values.yaml): 180s grace, 10s drain.
	assert.Equal(t, 180*time.Second-(10*time.Second+shutdownGrace)-terminationFallbackMargin,
		budget(180*time.Second, 10*time.Second))
	// Unset grace: the pre-#771 unbounded behavior, which is what every chart
	// predating this flag and every unit-test supervisor gets.
	assert.Equal(t, time.Duration(0), budget(0, 10*time.Second))
	// A grace period too small to fit a drain has no safe cutoff: waiting beats
	// guaranteeing an errno-111 abort of a healthy successor.
	assert.Equal(t, time.Duration(0), budget(20*time.Second, 10*time.Second))
}

// TestHandleShutdownFallsBackToDrainWhenNoSuccessorComes covers the case the
// #771 reproduction could not induce on-cluster, because a `kubectl delete pod`
// of a DaemonSet member self-rescues into the surge path: a termination where no
// successor can ever appear (node shutdown, scale-down, `kubectl delete
// daemonset`, a replacement stuck Pending). Admin answers LIVE at a newer epoch,
// so the mid-handoff branch is correctly entered — but nothing ever terminates
// our Envoy. The wait must expire at the budget and drain the child, rather than
// sit there until the kubelet's SIGKILL with connections open.
func TestHandleShutdownFallsBackToDrainWhenNoSuccessorComes(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 1) // not ours: the mid-handoff branch
	s := newShutdownSupervisor(t, f)
	// DrainTime 1s + shutdownGrace 5s + margin 10s + 2s of actual wait.
	s.cfg.TerminationGrace = 18 * time.Second
	require.Equal(t, 2*time.Second, s.successorWaitBudget())

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("handleShutdown never gave up waiting for a successor that cannot come")
	}

	assert.GreaterOrEqual(t, time.Since(start), 2*time.Second,
		"the fallback must not cut a handoff short before the budget")
	assert.False(t, s.childTracked(0), "the fallback must drain and reap the child")
}
