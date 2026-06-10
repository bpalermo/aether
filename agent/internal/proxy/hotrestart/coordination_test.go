package hotrestart

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// fakeAdmin starts an Envoy-admin stub that reports the given state and restart
// epoch on /server_info, and returns its host:port.
func fakeAdmin(t *testing.T, state string, epoch int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"state":%q,"command_line_options":{"restart_epoch":%d}}`, state, epoch)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
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
	ctx := context.Background()
	t.Run("fresh file + admin LIVE at that epoch -> epoch+1", func(t *testing.T) {
		s := newCoordSupervisor(t)
		s.cfg.AdminAddress = fakeAdmin(t, "LIVE", 3)
		writeRawState(t, s, 3, 0)
		s.initStartEpoch(ctx)
		assert.Equal(t, 4, s.nextEpoch)
	})
	t.Run("fresh file but admin NOT live (dead predecessor) -> epoch 0", func(t *testing.T) {
		s := newCoordSupervisor(t)
		s.cfg.AdminAddress = fakeAdmin(t, "PRE_INITIALIZING", 3)
		writeRawState(t, s, 3, 0)
		s.initStartEpoch(ctx)
		assert.Equal(t, 0, s.nextEpoch)
	})
	t.Run("stale predecessor -> epoch 0", func(t *testing.T) {
		s := newCoordSupervisor(t)
		s.cfg.AdminAddress = fakeAdmin(t, "LIVE", 3)
		writeRawState(t, s, 3, 2*predecessorStale)
		s.initStartEpoch(ctx)
		assert.Equal(t, 0, s.nextEpoch)
	})
	t.Run("no state file -> epoch 0", func(t *testing.T) {
		s := newCoordSupervisor(t)
		s.initStartEpoch(ctx)
		assert.Equal(t, 0, s.nextEpoch)
	})
	t.Run("disabled (no StateDir) -> epoch 0", func(t *testing.T) {
		s := New(Config{}, logr.Discard())
		s.initStartEpoch(ctx)
		assert.Equal(t, 0, s.nextEpoch)
	})
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
