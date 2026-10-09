package hotrestart

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configReadingStub is stubEnvoy that records "<--restart-epoch> <bootstrap
// content>" per serving invocation: which generation started, and which
// version of the bootstrap it read when it did.
func configReadingStub(t *testing.T, recordPath string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"epoch=\"\"; mode=\"\"; config=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --mode) mode=\"$2\"; shift 2;;\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    -c) config=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"[ \"$mode\" = \"validate\" ] && exit 0\n" +
		"echo \"$epoch $(cat \"$config\")\" >> \"" + recordPath + "\"\n" +
		"trap 'exit 0' TERM INT\n" +
		"while true; do sleep 0.05; done\n"

	path := filepath.Join(t.TempDir(), "stub-envoy.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// onRunGoroutine reports whether the caller is running on the goroutine that
// called Supervisor.Run, by looking for Run among its own stack frames. A
// goroutine Run started has Run as its creator, not as a frame.
func onRunGoroutine() bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".(*Supervisor).Run") {
			return true
		}
		if !more {
			return false
		}
	}
}

// runSupervisor starts s.Run and registers the cleanup that cancels it and
// waits for it, so no test leaves a supervisor or a stub child behind.
func runSupervisor(t *testing.T, s *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not return after context cancel")
		}
	})
}

// TestConfigChangeRightAfterTheFirstForkIsNotLost is issue #1470. The
// supervisor used to arm its bootstrap watch on a goroutine of its own and
// fork epoch 0 without waiting for it, so a bootstrap rewritten after epoch 0
// had started and before the watch was armed produced no event: Envoy kept
// the old bootstrap until the next change.
//
// The test forces that interleaving instead of waiting for the scheduler to
// produce it. The watcher is created through a hook; when the supervisor arms
// the watch off the goroutine that forks, the hook holds the arming until
// epoch 0 has started and the bootstrap has been rewritten, which is the
// order that loses the change. A supervisor that arms the watch on the
// forking goroutine, before the fork, cannot be put in that order at all, and
// for it the bootstrap is rewritten as soon as epoch 0 has started. Either
// way the newest Envoy must end up started from the rewritten bootstrap.
func TestConfigChangeRightAfterTheFirstForkIsNotLost(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	configPath := filepath.Join(t.TempDir(), "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	epochZeroStarted := func() bool { return len(recordedEpochs(t, recordPath)) >= 1 }

	s := New(Config{
		EnvoyPath:          configReadingStub(t, recordPath),
		ConfigPath:         configPath,
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		WatchConfig:        true,
	}, slog.New(slog.DiscardHandler), nil)

	var (
		forkedBeforeArmed atomic.Bool
		rewrittenByHook   atomic.Bool
		hookReturned      = make(chan struct{})
	)
	s.newConfigWatcher = func() (*fsnotify.Watcher, error) {
		defer close(hookReturned)
		if !onRunGoroutine() {
			// Armed concurrently with the fork: take the losing order.
			deadline := time.Now().Add(5 * time.Second)
			for !epochZeroStarted() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if err := os.WriteFile(configPath, []byte("v1\n"), 0o644); err != nil {
				return nil, err
			}
			rewrittenByHook.Store(true)
		}
		forkedBeforeArmed.Store(s.anyChildTracked())
		return fsnotify.NewWatcher()
	}

	runSupervisor(t, s)

	require.Eventually(t, epochZeroStarted, 5*time.Second, 10*time.Millisecond, "epoch 0 never started")
	select {
	case <-hookReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the config watcher was never created")
	}
	if !rewrittenByHook.Load() {
		require.NoError(t, os.WriteFile(configPath, []byte("v1\n"), 0o644))
	}

	assert.False(t, forkedBeforeArmed.Load(),
		"epoch 0 was forked before the bootstrap watch was armed: a bootstrap rewritten in between is lost")
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) >= 2 },
		5*time.Second, 20*time.Millisecond,
		"the bootstrap was rewritten after epoch 0 started and no hot restart followed")
	assert.Equal(t, []string{"0 v0", "1 v1"}, recordedEpochs(t, recordPath))
}

// TestSupervisorRunsWithoutAConfigWatch: a watch that cannot be set up is
// logged and given up on, and the supervisor still starts and supervises
// Envoy. It only loses the self-triggered hot restart; SIGHUP still works.
func TestSupervisorRunsWithoutAConfigWatch(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	for name, tc := range map[string]struct {
		configDir  func(t *testing.T) string
		newWatcher func() (*fsnotify.Watcher, error)
	}{
		"the watcher cannot be created": {
			configDir:  func(t *testing.T) string { return t.TempDir() },
			newWatcher: func() (*fsnotify.Watcher, error) { return nil, errors.New("too many open files") },
		},
		"the bootstrap directory cannot be watched": {
			configDir:  func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			newWatcher: fsnotify.NewWatcher,
		},
	} {
		t.Run(name, func(t *testing.T) {
			recordPath := filepath.Join(t.TempDir(), "epochs.txt")
			s := New(Config{
				EnvoyPath:          stubEnvoy(t, recordPath),
				ConfigPath:         filepath.Join(tc.configDir(t), "envoy.yaml"),
				DrainTime:          time.Second,
				ParentShutdownTime: time.Second,
				WatchConfig:        true,
			}, slog.New(slog.DiscardHandler), nil)
			s.newConfigWatcher = tc.newWatcher

			runSupervisor(t, s)

			require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) >= 1 },
				5*time.Second, 10*time.Millisecond, "epoch 0 never started")
			assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath))
		})
	}
}
