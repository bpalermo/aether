package hotrestart

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCoordSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	return New(Config{StateDir: t.TempDir()}, logr.Discard(), nil)
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
	t.Run("near-stale file and admin NOT live -> re-probes until stale, then epoch 0", func(t *testing.T) {
		s := newCoordSupervisor(t)
		s.cfg.AdminAddress = fakeAdmin(t, "PRE_INITIALIZING", 3)
		// Heartbeat about to go stale: the loop re-probes (fresh file must not be
		// trusted to be dead on one failed probe) and exits to epoch 0 once stale.
		writeRawState(t, s, 3, predecessorStale-200*time.Millisecond)
		s.initStartEpoch(ctx)
		assert.Equal(t, 0, s.nextEpoch)
	})

	t.Run("fresh file with transiently failing admin -> retries then epoch+1", func(t *testing.T) {
		s := newCoordSupervisor(t)
		// Admin fails the first two probes, then confirms LIVE at epoch 3.
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(w, `{"state":"LIVE","command_line_options":{"restart_epoch":3}}`)
		}))
		t.Cleanup(srv.Close)
		s.cfg.AdminAddress = srv.Listener.Addr().String()
		writeRawState(t, s, 3, 0)
		s.initStartEpoch(ctx)
		assert.Equal(t, 4, s.nextEpoch, "transient admin failures must not abandon a fresh predecessor")
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
}

func TestReadyMarker(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{ReadyMarkerPath: filepath.Join(dir, "ready")}, logr.Discard(), nil)
	_, err := os.Stat(s.cfg.ReadyMarkerPath)
	require.True(t, os.IsNotExist(err))
	s.setReady()
	_, err = os.Stat(s.cfg.ReadyMarkerPath)
	assert.NoError(t, err)
	s.clearReady()
	_, err = os.Stat(s.cfg.ReadyMarkerPath)
	assert.True(t, os.IsNotExist(err))
}

// TestReadinessHeldWhileServingParentMidHandoff reproduces the 2026-06-11 e2e
// finding: a surging successor pod binds the shared admin socket and answers at
// its (not yet LIVE) epoch, which previously made the old pod's supervisor drop
// the ready marker immediately — the DaemonSet (maxUnavailable=0 counts only
// Ready pods) then deleted the old pod mid-handoff and the kubelet's grace
// SIGKILL cut the hot-restart parent from under the successor (errno-111
// abort). While this supervisor's newest child is alive and admin is reachable,
// readiness must be HELD; it clears once the child exits (protocol terminate).
func TestReadinessHeldWhileServingParentMidHandoff(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	marker := filepath.Join(t.TempDir(), "ready")

	// Mutable admin: starts LIVE at this supervisor's epoch 0, then flips to a
	// successor answering INITIALIZING at epoch 1.
	var adminResp atomic.Value
	adminResp.Store(`{"state":"LIVE","command_line_options":{"restart_epoch":0}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, adminResp.Load().(string))
	}))
	defer srv.Close()

	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         configPath,
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		StateDir:           t.TempDir(),
		ReadyMarkerPath:    marker,
		AdminAddress:       srv.Listener.Addr().String(),
	}, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})

	// Phase A: LIVE at our epoch -> Ready.
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "pod never became ready")

	// Phase B: a successor binds admin, not yet LIVE. Readiness must be held
	// while our child (the successor's hot-restart parent) is alive.
	adminResp.Store(`{"state":"INITIALIZING","command_line_options":{"restart_epoch":1}}`)
	assert.Never(t, func() bool {
		_, err := os.Stat(marker)
		return os.IsNotExist(err)
	}, 3*readyPollInterval+500*time.Millisecond, 100*time.Millisecond,
		"ready marker dropped mid-handoff while still the serving parent")

	// Phase C: the successor's protocol terminates our child (clean exit) ->
	// nothing left serving here -> readiness clears.
	s.signalEpoch(0, syscall.SIGTERM)
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return os.IsNotExist(err)
	}, 10*time.Second, 100*time.Millisecond, "ready marker must clear once the child exits")
}

// TestHandoffWatchdogFiresWhenSuccessorNeverLive reproduces the 2026-06-10 e2e
// wedge: a cross-pod successor starts at epoch N+1 against a confirmed-live
// predecessor, the predecessor then dies mid hot-restart RPC, and the successor's
// main thread blocks forever — admin keeps answering with the old epoch (here: a
// static fake), LIVE at the new epoch never arrives. The handoff watchdog must
// bail out of Run with a non-nil error so the container restarts.
func TestHandoffWatchdogFiresWhenSuccessorNeverLive(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")

	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         configPath,
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		StateDir:           t.TempDir(),
		ReadyMarkerPath:    filepath.Join(t.TempDir(), "ready"),
		// The "predecessor": admin permanently LIVE at epoch 0, so initStartEpoch
		// selects epoch 1 — which then never reaches LIVE (the wedge).
		AdminAddress:    fakeAdmin(t, "LIVE", 0),
		HandoffDeadline: 1 * time.Second,
	}, logr.Discard(), nil)
	writeRawState(t, s, 0, 0)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.Background()) }()

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "handoff watchdog")
	case <-time.After(15 * time.Second):
		t.Fatal("handoff watchdog did not fire")
	}
	// It must have attempted the cross-pod successor epoch, not a fresh epoch 0.
	assert.Equal(t, []string{"1"}, recordedEpochs(t, recordPath))
}

// TestAdminWatchdogFiresWhenAdminUnreachableAfterLive covers the post-LIVE
// variant of the wedge: Envoy reached LIVE (pod Ready), then the admin endpoint
// stops answering entirely (main thread blocked in a hot-restart stats-merge
// against a dead parent) while the child process stays alive. The admin watchdog
// must bail out of Run with a non-nil error.
func TestAdminWatchdogFiresWhenAdminUnreachableAfterLive(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	marker := filepath.Join(t.TempDir(), "ready")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"state":"LIVE","command_line_options":{"restart_epoch":0}}`)
	}))
	defer srv.Close()

	s := New(Config{
		EnvoyPath:                 stubEnvoy(t, recordPath),
		ConfigPath:                configPath,
		DrainTime:                 time.Second,
		ParentShutdownTime:        time.Second,
		StateDir:                  t.TempDir(),
		ReadyMarkerPath:           marker,
		AdminAddress:              srv.Listener.Addr().String(),
		AdminUnresponsiveDeadline: 1 * time.Second,
	}, logr.Discard(), nil)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.Background()) }()

	// Wait for LIVE to be observed (ready marker present), then kill the admin.
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "pod never became ready")
	srv.Close()

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "admin watchdog")
	case <-time.After(15 * time.Second):
		t.Fatal("admin watchdog did not fire")
	}
}
