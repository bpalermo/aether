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
