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

// TestHandleShutdownWaitsForTheSurgeSuccessor is the issue #795 regression test,
// and carries issue #771's with it.
//
// The loop context is already cancelled (its only real state), Envoy is LIVE at
// OUR epoch and no handoff has begun — a plain `kubectl delete pod`, i.e. also
// node drain, eviction and preemption. Two things must hold:
//
//   - #771: the probe must really reach the admin endpoint. Before that fix it
//     failed in microseconds without opening a socket, so the branch was chosen
//     off a lie.
//   - #795: knowing Envoy is LIVE at our epoch, the supervisor must NOT drain.
//     The DaemonSet's surge replacement is created within ~1s and hot-restarts
//     us; the old Envoy has to still be there when it does. So: keep the child
//     serving, and end only when the successor's protocol terminates it.
//
// On main this fails at the one-second `childTracked` assertion: handleShutdown
// takes the drain branch immediately and the child is already SIGTERMed and
// reaped. That is the measured production regression — 1.5-2.3s termination,
// ~8s with no Envoy on the node, 130/126 prober connection_errors (rev214 F2).
func TestHandleShutdownWaitsForTheSurgeSuccessor(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0) // LIVE at our own epoch: NOT mid-handoff
	s := newShutdownSupervisor(t, f)
	// 30s - (1s drain + 5s grace) - 10s margin = 14s of budget; the successor
	// below arrives at 2s, so the wait must end long before the bound.
	s.cfg.TerminationGrace = 30 * time.Second
	require.Equal(t, 14*time.Second, s.successorWaitBudget())
	before := f.serverInfoHits.Load()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	// One second in: still serving. Nothing of ours may have signalled Envoy,
	// and no drain may have been requested.
	time.Sleep(1 * time.Second)
	require.True(t, s.childTracked(0),
		"envoy was stopped while a surge successor could still have hot-restarted it")
	require.Zero(t, f.drainHits.Load(), "a serving envoy with a successor coming must not be drained")
	select {
	case <-done:
		t.Fatal("handleShutdown returned instead of waiting for the surge successor")
	default:
	}

	// 2s in, the surge replacement's Envoy takes over and its parent-shutdown
	// protocol terminates ours.
	time.Sleep(1 * time.Second)
	s.signalEpoch(0, syscall.SIGTERM)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("handleShutdown did not return after the successor took over")
	}

	assert.Less(t, time.Since(start), 14*time.Second,
		"the handoff must end when the successor arrives, not at the budget")
	assert.Greater(t, f.serverInfoHits.Load(), before,
		"the branch decision was made without the probe ever reaching the admin endpoint")
	assert.Zero(t, f.drainHits.Load(), "a handoff costs no drain")
	assert.False(t, s.childTracked(0))
}

// TestHandleShutdownDrainsImmediatelyWhenToldTo covers the escape hatch for a
// deployment with no surge replacement (--shutdown-drain-immediately): there is
// nothing to wait for, so the graceful drain must start at once — and it must
// still be a DRAIN, not the bare SIGTERM that cost the node its data plane.
func TestHandleShutdownDrainsImmediatelyWhenToldTo(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	s.cfg.TerminationGrace = 180 * time.Second // a budget it must not spend
	s.cfg.ShutdownDrainImmediately = true

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("--shutdown-drain-immediately did not drain immediately")
	}

	elapsed := time.Since(start)
	assert.Less(t, elapsed, s.successorWaitBudget(), "the escape hatch must not wait for a successor")
	assert.GreaterOrEqual(t, elapsed, s.cfg.DrainTime, "the drain window must still be honoured")
	assert.Equal(t, int64(1), f.drainHits.Load(), "listeners must be drained exactly once")
	assert.Equal(t, "graceful", f.drainQuery.Load(), "the drain must be the graceful one")
	assert.False(t, s.childTracked(0), "envoy must be stopped after the drain")
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

	assert.GreaterOrEqual(t, time.Since(start), 2*time.Second+s.cfg.DrainTime,
		"the fallback must not cut a handoff short before the budget, nor skip the drain window")
	assert.Equal(t, int64(1), f.drainHits.Load(),
		"the fallback must drain the listeners, not bare-SIGTERM a serving envoy")
	assert.Equal(t, "graceful", f.drainQuery.Load())
	assert.False(t, s.childTracked(0), "the fallback must drain and reap the child")
}

// TestHandleShutdownDrainsGracefullyWhenNoSuccessorArrives is the other half of
// #795: the successor wait is bounded, and what happens at the bound is a real
// drain.
//
// Envoy is LIVE at OUR epoch (no handoff started) and nothing ever comes — a
// node shutdown, a scale-down, `kubectl delete daemonset`, a replacement stuck
// Pending. The supervisor must hold the child for the whole budget, then ask
// Envoy to drain its listeners gracefully ONCE, wait --drain-time, and only then
// stop it. A bare SIGTERM here is what Envoy exits on in 0.33-0.74s with every
// connection still open, which is exactly what this branch exists to avoid.
func TestHandleShutdownDrainsGracefullyWhenNoSuccessorArrives(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0) // ours, still serving
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

	assert.GreaterOrEqual(t, time.Since(start), 2*time.Second+s.cfg.DrainTime,
		"the budget must be spent waiting, and the drain window honoured after it")
	assert.Equal(t, int64(1), f.drainHits.Load(), "listeners must be drained exactly once")
	assert.Equal(t, "graceful", f.drainQuery.Load(), "the drain must be the graceful one")
	assert.False(t, s.childTracked(0), "the child must be SIGTERMed and reaped after the drain")
}

// TestHandleShutdownStopsADeadChildWithoutDraining is the fourth branch: the
// admin does not answer at all, so the child is gone or its main thread is
// wedged. There is nothing to drain and no successor to wait for — asking a
// dead admin to drain would only burn the request timeout.
func TestHandleShutdownStopsADeadChildWithoutDraining(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	s.cfg.TerminationGrace = 180 * time.Second
	// The admin stops answering entirely (a wedged main thread leaves the socket
	// bound but never accepting; here, closed).
	f.srv.Close()

	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("an unreachable admin must not put the shutdown into a successor wait")
	}
	assert.Zero(t, f.drainHits.Load(), "a dead admin must not be asked to drain")
	assert.False(t, s.childTracked(0), "the child must still be reaped")
}
