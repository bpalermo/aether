package hotrestart

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSurgeSupervisor is the surge pod of a roll that changes the worker count:
// the node's Envoy (f) is LIVE at predecessorEpoch with predecessorWorkers,
// the heartbeat is fresh, and this supervisor's Envoy would run 2.
func newSurgeSupervisor(t *testing.T, f *fakeAdminServer, recordPath string, drain time.Duration) *Supervisor {
	t.Helper()
	requireShell(t)
	s := New(Config{
		EnvoyPath:                 stubEnvoy(t, recordPath),
		ConfigPath:                filepath.Join(t.TempDir(), "envoy.yaml"),
		BaseID:                    testBaseID(),
		DrainTime:                 drain,
		ParentShutdownTime:        1 * time.Second,
		ExtraArgs:                 []string{"--concurrency", "2"},
		StateDir:                  t.TempDir(),
		ReadyMarkerPath:           filepath.Join(t.TempDir(), "ready"),
		AdminAddress:              f.addr(),
		AdminUnresponsiveDeadline: 2 * time.Second,
		// 14 s for the replacement once the termination is requested:
		// 30 s - (drain 1 s + 5 s) - 10 s.
		TerminationGrace: 30 * time.Second,
	}, slog.New(slog.DiscardHandler), nil)
	writeRawState(t, s, predecessorEpoch, 0)
	return s
}

// TestTerminationDuringTheWorkerCountDrainFinishesTheReplacement is issue
// #1679, against the real Run loop.
//
// A roll changes the Envoy worker count and is reverted while the surge pod
// starts. The surge pod's supervisor has asked the kept pod's Envoy to drain
// (#1136) and is waiting out the drain time when its SIGTERM arrives. It used
// to stop there: with its context cancelled it could not reach the admin, so
// it did not stop the predecessor, read the failed dial as "the predecessor is
// gone", started nothing and exited. The kept pod's Envoy closed its listeners
// and stayed LIVE on the admin: a Ready pod, and a node with no data plane.
//
// A supervisor that has asked a predecessor to drain finishes the replacement:
// it stops the predecessor and starts its own Envoy, and only then obeys the
// termination.
func TestTerminationDuringTheWorkerCountDrainFinishesTheReplacement(t *testing.T) {
	f := newPredecessor(t, 4)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newSurgeSupervisor(t, f, recordPath, 1*time.Second)

	cancel, done := runUntilCleanup(t, s)
	require.Eventually(t, func() bool { return f.drainHits.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
		"the predecessor was never asked to drain")
	// The DaemonSet deletes the surge pod during the drain-time wait.
	cancel()

	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 1 }, 10*time.Second, 10*time.Millisecond,
		"the supervisor that drained the node's envoy started none of its own: the node is left with an envoy that "+
			"has closed its listeners (quit sent: %d)", f.quitHits.Load())
	assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath), "a fresh lineage, as after any worker-count change")
	assert.EqualValues(t, 1, f.quitHits.Load(), "the drained predecessor must be stopped, not left LIVE with no listeners")
	assert.Contains(t, f.connectionsServing("/quitquitquit"), []string{"/server_info", "/quitquitquit"},
		"the stop must still ride a connection that re-checked the predecessor's identity")

	// The termination is obeyed once the replacement is done (here: once its
	// budget is spent, the fake admin being gone with the predecessor).
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the termination was never obeyed")
	}
}

// TestTerminationBeforeTheDrainDrainsNothing: the other half of the invariant.
// A supervisor whose termination was requested before it asked the predecessor
// to drain must not ask: it would take the node's listeners and then leave.
func TestTerminationBeforeTheDrainDrainsNothing(t *testing.T) {
	f := newPredecessor(t, 4)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newSurgeSupervisor(t, f, recordPath, 1*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	assert.Zero(t, f.drainHits.Load(), "a supervisor that was already told to stop drained the node's envoy")
	assert.Zero(t, f.quitHits.Load())
	assert.Empty(t, recordedEpochs(t, recordPath), "and it must start no envoy")
}

// heldTerminationLog signals, once, when the supervisor logs that it holds a
// termination back for an unfinished replacement (awaitReplacement).
type heldTerminationLog struct {
	slog.Handler
	once sync.Once
	held chan struct{}
}

func (h *heldTerminationLog) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "finishing the replacement first") {
		h.once.Do(func() { close(h.held) })
	}
	return nil
}

func (h *heldTerminationLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *heldTerminationLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *heldTerminationLog) WithGroup(string) slog.Handler            { return h }

// TestTerminationHeldForADrainThatIsRefusedStartsNoEnvoy: the termination
// arrives after the replacement was begun and before the predecessor answers
// the drain, and the predecessor then refuses it. The replacement is over
// (nothing was drained), and the termination that was held for it is still
// only on its way: the context the supervisor runs on is cancelled by a poll,
// up to replacementPoll later. Run reaches its first fork inside that window.
//
// It must fork nothing: this pod is terminating, the predecessor serves, and
// an Envoy hot-restarted from it now would take the node over only to be
// drained. The context cannot say so yet; the recorded request does.
func TestTerminationHeldForADrainThatIsRefusedStartsNoEnvoy(t *testing.T) {
	f := newPredecessor(t, 4)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newSurgeSupervisor(t, f, recordPath, 1*time.Second)
	held := &heldTerminationLog{Handler: slog.DiscardHandler, held: make(chan struct{})}
	s.log = slog.New(held)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var heldInTime atomic.Bool
	f.refuseDrain.Store(func() int {
		// The DaemonSet deletes the surge pod while the drain request is in
		// flight. Answer only once the supervisor holds the termination back.
		cancel()
		select {
		case <-held.held:
			heldInTime.Store(true)
		case <-time.After(10 * time.Second):
		}
		return http.StatusInternalServerError
	})

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		for e := 0; e < 6; e++ {
			s.signalEpoch(e, syscall.SIGKILL)
		}
	})
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	require.True(t, heldInTime.Load(), "the termination was never held back for the replacement: this test no longer "+
		"reaches the window it is about")
	assert.EqualValues(t, 1, f.drainHits.Load(), "the predecessor was not asked to drain exactly once")
	assert.Zero(t, f.quitHits.Load(), "a predecessor that refused the drain serves: it must not be stopped")
	assert.False(t, s.replacementUnfinished(), "a drain that was refused is not a replacement to finish")
	assert.Empty(t, recordedEpochs(t, recordPath),
		"a terminating supervisor started an envoy after the predecessor refused its drain: it takes the node over "+
			"from an envoy that serves, only to be drained itself")
}

// TestBeginReplacementRefusesAfterTheTerminationRequest pins the ordering the
// two tests above depend on: the request is recorded before the replacement
// is looked at, and a replacement is not begun once the request is recorded.
func TestBeginReplacementRefusesAfterTheTerminationRequest(t *testing.T) {
	s := New(Config{}, slog.New(slog.DiscardHandler), nil)
	require.True(t, s.beginReplacement())
	require.True(t, s.replacementUnfinished())
	s.endReplacement()
	require.False(t, s.replacementUnfinished())

	s.mu.Lock()
	s.terminationAt = time.Now()
	s.mu.Unlock()
	assert.False(t, s.beginReplacement(), "a replacement was begun after this pod's termination was requested")
	assert.False(t, s.replacementUnfinished())
}

// TestHeldTerminationIsReleasedWhenOurEnvoyIsLive: the replacement is finished
// when this supervisor's own Envoy is LIVE, and the termination is delivered
// then, not at the end of the budget.
func TestHeldTerminationIsReleasedWhenOurEnvoyIsLive(t *testing.T) {
	s := New(Config{DrainTime: time.Minute}, slog.New(slog.DiscardHandler), nil)
	require.True(t, s.beginReplacement())

	ctx, cancel := context.WithCancel(context.Background())
	run, stop := s.deferTermination(ctx)
	defer stop()
	cancel()
	select {
	case <-run.Done():
		t.Fatal("the termination was delivered while the replacement was unfinished")
	case <-time.After(500 * time.Millisecond):
	}
	assert.Positive(t, s.terminationRequestedFor())

	s.markEpochLive()
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the termination was not delivered once this supervisor's envoy was LIVE")
	}
}

// TestTerminationIsNotHeldWithoutAReplacement: every other termination reaches
// the supervisor at once, as before.
func TestTerminationIsNotHeldWithoutAReplacement(t *testing.T) {
	s := New(Config{DrainTime: time.Minute}, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(context.Background())
	run, stop := s.deferTermination(ctx)
	defer stop()
	require.NoError(t, run.Err())
	cancel()
	select {
	case <-run.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a termination with no replacement in flight was held back")
	}
}

// TestReplacementFinishBudget: the hold never outlasts what the pod's own
// drain fallback leaves of its termination grace.
func TestReplacementFinishBudget(t *testing.T) {
	s := New(Config{DrainTime: 10 * time.Second, TerminationGrace: 180 * time.Second}, slog.New(slog.DiscardHandler), nil)
	assert.Equal(t, 56*time.Second, s.replacementFinishBudget(), "2 x drain + 5 s + 1 s + 30 s")

	s = New(Config{DrainTime: 10 * time.Second, TerminationGrace: 45 * time.Second}, slog.New(slog.DiscardHandler), nil)
	assert.Equal(t, 20*time.Second, s.replacementFinishBudget(), "45 s - (10 s + 5 s) - 10 s")

	s = New(Config{DrainTime: 10 * time.Second}, slog.New(slog.DiscardHandler), nil)
	assert.Equal(t, 56*time.Second, s.replacementFinishBudget(), "an unknown grace period does not shorten it")
}

// TestSuccessorWaitBudgetCountsTheHeldTermination: the time a termination was
// held back is taken from the wait for a successor, so the drain fallback
// still fits in the grace period. It never becomes 0, which means "no bound".
func TestSuccessorWaitBudgetCountsTheHeldTermination(t *testing.T) {
	s := New(Config{DrainTime: 10 * time.Second, TerminationGrace: 180 * time.Second}, slog.New(slog.DiscardHandler), nil)
	require.Equal(t, 155*time.Second, s.successorWaitBudget())

	s.mu.Lock()
	s.terminationAt = time.Now().Add(-40 * time.Second)
	s.mu.Unlock()
	assert.InDelta(t, (115 * time.Second).Seconds(), s.successorWaitBudget().Seconds(), 1)

	s.mu.Lock()
	s.terminationAt = time.Now().Add(-10 * time.Minute)
	s.mu.Unlock()
	assert.Equal(t, time.Millisecond, s.successorWaitBudget())
}

// TestAnotherLineageLiveAtOurEpochIsNotOurs is the note of the review that
// found issue #1679: readiness was keyed by epoch number, not identity.
//
// The kept pod ran epoch 0. A worker-count successor stopped its Envoy and
// started a fresh lineage, which is LIVE at epoch 0 too. The kept pod has no
// Envoy, asks the node's admin about "its" epoch 0, and must not read the
// answer as its own: it marked itself Ready with nothing in it.
func TestAnotherLineageLiveAtOurEpochIsNotOurs(t *testing.T) {
	requireShell(t)
	f := newFakeAdmin(t, adminLiveState, 0)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	marker := filepath.Join(t.TempDir(), "ready")
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
	}, slog.New(slog.DiscardHandler), nil)
	f.identity.Store(s.adminIdentity)

	runUntilCleanup(t, s)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		10*time.Second, 20*time.Millisecond, "the kept pod never went Ready at epoch 0")

	// The successor stops our Envoy (exit 0) and its own lineage answers the
	// node's admin: LIVE, epoch 0, another supervisor's identity.
	f.identity.Store(predecessorIdentity)
	s.signalEpoch(0, syscall.SIGTERM)
	require.Eventually(t, func() bool { return !s.anyChildTracked() }, 10*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return os.IsNotExist(err) },
		5*time.Second, 20*time.Millisecond, "a pod with no envoy stayed Ready")

	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a pod with no envoy marked itself Ready: another lineage is LIVE at its last epoch number")
		}
	}
	live, reachable := s.adminServerInfo(context.Background(), 0)
	assert.False(t, live)
	assert.True(t, reachable)
	assert.True(t, s.adminPeerLiveAtEpoch(context.Background(), 0),
		"the same answer is a live PEER at epoch 0: what a pod looking for an envoy to hot-restart from asks")
}
