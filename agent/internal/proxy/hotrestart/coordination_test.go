package hotrestart

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCoordSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	return New(Config{StateDir: t.TempDir()}, logr.Discard())
}

// writeRawState writes the state file directly with a given epoch and heartbeat age.
func writeRawState(t *testing.T, s *Supervisor, epoch int, age time.Duration) {
	t.Helper()
	ms := time.Now().Add(-age).UnixMilli()
	require.NoError(t, os.WriteFile(s.statePath(), []byte(fmt.Sprintf("%d %d\n", epoch, ms)), 0o644))
}

func TestWriteReadStateRoundTrip(t *testing.T) {
	s := newCoordSupervisor(t)
	s.writeState(7)
	epoch, hb, ok := s.readState()
	require.True(t, ok)
	assert.Equal(t, 7, epoch)
	assert.WithinDuration(t, time.Now(), hb, 2*time.Second)
}

func TestWriteStateDoesNotDowngradeFreshHigherEpoch(t *testing.T) {
	s := newCoordSupervisor(t)
	// A live successor owns epoch 5.
	writeRawState(t, s, 5, 0)
	// The draining predecessor heartbeats its lower epoch 4 — must be ignored.
	s.writeState(4)
	epoch, _, ok := s.readState()
	require.True(t, ok)
	assert.Equal(t, 5, epoch, "fresh higher epoch must not be downgraded")
}

func TestWriteStateOverwritesStaleHigherEpoch(t *testing.T) {
	s := newCoordSupervisor(t)
	// A crashed predecessor left epoch 5 with a stale heartbeat.
	writeRawState(t, s, 5, 2*predecessorStale)
	// A fresh node resets to epoch 0.
	s.writeState(0)
	epoch, _, ok := s.readState()
	require.True(t, ok)
	assert.Equal(t, 0, epoch, "stale higher epoch must be overwritten on reset")
}

func TestInitStartEpoch(t *testing.T) {
	t.Run("live predecessor -> epoch+1", func(t *testing.T) {
		s := newCoordSupervisor(t)
		writeRawState(t, s, 3, 0)
		s.initStartEpoch()
		assert.Equal(t, 4, s.nextEpoch)
	})
	t.Run("stale predecessor -> epoch 0", func(t *testing.T) {
		s := newCoordSupervisor(t)
		writeRawState(t, s, 3, 2*predecessorStale)
		s.initStartEpoch()
		assert.Equal(t, 0, s.nextEpoch)
	})
	t.Run("no state file -> epoch 0", func(t *testing.T) {
		s := newCoordSupervisor(t)
		s.initStartEpoch()
		assert.Equal(t, 0, s.nextEpoch)
	})
	t.Run("disabled (no StateDir) -> epoch 0", func(t *testing.T) {
		s := New(Config{}, logr.Discard())
		s.initStartEpoch()
		assert.Equal(t, 0, s.nextEpoch)
	})
}

func TestSupersededBySuccessor(t *testing.T) {
	s := newCoordSupervisor(t)
	// Our newest epoch is 2.
	writeRawState(t, s, 3, 0) // a fresh successor at epoch 3
	assert.True(t, s.supersededBySuccessor(2))

	writeRawState(t, s, 2, 0) // same epoch, not a successor
	assert.False(t, s.supersededBySuccessor(2))

	writeRawState(t, s, 3, 2*predecessorStale) // stale successor -> treat as crash
	assert.False(t, s.supersededBySuccessor(2))

	off := New(Config{}, logr.Discard())
	assert.False(t, off.supersededBySuccessor(2))
}

func TestReadyMarker(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{ReadyMarkerPath: filepath.Join(dir, "ready")}, logr.Discard())
	_, err := os.Stat(s.cfg.ReadyMarkerPath)
	require.True(t, os.IsNotExist(err))
	s.setReady()
	_, err = os.Stat(s.cfg.ReadyMarkerPath)
	assert.NoError(t, err)
	s.clearReady()
	_, err = os.Stat(s.cfg.ReadyMarkerPath)
	assert.True(t, os.IsNotExist(err))
}
