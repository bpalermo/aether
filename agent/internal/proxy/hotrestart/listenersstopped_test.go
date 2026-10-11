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

// TestEnvoyThatStoppedItsListenersIsNotReadyAndIsHotRestarted is the kept
// pod's side of issue #1679, against the real Run loop.
//
// Another supervisor asked this pod's Envoy to drain its listeners, to replace
// it for a worker-count change, and never came back to stop it. The Envoy
// answers LIVE on /server_info and /ready exactly as before; what changed is
// listener_manager.listener_stopped. The pod used to stay Ready for good, with
// an Envoy that serves nothing.
//
// It must drop Ready when the counter moves, and, the Envoy still being there
// listenersStoppedGrace later, hot-restart it in place: the next epoch binds
// the listeners again.
func TestEnvoyThatStoppedItsListenersIsNotReadyAndIsHotRestarted(t *testing.T) {
	requireShell(t)
	f := newFakeAdmin(t, adminLiveState, 0)
	// A hot-restart child inherits its parent's counters: the value an Envoy
	// starts from says nothing, only an increase does.
	f.listenersStopped.Store(3)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	marker := filepath.Join(t.TempDir(), "ready")
	metrics, reader := newTestSupervisorMetrics(t)
	s := New(Config{
		EnvoyPath:                 stubEnvoy(t, recordPath),
		ConfigPath:                filepath.Join(t.TempDir(), "envoy.yaml"),
		BaseID:                    testBaseID(),
		DrainTime:                 1 * time.Second,
		ParentShutdownTime:        1 * time.Second,
		StateDir:                  t.TempDir(),
		ReadyMarkerPath:           marker,
		AdminAddress:              f.addr(),
		AdminUnresponsiveDeadline: time.Minute,
	}, slog.New(slog.DiscardHandler), metrics)
	f.identity.Store(s.adminIdentity)

	_, done := runUntilCleanup(t, s)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		10*time.Second, 20*time.Millisecond, "the pod never went Ready at epoch 0")
	require.Eventually(t, func() bool { return f.statsHits.Load() >= 1 }, 10*time.Second, 20*time.Millisecond,
		"the supervisor never read its envoy's listener state")
	time.Sleep(3 * time.Second)
	_, err := os.Stat(marker)
	require.NoError(t, err, "a counter inherited from a hot-restart parent made the pod NotReady")
	require.Equal(t, []string{"0"}, recordedEpochs(t, recordPath))

	// The drain time is up: the Envoy closes its listeners.
	stopped := time.Now()
	f.listenersStopped.Store(4)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return os.IsNotExist(err) },
		5*time.Second, 20*time.Millisecond, "the pod reports Ready with an envoy that has stopped its listeners")
	assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath),
		"the envoy is left to whoever drained it for listenersStoppedGrace")

	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 },
		listenersStoppedGrace+10*time.Second, 20*time.Millisecond, "the drained envoy was never hot-restarted")
	assert.Equal(t, []string{"0", "1"}, recordedEpochs(t, recordPath), "an in-place hot restart: the next epoch")
	assert.GreaterOrEqual(t, time.Since(stopped), listenersStoppedGrace,
		"the hot restart did not leave the envoy to a supervisor that may be replacing it")
	assert.EqualValues(t, 1,
		metricSumByAttr(t, reader, "aether.supervisor.restart_triggers", attrTriggerReason)[triggerListenersStopped])
	assert.Zero(t, f.drainHits.Load())

	// The next epoch is LIVE with its listeners (and its parent's counter).
	f.set(adminLiveState, 1)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		10*time.Second, 20*time.Millisecond, "the pod did not go Ready again with the restarted envoy")
	// Its parent exits (terminated by the child), and nothing asks for a
	// second restart: the baseline is the restarted Envoy's own.
	s.signalEpoch(0, syscall.SIGTERM)
	select {
	case err := <-done:
		t.Fatalf("Run returned %v", err)
	case <-time.After(listenersStoppedGrace + 4*time.Second):
	}
	assert.Equal(t, []string{"0", "1"}, recordedEpochs(t, recordPath), "the restored envoy was restarted again")
	_, err = os.Stat(marker)
	assert.NoError(t, err)
}

// newWatchedSupervisor is a supervisor with one tracked Envoy at epoch 0 whose
// admin (f) answers as its own, a fake clock, and its listener watch.
func newWatchedSupervisor(t *testing.T, f *fakeAdminServer) (*Supervisor, *listenerWatch, *fakeClock) {
	t.Helper()
	s := newShutdownSupervisor(t, f)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	return s, newListenerWatch(s), clk
}

func drainTrigger(s *Supervisor) string {
	select {
	case reason := <-s.selfTrigger:
		return reason
	default:
		return ""
	}
}

// TestListenerWatchBaseline: the baseline is the Envoy's own value, taken once
// the pod is Ready and the Envoy is the supervisor's only one; before that, no
// value means anything.
func TestListenerWatchBaseline(t *testing.T) {
	ctx := context.Background()
	f := newFakeAdmin(t, adminLiveState, 0)
	s, w, clk := newWatchedSupervisor(t, f)

	f.listenersStopped.Store(7)
	assert.False(t, w.stopped(ctx, 0, false), "not Ready yet: a cross-pod parent may still be merging its counters in")
	clk.advance(w.interval)
	f.listenersStopped.Store(9)
	assert.False(t, w.stopped(ctx, 0, true), "the first reading of a Ready pod is the baseline")
	clk.advance(w.interval)
	assert.False(t, w.stopped(ctx, 0, true), "the baseline itself is not an increase")

	clk.advance(w.interval)
	f.listenersStopped.Store(10)
	assert.True(t, w.stopped(ctx, 0, true))
	assert.Empty(t, drainTrigger(s), "no restart before the grace period")
}

// TestListenerWatchRestartsAfterTheGraceAndAsksAgain: one trigger once the
// grace period is over, none while it is pending, another when the Envoy still
// serves nothing listenersStoppedRetrigger later (a restart that was skipped).
func TestListenerWatchRestartsAfterTheGraceAndAsksAgain(t *testing.T) {
	ctx := context.Background()
	f := newFakeAdmin(t, adminLiveState, 0)
	s, w, clk := newWatchedSupervisor(t, f)
	require.False(t, w.stopped(ctx, 0, true)) // baseline 0

	clk.advance(w.interval)
	f.listenersStopped.Store(1)
	require.True(t, w.stopped(ctx, 0, true))

	clk.advance(listenersStoppedGrace - time.Second)
	require.True(t, w.stopped(ctx, 0, false))
	require.Empty(t, drainTrigger(s), "restarted before the grace period was over")

	clk.advance(time.Second)
	require.True(t, w.stopped(ctx, 0, false))
	require.Equal(t, triggerListenersStopped, drainTrigger(s))

	clk.advance(time.Second)
	require.True(t, w.stopped(ctx, 0, false))
	require.Empty(t, drainTrigger(s), "one restart is asked for, not one per tick")

	clk.advance(listenersStoppedRetrigger)
	require.True(t, w.stopped(ctx, 0, false))
	require.Equal(t, triggerListenersStopped, drainTrigger(s), "a restart that did not happen must be asked for again")
}

// TestListenerWatchChecksOncePerIntervalUntilStopped: one admin request per
// re-verify interval while all is well, one per tick once the listeners are
// known to be stopped.
func TestListenerWatchChecksOncePerIntervalUntilStopped(t *testing.T) {
	ctx := context.Background()
	f := newFakeAdmin(t, adminLiveState, 0)
	_, w, clk := newWatchedSupervisor(t, f)
	require.False(t, w.stopped(ctx, 0, true))
	require.EqualValues(t, 1, f.statsHits.Load())

	for range 3 {
		clk.advance(w.interval / 4)
		require.False(t, w.stopped(ctx, 0, true))
	}
	assert.EqualValues(t, 1, f.statsHits.Load(), "the listener state was read more than once in one interval")

	clk.advance(w.interval)
	f.listenersStopped.Store(1)
	require.True(t, w.stopped(ctx, 0, true))
	clk.advance(time.Second)
	require.True(t, w.stopped(ctx, 0, false))
	assert.EqualValues(t, 3, f.statsHits.Load(), "once stopped, every tick must look")
}

// TestListenerWatchReadsOnlyItsOwnEnvoy: the counter of another supervisor's
// Envoy on the shared admin address, of another epoch, or of an Envoy that is
// not this supervisor's only one, is never compared.
func TestListenerWatchReadsOnlyItsOwnEnvoy(t *testing.T) {
	ctx := context.Background()

	t.Run("another supervisor's envoy answers", func(t *testing.T) {
		f := newFakeAdmin(t, adminLiveState, 0)
		s, w, clk := newWatchedSupervisor(t, f)
		require.False(t, w.stopped(ctx, 0, true))
		// A cross-pod successor took the admin: it inherited our counters, and
		// ours rose when its workers started.
		f.identity.Store(predecessorIdentity)
		f.listenersStopped.Store(5)
		clk.advance(w.interval)
		assert.False(t, w.stopped(ctx, 0, true))
		assert.Zero(t, f.statsHits.Load()-1, "the stats of an envoy that is not ours were asked for")
		_, ok := s.ownListenersStopped(ctx, 0)
		assert.False(t, ok)
	})

	t.Run("our envoy answers for another epoch", func(t *testing.T) {
		f := newFakeAdmin(t, adminLiveState, 0)
		s, _, _ := newWatchedSupervisor(t, f)
		_, ok := s.ownListenersStopped(ctx, 1)
		assert.False(t, ok)
		count, ok := s.ownListenersStopped(ctx, 0)
		assert.True(t, ok)
		assert.Zero(t, count)
		assert.Contains(t, f.connectionsServing("/stats"), []string{"/server_info", "/stats"},
			"the counter must be read on the connection whose /server_info was checked")
	})

	t.Run("our envoy is not LIVE", func(t *testing.T) {
		f := newFakeAdmin(t, "INITIALIZING", 0)
		s, _, _ := newWatchedSupervisor(t, f)
		_, ok := s.ownListenersStopped(ctx, 0)
		assert.False(t, ok)
	})

	t.Run("an in-pod hot restart is in flight", func(t *testing.T) {
		f := newFakeAdmin(t, adminLiveState, 0)
		s, w, clk := newWatchedSupervisor(t, f)
		require.False(t, w.stopped(ctx, 0, true))
		// Epoch 1 is forked: epoch 0 is now a hot-restart parent, and stops
		// its listeners because its child serves.
		require.NoError(t, s.hotRestart())
		t.Cleanup(func() { s.signalEpoch(1, syscall.SIGKILL) })
		f.set(adminLiveState, 1)
		clk.advance(w.interval)
		assert.False(t, w.stopped(ctx, 1, true), "the child is not the supervisor's only envoy yet")
		// The parent stops its listeners, and the child merges the counter in.
		f.listenersStopped.Store(1)
		clk.advance(w.interval)
		assert.False(t, w.stopped(ctx, 1, true),
			"a counter merged in from a parent that is still alive was read as the child's own stop")
		// The parent is gone: the child's value, inherited, is the baseline.
		s.signalEpoch(0, syscall.SIGKILL)
		require.Eventually(t, func() bool {
			select {
			case exit := <-s.childExited:
				s.reap(exit.epoch)
			default:
			}
			return s.soleChild(1)
		}, 5*time.Second, 20*time.Millisecond)
		clk.advance(w.interval)
		assert.False(t, w.stopped(ctx, 1, true))
		clk.advance(w.interval)
		assert.False(t, w.stopped(ctx, 1, true), "the counter a child inherits from its parent is not a stop of its own")
	})
}

func TestParseStat(t *testing.T) {
	v, ok := parseStat([]byte("listener_manager.listener_stopped: 12\n"), adminListenersStoppedStat)
	assert.True(t, ok)
	assert.EqualValues(t, 12, v)

	// The filter is a regular expression: other names can match it.
	v, ok = parseStat([]byte("listener_managerXlistener_stopped: 9\nlistener_manager.listener_stopped: 2\n"), adminListenersStoppedStat)
	assert.True(t, ok)
	assert.EqualValues(t, 2, v)

	for _, body := range []string{"", "listener_manager.listener_stopped: many\n", "listener_manager.listener_added: 1\n", "no stats\n"} {
		_, ok = parseStat([]byte(body), adminListenersStoppedStat)
		assert.False(t, ok, body)
	}
}
