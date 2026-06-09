package hotrestart

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
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
	}, logr.Discard())

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
		"epoch=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
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
	}, logr.Discard())

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
