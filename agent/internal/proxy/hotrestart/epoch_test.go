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

// TestReserveEpochNeverReusesATrackedEpoch is the S14 regression test. children
// is keyed by restart epoch, and resetEpochForRetry rewinds nextEpoch to 0 on
// every bind-collision retry, so a relaunch could overwrite the *exec.Cmd of a
// still-draining epoch-0 Envoy — orphaning that process (signalEpoch would then
// resolve epoch 0 to the new cmd) and undercounting awaitProtocolTermination's
// pending children.
func TestReserveEpochNeverReusesATrackedEpoch(t *testing.T) {
	requireShell(t)

	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		StateDir:           t.TempDir(),
	}, slog.New(slog.DiscardHandler), nil)

	require.NoError(t, s.hotRestart())
	require.True(t, s.childTracked(0))
	t.Cleanup(func() {
		s.signalEpoch(0, syscall.SIGKILL)
		s.signalEpoch(1, syscall.SIGKILL)
	})

	// The bind-collision retry path: rewind, then relaunch while epoch 0's
	// Envoy is still alive (it is draining, not reaped).
	s.resetEpochForRetry()
	require.Equal(t, -1, s.currentEpoch())
	require.NoError(t, s.hotRestart())

	assert.Equal(t, 1, s.currentEpoch(), "the relaunch must attach above the tracked epoch")
	assert.True(t, s.childTracked(0), "the still-draining epoch-0 child must not be dropped from tracking")
	assert.True(t, s.childTracked(1))

	// Both processes must be independently addressable — the whole point of not
	// reusing the key.
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 },
		5*time.Second, 20*time.Millisecond, "the second envoy never started")
	// Unordered: the two stubs append concurrently, and which lands first is
	// not part of the invariant — that both exist as distinct processes is.
	assert.ElementsMatch(t, []string{"0", "1"}, recordedEpochs(t, recordPath))
}

// TestHotRestartDoesNotAdvanceEpochWhenStartFails is the S15 regression test. A
// fork failure is non-fatal on the handleDebounce path ("keeping current
// epoch"), so advancing nextEpoch before Start() left currentEpoch() naming an
// epoch with no child: both wedge watchdogs and the readiness hold are gated on
// childTracked(epoch), so none could ever fire again.
func TestHotRestartDoesNotAdvanceEpochWhenStartFails(t *testing.T) {
	requireShell(t)

	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		StateDir:           t.TempDir(),
	}, slog.New(slog.DiscardHandler), nil)

	require.NoError(t, s.hotRestart())
	t.Cleanup(func() { s.signalEpoch(0, syscall.SIGKILL) })
	require.Equal(t, 0, s.currentEpoch())

	// Next fork fails (ENOENT stands in for ENOMEM / pid limit).
	s.cfg.EnvoyPath = filepath.Join(t.TempDir(), "no-such-envoy")
	require.Error(t, s.hotRestart())

	assert.Equal(t, 0, s.currentEpoch(), "a failed fork must not advance the epoch")
	assert.True(t, s.childTracked(s.currentEpoch()),
		"currentEpoch() must keep naming a tracked child, or the watchdogs and the readiness hold are disarmed")
	assert.False(t, s.childTracked(1))
}

// TestInitStartEpochPublishesEpochAndGateTogether is the S16 regression test:
// the readiness gate for a cross-pod successor must be in place by the time the
// successor epoch is visible, not set by a separate later step. Otherwise a
// watchLiveness tick landing in between sees the PREDECESSOR answering LIVE at
// exactly that epoch and marks the pod Ready before this pod's Envoy exists —
// #132's hazard, re-entering through the bind-collision retry path where the
// previous gate is certainly stale.
func TestInitStartEpochPublishesEpochAndGateTogether(t *testing.T) {
	s := New(Config{
		StateDir:           t.TempDir(),
		ParentShutdownTime: 15 * time.Second,
		AdminAddress:       fakeAdmin(t, adminLiveState, 3),
	}, slog.New(slog.DiscardHandler), nil)
	writeRawState(t, s, 3, 0)

	// A stale gate from a previous attempt, as a bind-collision retry would
	// leave behind (retries can run minutes against a 15s parent-shutdown-time).
	s.mu.Lock()
	s.readyGate = time.Now().Add(-time.Hour)
	s.mu.Unlock()

	s.initStartEpoch(context.Background())

	require.Equal(t, 3, s.currentEpoch(), "predecessor at epoch 3 should give a start epoch of 4")
	assert.True(t, time.Now().Before(s.readyGateTime()),
		"initStartEpoch must gate readiness in the same critical section that publishes the successor epoch")
	assert.WithinDuration(t, time.Now().Add(15*time.Second+readyGateBuffer), s.readyGateTime(), 2*time.Second)
}

// TestInitStartEpochDoesNotGateWithoutAPredecessor is the control for the test
// above: a fresh node (no live predecessor) starts at epoch 0 and must become
// Ready as soon as its own Envoy is LIVE, with no gate at all.
func TestInitStartEpochDoesNotGateWithoutAPredecessor(t *testing.T) {
	s := New(Config{
		StateDir:           t.TempDir(),
		ParentShutdownTime: 15 * time.Second,
		AdminAddress:       fakeAdmin(t, adminLiveState, 3),
	}, slog.New(slog.DiscardHandler), nil)
	writeRawState(t, s, 3, 2*predecessorStale) // stale: the predecessor is gone

	s.initStartEpoch(context.Background())

	require.Equal(t, 0, s.nextEpoch)
	assert.True(t, s.readyGateTime().IsZero(), "a fresh epoch-0 start must not be gated")
}

// TestReadinessHeldWhileAnEarlierEpochStillServes is the S17 regression test —
// and a candidate fix for the unexplained proxy readiness-marker flap on
// w01/w05 in the 2026-09-03 soak.
//
// During a bind-collision retry currentEpoch() is the rewound nextEpoch-1 (-1),
// an epoch that never had a child, while the previous epoch's Envoy is still
// tracked and still serving every request on the node. Keying the readiness hold
// on childTracked(currentEpoch()) dropped the ready marker for the whole retry.
func TestReadinessHeldWhileAnEarlierEpochStillServes(t *testing.T) {
	requireShell(t)

	marker := filepath.Join(t.TempDir(), "ready")
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		StateDir:           t.TempDir(),
		ReadyMarkerPath:    marker,
	}, slog.New(slog.DiscardHandler), nil)

	require.NoError(t, s.hotRestart()) // epoch 0, still tracked and serving
	t.Cleanup(func() { s.signalEpoch(0, syscall.SIGKILL) })
	s.setReady()

	// The retry has rewound the epoch; admin is reachable but not LIVE at -1.
	s.resetEpochForRetry()
	require.Equal(t, -1, s.currentEpoch())

	ready, holding := s.onNotLiveEpoch(context.Background(), s.currentEpoch(), true, true, false)
	assert.True(t, ready, "readiness must be held while an earlier epoch's envoy still serves the node")
	assert.True(t, holding)
	_, err := os.Stat(marker)
	assert.NoError(t, err, "the ready marker must not be cleared during a retry")

	// Control: once nothing of ours is left running, readiness must drop.
	s.signalEpoch(0, syscall.SIGTERM)
	require.Eventually(t, func() bool {
		select {
		case exit := <-s.childExited:
			s.reap(exit.epoch)
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}, 10*time.Second, 10*time.Millisecond, "stub did not exit on SIGTERM")

	ready, holding = s.onNotLiveEpoch(context.Background(), s.currentEpoch(), true, true, true)
	assert.False(t, ready, "with no child left tracked, readiness must clear")
	assert.False(t, holding)
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "the ready marker must be cleared once nothing of ours serves")
}
