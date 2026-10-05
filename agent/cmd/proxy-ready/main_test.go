package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subprocessEnv makes this test binary behave as proxy-ready itself, so the
// exit-code assertions below exercise the real main() (including os.Exit(1))
// rather than a stand-in.
const subprocessEnv = "AETHER_PROXY_READY_SUBPROCESS_MARKER"

func TestMain(m *testing.M) {
	if marker, ok := os.LookupEnv(subprocessEnv); ok {
		os.Args = []string{"proxy-ready", "--ready-marker=" + marker}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestDefaultReadyMarkerMatchesChart pins the default to the path the chart
// mounts the per-pod ready-marker emptyDir at and the supervisor's own
// --ready-marker default (agent/internal/cmd/supervisor.go). A drift here makes
// every proxy pod permanently NotReady if the chart ever stops passing the flag
// explicitly.
func TestDefaultReadyMarkerMatchesChart(t *testing.T) {
	assert.Equal(t, "/var/run/aether-proxy/ready", defaultReadyMarker)
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(present, []byte("ready\n"), 0o644))
	absent := filepath.Join(dir, "absent")

	t.Run("marker present", func(t *testing.T) {
		assert.NoError(t, run([]string{"--ready-marker=" + present}))
	})
	t.Run("marker absent", func(t *testing.T) {
		assert.Error(t, run([]string{"--ready-marker=" + absent}))
	})
	t.Run("unknown flag", func(t *testing.T) {
		assert.Error(t, run([]string{"--nope"}))
	})
}

// TestExitCodes is the contract the kubelet actually reads: 0 = Ready, non-zero
// = not Ready. Anything else (a panic, a usage dump on stdout, a hang) would
// silently mark every proxy pod NotReady, so assert on the real process.
func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(present, []byte("ready\n"), 0o644))

	tests := []struct {
		name     string
		marker   string
		wantExit int
	}{
		{name: "present exits 0", marker: present, wantExit: 0},
		{name: "absent exits 1", marker: filepath.Join(dir, "absent"), wantExit: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), subprocessEnv+"="+tc.marker)
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

// TestUnixSocket is the ext_authz sidecar's startupProbe contract (#1275): ready
// iff something is ACCEPTING on the socket. A socket file whose listener is gone
// is exactly what a restarted sidecar finds in its emptyDir, and it must read as
// not ready, or the kubelet would start the proxy container against a dead
// authz socket — the 403s under failureMode DENY this probe exists to prevent.
func TestUnixSocket(t *testing.T) {
	// Short path: the AF_UNIX budget is 107 bytes and Bazel's TEST_TMPDIR alone
	// can exceed it.
	dir, err := os.MkdirTemp("", "pr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	stale := filepath.Join(dir, "stale.sock")
	staleLn, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	require.NoError(t, err)
	// Keep the file, drop the listener: a crashed sidecar's leftover.
	staleLn.SetUnlinkOnClose(false)
	require.NoError(t, staleLn.Close())
	_, err = os.Stat(stale)
	require.NoError(t, err, "control: the stale socket file must still exist")

	// A regular file is not a socket either.
	regular := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(regular, nil, 0o644))

	t.Run("listening", func(t *testing.T) {
		assert.NoError(t, run([]string{"--unix-socket=" + live}))
	})
	t.Run("stale socket file", func(t *testing.T) {
		assert.Error(t, run([]string{"--unix-socket=" + stale}))
	})
	t.Run("missing", func(t *testing.T) {
		assert.Error(t, run([]string{"--unix-socket=" + filepath.Join(dir, "absent.sock")}))
	})
	t.Run("regular file", func(t *testing.T) {
		assert.Error(t, run([]string{"--unix-socket=" + regular}))
	})
	t.Run("socket mode ignores the marker", func(t *testing.T) {
		// A present marker must not rescue a dead socket: the two modes are
		// independent, and the sidecar's probe passes no marker at all.
		marker := filepath.Join(dir, "ready")
		require.NoError(t, os.WriteFile(marker, []byte("ready\n"), 0o644))
		assert.Error(t, run([]string{"--ready-marker=" + marker, "--unix-socket=" + stale}))
	})
}
