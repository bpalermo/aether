package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"aethermesh.dev/agent/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subprocessEnv makes this test binary behave as agent-ready itself, so the
// exit-code assertions exercise the real main() (including os.Exit(1)).
const subprocessEnv = "AETHER_AGENT_READY_SUBPROCESS_ARGS"

func TestMain(m *testing.M) {
	if socket, ok := os.LookupEnv(subprocessEnv); ok {
		os.Args = []string{"agent-ready", "--socket=" + socket}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestDefaultSocketMatchesTheAgent pins the probe's default to the path the
// agent serves on and the chart passes. A drift makes every agent pod
// permanently NotReady (and, on the liveness probe, crash-looped).
func TestDefaultSocketMatchesTheAgent(t *testing.T) {
	assert.Equal(t, constants.DefaultAgentHealthSocketPath, defaultSocket)
}

// healthServer serves /healthz (always 200) and /readyz (status ready) on a
// Unix socket, as the agent does.
func healthServer(t *testing.T, ready int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ar")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(ready)
		if r.URL.Query().Has("verbose") {
			fmt.Fprint(w, "[-]standby failed: first snapshot not built yet\nreadyz check failed")
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestRun(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		assert.NoError(t, run([]string{"--socket=" + healthServer(t, http.StatusOK)}))
	})
	t.Run("not ready names the failing check", func(t *testing.T) {
		err := run([]string{"--socket=" + healthServer(t, http.StatusInternalServerError)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
		assert.Contains(t, err.Error(), "standby failed")
	})
	t.Run("liveness path", func(t *testing.T) {
		assert.NoError(t, run([]string{"--socket=" + healthServer(t, http.StatusInternalServerError), "--path=/healthz"}))
	})
	t.Run("no socket", func(t *testing.T) {
		assert.Error(t, run([]string{"--socket=" + filepath.Join(t.TempDir(), "absent.sock")}))
	})
	t.Run("a server that never answers is bounded", func(t *testing.T) {
		dir, err := os.MkdirTemp("", "ar")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			c, err := ln.Accept()
			if err == nil {
				t.Cleanup(func() { _ = c.Close() })
			}
		}()
		started := time.Now()
		assert.Error(t, run([]string{"--socket=" + filepath.Join(dir, "s.sock"), "--timeout=200ms"}))
		assert.Less(t, time.Since(started), 2*time.Second)
	})
	t.Run("unknown flag", func(t *testing.T) {
		assert.Error(t, run([]string{"--nope"}))
	})
}

// TestExitCodes is the contract the kubelet reads: 0 = Ready, non-zero = not.
func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		socket   string
		wantExit int
	}{
		{"ready exits 0", healthServer(t, http.StatusOK), 0},
		{"not ready exits 1", healthServer(t, http.StatusServiceUnavailable), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), subprocessEnv+"="+tc.socket)
			out, err := cmd.CombinedOutput()
			if tc.wantExit == 0 {
				require.NoError(t, err, "output: %s", out)
				assert.Empty(t, string(out), "a ready probe must say nothing")
				return
			}
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			assert.Equal(t, tc.wantExit, exitErr.ExitCode())
			assert.Contains(t, string(out), "not ready")
		})
	}
}
