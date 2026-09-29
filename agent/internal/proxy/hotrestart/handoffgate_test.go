package hotrestart

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #991: under soak load the successor Envoy took 3-6s from fork to
// "starting workers", and two supervisor assumptions broke. These tests drive
// the coordination state machine (onLiveEpoch / onNotLiveEpoch /
// checkWedgeWatchdogs) on a fake clock, so the exact second at which the pod may
// report Ready — or must stop holding — is asserted, not approximated.

// fakeClock is a manually advanced clock for Supervisor.now. The state machine
// under test runs on the test goroutine, so it needs no locking.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) set(t time.Time)         { c.t = t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func markerExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// newGatedSuccessor returns a supervisor that has confirmed a live predecessor
// at epoch 3 (so it gates epoch 4) and has "forked" epoch 4 at the fake clock's
// current time, exactly as initStartEpoch + hotRestart leave it.
func newGatedSuccessor(t *testing.T, clk *fakeClock, pst time.Duration) (*Supervisor, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready")
	s := New(Config{
		StateDir:           t.TempDir(),
		ParentShutdownTime: pst,
		ReadyMarkerPath:    marker,
		AdminAddress:       fakeAdmin(t, adminLiveState, 3),
	}, slog.New(slog.DiscardHandler), nil)
	s.now = clk.now
	writeRawState(t, s, 3, 0)

	s.initStartEpoch(context.Background())
	require.Equal(t, 4, s.nextEpoch, "a live predecessor at epoch 3 must select start epoch 4")

	// hotRestart's bookkeeping for the fork of epoch 4, without a process.
	s.mu.Lock()
	s.nextEpoch = 5
	s.epochLaunched = clk.now()
	s.epochLive = false
	s.mu.Unlock()
	require.Equal(t, 4, s.currentEpoch())
	return s, marker
}

// TestSuccessorReadyGateAnchorsOnFirstLive is gate (A) of #991. The successor's
// first LIVE is observed 5s after the fork (rev242's measured fork->workers was
// 3.2-6.0s). Envoy arms parent shutdown at startWorkers, so the predecessor is
// still serving at fork+18s — the old, fork-anchored gate. The pod must stay
// NotReady until LIVE + ParentShutdownTime + liveGateBuffer.
//
// RED on the pre-fix code: it marked the pod Ready at fork+18s.
func TestSuccessorReadyGateAnchorsOnFirstLive(t *testing.T) {
	const pst = 15 * time.Second
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	fork := clk.now()
	s, marker := newGatedSuccessor(t, clk, pst)
	ctx := context.Background()

	clk.set(fork.Add(5 * time.Second))
	ready := s.onLiveEpoch(ctx, 4, false) // first observed LIVE
	require.False(t, ready)

	liveGate := fork.Add(5*time.Second + pst + liveGateBuffer)
	assert.Equal(t, liveGate, s.readyGateTime(),
		"the gate must be re-anchored on the first observed LIVE, using the ParentShutdownTime passed to envoy")

	// fork + ParentShutdownTime + readyGateBuffer: the old gate. The
	// predecessor dies at ~LIVE+15s = fork+20s, so Ready here is #991 group A.
	clk.set(fork.Add(pst + readyGateBuffer))
	ready = s.onLiveEpoch(ctx, 4, ready)
	assert.False(t, ready, "the successor must not be Ready at fork+%s while the predecessor still serves", pst+readyGateBuffer)
	assert.False(t, markerExists(t, marker))

	clk.set(liveGate.Add(-time.Millisecond))
	ready = s.onLiveEpoch(ctx, 4, ready)
	assert.False(t, ready, "not Ready a moment before LIVE+ParentShutdownTime+buffer")

	clk.set(liveGate)
	ready = s.onLiveEpoch(ctx, 4, ready)
	assert.True(t, ready, "Ready once LIVE+ParentShutdownTime+buffer has elapsed")
	assert.True(t, markerExists(t, marker))
}

// TestSuccessorReadyGateKeepsForkFloorOnFastLive is the control for the test
// above: a successor that reaches LIVE within a second of its fork (the
// unloaded case) keeps the fork-anchored gate — the re-anchor only ever moves
// the gate out.
func TestSuccessorReadyGateKeepsForkFloorOnFastLive(t *testing.T) {
	const pst = 15 * time.Second
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	fork := clk.now()
	s, _ := newGatedSuccessor(t, clk, pst)
	ctx := context.Background()

	clk.set(fork.Add(500 * time.Millisecond))
	ready := s.onLiveEpoch(ctx, 4, false)
	require.False(t, ready)
	forkGate := fork.Add(pst + readyGateBuffer)
	assert.Equal(t, forkGate, s.readyGateTime(), "a fast LIVE must not pull the fork-anchored gate in")

	clk.set(forkGate)
	assert.True(t, s.onLiveEpoch(ctx, 4, ready), "Ready at the fork-anchored gate")
}

// TestLiveAnchorIgnoresUngatedEpochs: only the epoch initStartEpoch gated is
// re-anchored. A later in-pod hot restart has no predecessor pod to protect,
// and a fresh epoch-0 start has no gate at all.
func TestLiveAnchorIgnoresUngatedEpochs(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, _ := newGatedSuccessor(t, clk, 15*time.Second)
	before := s.readyGateTime()

	clk.advance(time.Minute)
	gate, _, changed := s.liveAnchoredReadyGate(5, clk.now())
	assert.False(t, changed, "an in-pod epoch after the handoff is not the gated successor")
	assert.Equal(t, before, gate)

	fresh := New(Config{StateDir: t.TempDir(), ParentShutdownTime: 15 * time.Second}, slog.New(slog.DiscardHandler), nil)
	fresh.now = clk.now
	_, _, changed = fresh.liveAnchoredReadyGate(0, clk.now())
	assert.False(t, changed, "an ungated epoch-0 start must stay ungated")
	assert.True(t, fresh.readyGateTime().IsZero())
}

// newHoldingParent returns a Ready supervisor serving as the hot-restart parent:
// its Envoy (epoch 1) is tracked and the ready marker is present.
func newHoldingParent(t *testing.T, clk *fakeClock) (*Supervisor, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready")
	s := New(Config{
		StateDir:        t.TempDir(),
		ReadyMarkerPath: marker,
	}, slog.New(slog.DiscardHandler), nil)
	s.now = clk.now
	s.mu.Lock()
	s.children[1] = &exec.Cmd{} // tracked; only its presence matters here
	s.nextEpoch = 2
	s.epochLive = true
	s.mu.Unlock()
	s.setReady()
	return s, marker
}

// TestReadinessHeldThroughBusySuccessorAdmin is gate (B) of #991. Mid-handoff
// the shared admin port is the successor's, whose main thread is busy loading
// its first listener batch; /server_info misses the 1s probe timeout for a few
// seconds. The parent pod must keep its ready marker, or three failed exec
// probes later the DaemonSet deletes it before the successor has taken over.
//
// RED on the pre-fix code: it cleared the marker on the first unreachable tick.
func TestReadinessHeldThroughBusySuccessorAdmin(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, marker := newHoldingParent(t, clk)
	ctx := context.Background()

	// Reachable at the successor's (not-yet-LIVE) epoch: the classic hold.
	ready, holding := s.onNotLiveEpoch(ctx, 1, true, true, false, time.Time{})
	require.True(t, ready)
	require.True(t, holding)

	// The successor's admin stops answering within the probe timeout.
	unreachableSince := clk.now()
	for range 3 {
		clk.advance(time.Second)
		ready, holding = s.onNotLiveEpoch(ctx, 1, ready, false, holding, unreachableSince)
		assert.True(t, ready, "readiness must be held %s into an unreachable streak with our envoy tracked", clk.now().Sub(unreachableSince))
		assert.True(t, holding)
		assert.True(t, markerExists(t, marker), "the ready marker must not be cleared on a busy successor's admin timeout")
	}

	// The hold lasts right up to the admin watchdog's bound.
	clk.set(unreachableSince.Add(s.adminUnresponsiveDeadline() - time.Second))
	ready, holding = s.onNotLiveEpoch(ctx, 1, ready, false, holding, unreachableSince)
	assert.True(t, ready, "readiness must be held until the admin watchdog's bound")
	assert.True(t, s.holdingUnreachable)

	// The successor's admin answers again: still holding, streak over.
	ready, holding = s.onNotLiveEpoch(ctx, 1, ready, true, holding, time.Time{})
	assert.True(t, ready)
	assert.True(t, holding)
	assert.False(t, s.holdingUnreachable)
}

// TestReadinessHoldThroughUnreachableAdminIsBounded proves the bound of gate
// (B): an admin unreachable past the admin watchdog's deadline ends the hold —
// on the same tick the watchdog restarts the container. GREEN on both the
// pre-fix and fixed code; it guards against the fix holding forever.
func TestReadinessHoldThroughUnreachableAdminIsBounded(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, marker := newHoldingParent(t, clk)
	ctx := context.Background()
	bound := s.adminUnresponsiveDeadline()
	require.Equal(t, defaultAdminUnresponsiveDeadline, bound, "the hold is bounded by the existing admin watchdog, not a new constant")

	unreachableSince := clk.now()
	clk.set(unreachableSince.Add(bound - time.Second))
	require.False(t, s.checkWedgeWatchdogs(ctx, 1, true, false, unreachableSince),
		"the admin watchdog must not fire inside its bound")

	// Entered Ready and holding (as the fixed code leaves it one tick
	// earlier), so this asserts the bound itself, whatever the earlier ticks did.
	clk.set(unreachableSince.Add(bound + time.Second))
	ready, holding := s.onNotLiveEpoch(ctx, 1, true, false, true, unreachableSince)
	assert.False(t, ready, "an admin unreachable past the watchdog bound must end the hold")
	assert.False(t, holding)
	assert.False(t, markerExists(t, marker))
	assert.True(t, s.checkWedgeWatchdogs(ctx, 1, true, false, unreachableSince),
		"the admin watchdog must fire at the same bound")

	// And with nothing of ours left running, an unreachable admin never holds.
	s2, marker2 := newHoldingParent(t, clk)
	s2.mu.Lock()
	delete(s2.children, 1)
	s2.mu.Unlock()
	ready, _ = s2.onNotLiveEpoch(ctx, 1, true, false, true, clk.now())
	assert.False(t, ready)
	assert.False(t, markerExists(t, marker2))
}
