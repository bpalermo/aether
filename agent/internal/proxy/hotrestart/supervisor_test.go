package hotrestart

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEnvoyCmd(t *testing.T) {
	s := New(Config{
		EnvoyPath:          "/usr/local/bin/envoy",
		ConfigPath:         "/etc/envoy/envoy.yaml",
		BaseID:             7,
		DrainTime:          45 * time.Second,
		ParentShutdownTime: 60 * time.Second,
		ExtraArgs:          []string{"-l", "info", "--service-cluster", "aether-proxy"},
	}, slog.New(slog.DiscardHandler), nil)

	cmd := s.buildEnvoyCmd(3)

	assert.Equal(t, "/usr/local/bin/envoy", cmd.Path)
	want := []string{
		"/usr/local/bin/envoy",
		"-c", "/etc/envoy/envoy.yaml",
		"--base-id", "7",
		"--restart-epoch", "3",
		"--drain-time-s", "45",
		"--parent-shutdown-time-s", "60",
		"-l", "info", "--service-cluster", "aether-proxy",
	}
	assert.Equal(t, want, cmd.Args)
}

// stubEnvoy writes a tiny shell script that records each invocation's
// --restart-epoch to recordPath and then blocks until signaled, exiting 0 on
// SIGTERM. It stands in for the Envoy binary so the supervisor's lifecycle can be
// exercised without a real Envoy or its shared-memory IPC.
func stubEnvoy(t *testing.T, recordPath string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"epoch=\"\"; mode=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --mode) mode=\"$2\"; shift 2;;\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"# config pre-validation invocations succeed immediately\n" +
		"[ \"$mode\" = \"validate\" ] && exit 0\n" +
		"echo \"$epoch\" >> \"" + recordPath + "\"\n" +
		"trap 'exit 0' TERM INT\n" +
		"while true; do sleep 0.05; done\n"

	path := filepath.Join(t.TempDir(), "stub-envoy.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func recordedEpochs(t *testing.T, recordPath string) []string {
	t.Helper()
	b, err := os.ReadFile(recordPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestSupervisorConfigChangeTriggersHotRestart drives the full Strategy-A path:
// start epoch 0, change the watched bootstrap config, and observe epoch 1 spawned,
// then a clean shutdown on context cancellation.
func TestSupervisorConfigChangeTriggersHotRestart(t *testing.T) {
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
		BaseID:             0,
		DrainTime:          1 * time.Second,
		ParentShutdownTime: 1 * time.Second,
		WatchConfig:        true,
	}, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// Epoch 0 should come up.
	require.Eventually(t, func() bool {
		return len(recordedEpochs(t, recordPath)) >= 1
	}, 5*time.Second, 50*time.Millisecond, "epoch 0 never started")

	// Change the bootstrap config -> watcher fires -> debounced hot restart -> epoch 1.
	require.NoError(t, os.WriteFile(configPath, []byte("v1\n"), 0o644))
	require.Eventually(t, func() bool {
		epochs := recordedEpochs(t, recordPath)
		return len(epochs) >= 2 && epochs[0] == "0" && epochs[1] == "1"
	}, 5*time.Second, 50*time.Millisecond, "config change did not trigger epoch 1")

	// Cancellation drains and returns nil.
	cancel()
	select {
	case err := <-runErr:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after context cancel")
	}
}

// TestHotRestartDeferredWhileEpochNotLive is the R7 regression test
// (docs/proposals/002): a hot-restart trigger must be deferred while the current
// epoch is still initializing — forking N+1 against a not-yet-LIVE N makes Envoy
// exit fatally and restarts the container.
func TestHotRestartDeferredWhileEpochNotLive(t *testing.T) {
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
		WatchConfig:        true,
		// Admin reports the current epoch as still initializing.
		AdminAddress: fakeAdmin(t, "PRE_INITIALIZING", 0),
	}, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	require.Eventually(t, func() bool {
		return len(recordedEpochs(t, recordPath)) >= 1
	}, 5*time.Second, 50*time.Millisecond, "epoch 0 never started")

	// Trigger a restart; with the epoch not LIVE it must be deferred, not forked.
	require.NoError(t, os.WriteFile(configPath, []byte("v1\n"), 0o644))
	require.Never(t, func() bool {
		return len(recordedEpochs(t, recordPath)) >= 2
	}, 2*time.Second, 100*time.Millisecond, "hot restart must be deferred while current epoch is not LIVE")

	cancel()
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after cancel")
	}
}

// failingThenServingEnvoy writes a stub that records each invocation's epoch and
// exits 1 immediately while blockerPath exists (simulating the base-id domain
// socket held by a live predecessor), then behaves like stubEnvoy once the
// blocker is gone.
func failingThenServingEnvoy(t *testing.T, recordPath, blockerPath string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"epoch=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"echo \"$epoch\" >> \"" + recordPath + "\"\n" +
		"if [ -e \"" + blockerPath + "\" ]; then\n" +
		"  echo 'unable to bind domain socket with base_id=0, id=0, errno=98' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"trap 'exit 0' TERM INT\n" +
		"while true; do sleep 0.05; done\n"

	path := filepath.Join(t.TempDir(), "stub-envoy-collide.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestBindCollisionRetriedInProcess is the regression test for the worker-04
// EADDRINUSE crash-loop (e2e 2026-06-11): a stale heartbeat under node load made
// initStartEpoch pick epoch 0 while the predecessor's Envoy still held the
// base-id domain socket; the fast non-LIVE exit must be retried in-process (the
// predecessor keeps serving meanwhile) instead of failing the supervisor into
// the kubelet's CrashLoopBackOff.
func TestBindCollisionRetriedInProcess(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	blockerPath := filepath.Join(t.TempDir(), "socket-held")
	require.NoError(t, os.WriteFile(blockerPath, []byte("x"), 0o644))

	s := New(Config{
		EnvoyPath:          failingThenServingEnvoy(t, recordPath, blockerPath),
		ConfigPath:         configPath,
		DrainTime:          1 * time.Second,
		ParentShutdownTime: 1 * time.Second,
		StateDir:           t.TempDir(), // cross-pod mode; no state file = stale/absent heartbeat
	}, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// At least two attempts must be made (initial + ≥1 in-process retry) without
	// Run returning. Retries are paced by bindCollisionRetryPause (3s).
	require.Eventually(t, func() bool {
		return len(recordedEpochs(t, recordPath)) >= 2
	}, 15*time.Second, 100*time.Millisecond, "bind collision was not retried in-process")
	select {
	case err := <-runErr:
		t.Fatalf("supervisor exited during bind-collision retries: %v", err)
	default:
	}

	// Predecessor exits (socket released): the next retry must come up and stay up.
	require.NoError(t, os.Remove(blockerPath))
	require.Eventually(t, func() bool {
		launched, _ := s.epochProgress()
		return s.childTracked(0) && time.Since(launched) > bindCollisionWindow
	}, 20*time.Second, 100*time.Millisecond, "envoy did not come up after the socket was released")
	select {
	case err := <-runErr:
		t.Fatalf("supervisor exited after successful retry: %v", err)
	default:
	}

	// All attempts were epoch 0 (no climbing against a dead parent).
	for _, e := range recordedEpochs(t, recordPath) {
		assert.Equal(t, "0", e)
	}

	// Tear down via the clean successor-termination path: SIGTERM the stub (it
	// exits 0, which cross-pod mode treats as protocol termination and awaits
	// pod deletion), then cancel. Cancelling with a live child would instead
	// enter awaitProtocolTermination, which deliberately never returns without
	// an admin endpoint.
	s.signalEpoch(0, syscall.SIGTERM)
	require.Eventually(t, func() bool { return !s.childTracked(0) },
		10*time.Second, 50*time.Millisecond, "stub did not exit on SIGTERM")
	cancel()
	select {
	case err := <-runErr:
		assert.NoError(t, err, "Run must return nil on context cancel")
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not return after context cancel")
	}
}

// validatingStubEnvoy behaves like stubEnvoy but also implements
// `--mode validate`: it exits 1 if the config file contains "poison",
// 0 otherwise (mirroring envoy's bootstrap-init validation).
func validatingStubEnvoy(t *testing.T, recordPath string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"mode=\"\"; epoch=\"\"; cfg=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --mode) mode=\"$2\"; shift 2;;\n" +
		"    -c) cfg=\"$2\"; shift 2;;\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ \"$mode\" = \"validate\" ]; then\n" +
		"  grep -q poison \"$cfg\" && { echo 'config poison detected' >&2; exit 1; }\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"$epoch\" >> \"" + recordPath + "\"\n" +
		"trap 'exit 0' TERM INT\n" +
		"while true; do sleep 0.05; done\n"

	path := filepath.Join(t.TempDir(), "stub-envoy-validate.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestPoisonedConfigDoesNotHotRestart is the regression test for the rev-64
// outage (2026-06-11): a bootstrap config that fails node-local validation
// must NOT be hot-restarted into — the current epoch keeps serving — and a
// subsequent good config must restart normally.
func TestPoisonedConfigDoesNotHotRestart(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")

	s := New(Config{
		EnvoyPath:          validatingStubEnvoy(t, recordPath),
		ConfigPath:         configPath,
		DrainTime:          1 * time.Second,
		ParentShutdownTime: 1 * time.Second,
		WatchConfig:        true,
	}, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	require.Eventually(t, func() bool {
		return len(recordedEpochs(t, recordPath)) >= 1
	}, 5*time.Second, 50*time.Millisecond, "epoch 0 never started")

	// Poisoned config: watcher fires, validation rejects, NO epoch 1.
	require.NoError(t, os.WriteFile(configPath, []byte("v1 poison\n"), 0o644))
	time.Sleep(2 * time.Second) // debounce (500ms) + headroom for a wrong restart to appear
	assert.Len(t, recordedEpochs(t, recordPath), 1, "poisoned config must not be hot-restarted into")
	select {
	case err := <-runErr:
		t.Fatalf("supervisor exited on poisoned config: %v", err)
	default:
	}

	// Good config afterwards: validated, epoch 1 spawns.
	require.NoError(t, os.WriteFile(configPath, []byte("v2 fixed\n"), 0o644))
	require.Eventually(t, func() bool {
		epochs := recordedEpochs(t, recordPath)
		return len(epochs) == 2 && epochs[1] == "1"
	}, 5*time.Second, 50*time.Millisecond, "good config after a rejected one did not hot restart")

	cancel()
	select {
	case err := <-runErr:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after cancel")
	}
}
