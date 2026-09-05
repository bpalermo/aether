package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subprocessEnv makes this test binary behave as mesh-dns-ready itself, so the
// exit-code assertions below exercise the real main() (including os.Exit(1))
// rather than a stand-in.
const subprocessEnv = "AETHER_MESH_DNS_READY_SUBPROCESS_MARKER"

func TestMain(m *testing.M) {
	if marker, ok := os.LookupEnv(subprocessEnv); ok {
		os.Args = []string{"mesh-dns-ready", "--ready-marker=" + marker}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestDefaultReadyMarkerMatchesChart pins the default to the path the chart
// mounts the per-pod ready-marker emptyDir at and the mesh-dns daemon's own
// --ready-marker default (agent/cmd/mesh-dns/main.go). A drift here makes every
// mesh-dns pod permanently NotReady if the chart ever stops passing the flag
// explicitly — which on this DaemonSet also wedges the surge rollout, since
// maxUnavailable is 0 and the successor never becomes Ready.
func TestDefaultReadyMarkerMatchesChart(t *testing.T) {
	assert.Equal(t, "/run/aether/mesh-dns.ready", defaultReadyMarker)
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "mesh-dns.ready")
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
// silently mark every mesh-dns pod NotReady, so assert on the real process.
func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "mesh-dns.ready")
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
