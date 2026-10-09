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

// restartEpochEntries returns the values of every RestartEpochEnv entry in an
// environment block. Exactly one is the contract: with two, which one a
// process reads is up to its libc.
func restartEpochEntries(env []string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, RestartEpochEnv+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestChildEnv(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		epoch   int
		want    []string
	}{
		{
			name:    "adds the epoch to an environment that has none",
			environ: []string{"PATH=/usr/bin", "POD_NAME=aether-proxy-x"},
			epoch:   0,
			want:    []string{"PATH=/usr/bin", "POD_NAME=aether-proxy-x", "AETHER_RESTART_EPOCH=0"},
		},
		{
			name:    "a stale inherited value cannot win",
			environ: []string{"AETHER_RESTART_EPOCH=99", "PATH=/usr/bin"},
			epoch:   4,
			want:    []string{"PATH=/usr/bin", "AETHER_RESTART_EPOCH=4"},
		},
		{
			name:    "every stale entry is dropped, including an empty one",
			environ: []string{"AETHER_RESTART_EPOCH=", "A=1", "AETHER_RESTART_EPOCH=7", "B=2"},
			epoch:   1,
			want:    []string{"A=1", "B=2", "AETHER_RESTART_EPOCH=1"},
		},
		{
			name:    "a variable that only shares the prefix is kept",
			environ: []string{"AETHER_RESTART_EPOCH_HINT=5", "AETHER_RESTART=1"},
			epoch:   2,
			want:    []string{"AETHER_RESTART_EPOCH_HINT=5", "AETHER_RESTART=1", "AETHER_RESTART_EPOCH=2"},
		},
		{
			name:    "an empty environment still carries the epoch",
			environ: nil,
			epoch:   3,
			want:    []string{"AETHER_RESTART_EPOCH=3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.environ...)
			assert.Equal(t, tt.want, childEnv(tt.environ, tt.epoch))
			assert.Equal(t, in, tt.environ, "the caller's slice must not be modified")
		})
	}
}

// TestBuildEnvoyCmdExportsRestartEpoch pins the two halves of the contract on
// the command the supervisor actually starts: the child keeps the supervisor's
// environment (ordinary inheritance: nothing the supervisor was started with is
// dropped), and it carries its own --restart-epoch as AETHER_RESTART_EPOCH even when the
// supervisor inherited another value (issue #1333).
func TestBuildEnvoyCmdExportsRestartEpoch(t *testing.T) {
	t.Setenv(RestartEpochEnv, "99")
	t.Setenv("AETHER_TEST_INHERITED", "kept")

	s := New(Config{EnvoyPath: "/usr/local/bin/envoy", ConfigPath: "/etc/envoy/envoy.yaml"},
		slog.New(slog.DiscardHandler), nil)

	for _, epoch := range []int{0, 3, 12} {
		cmd := s.buildEnvoyCmd(epoch)
		require.NotNil(t, cmd.Env, "a nil Env would inherit the supervisor's stale value")
		assert.Equal(t, []string{cmd.Args[indexOf(cmd.Args, "--restart-epoch")+1]}, restartEpochEntries(cmd.Env),
			"the exported epoch must be the --restart-epoch of this child, exactly once")
		assert.Contains(t, cmd.Env, "AETHER_TEST_INHERITED=kept", "the inherited environment must be kept")
		assert.Len(t, cmd.Env, len(os.Environ()), "one variable overridden, none dropped, none added")
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// epochEnvStub is stubEnvoy that records "<--restart-epoch> <AETHER_RESTART_EPOCH>"
// per invocation, as the process itself sees them.
func epochEnvStub(t *testing.T, recordPath string) string {
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
		"[ \"$mode\" = \"validate\" ] && exit 0\n" +
		"echo \"$epoch ${AETHER_RESTART_EPOCH-unset}\" >> \"" + recordPath + "\"\n" +
		"trap 'exit 0' TERM INT\n" +
		"while true; do sleep 0.05; done\n"

	path := filepath.Join(t.TempDir(), "stub-envoy.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestEnvoyChildSeesItsRestartEpoch runs real child processes through each way
// the supervisor starts one and reads back what the child saw: the first start
// (epoch 0), an in-pod hot restart (epoch 1), and a cross-pod handoff that
// attaches one above the predecessor's epoch. The supervisor's own environment
// carries a stale value throughout.
func TestEnvoyChildSeesItsRestartEpoch(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}
	t.Setenv(RestartEpochEnv, "99")

	t.Run("first start and in-pod hot restart", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "envoy.yaml")
		require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
		recordPath := filepath.Join(t.TempDir(), "epochs.txt")

		s := New(Config{
			EnvoyPath:          epochEnvStub(t, recordPath),
			ConfigPath:         configPath,
			DrainTime:          time.Second,
			ParentShutdownTime: time.Second,
			WatchConfig:        true,
		}, slog.New(slog.DiscardHandler), nil)

		ctx, cancel := context.WithCancel(context.Background())
		runErr := make(chan error, 1)
		go func() { runErr <- s.Run(ctx) }()
		// Registered before the first assertion that can abort the subtest, so
		// a failure above never leaves the supervisor or its stub children
		// running: cancellation is what makes Run stop and reap them.
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-runErr:
				assert.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Error("supervisor did not return after context cancel")
			}
		})

		require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) >= 1 },
			5*time.Second, 50*time.Millisecond, "epoch 0 never started")
		// No pause is needed before this write. The supervisor arms its
		// bootstrap watch before it forks epoch 0 (#1470), so a write made
		// once epoch 0 has recorded itself is always reported. When the watch
		// was armed on a goroutine of its own, this write could land first and
		// be lost, and the wait below then failed after its full 5 s.
		// TestConfigChangeRightAfterTheFirstForkIsNotLost forces that order.
		require.NoError(t, os.WriteFile(configPath, []byte("v1\n"), 0o644))
		require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) >= 2 },
			5*time.Second, 50*time.Millisecond, "config change did not trigger epoch 1")

		assert.Equal(t, []string{"0 0", "1 1"}, recordedEpochs(t, recordPath))
	})

	t.Run("cross-pod handoff attaches at predecessor+1", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "epochs.txt")
		s := New(Config{
			EnvoyPath:          epochEnvStub(t, recordPath),
			ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
			DrainTime:          time.Second,
			ParentShutdownTime: time.Second,
			StateDir:           t.TempDir(),
			AdminAddress:       fakeAdmin(t, "LIVE", 6),
		}, slog.New(slog.DiscardHandler), nil)

		// A live predecessor pod at epoch 6: this pod's first Envoy is epoch 7.
		writeRawState(t, s, 6, 0)
		s.initStartEpoch(context.Background())
		require.NoError(t, s.hotRestart())
		t.Cleanup(func() { s.signalEpoch(7, syscall.SIGKILL) })

		require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) >= 1 },
			5*time.Second, 20*time.Millisecond, "the successor never started")
		assert.Equal(t, []string{"7 7"}, recordedEpochs(t, recordPath))
	})
}
