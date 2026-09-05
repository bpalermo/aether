package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallFile(t *testing.T) {
	t.Run("copies contents and makes the result executable", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "src")
		require.NoError(t, os.WriteFile(source, []byte("payload"), 0o600))
		dest := filepath.Join(dir, "dest")

		require.NoError(t, installFile(source, dest))

		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(got))

		info, err := os.Stat(dest)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(),
			"the runtime container execs this; it must be installed executable")

		// The copy goes through a .tmp + rename, which must leave nothing behind.
		_, err = os.Stat(dest + ".tmp")
		assert.True(t, os.IsNotExist(err), "the staging file must be renamed away")
	})

	t.Run("a missing source is an error, not a silent no-op", func(t *testing.T) {
		dir := t.TempDir()
		err := installFile(filepath.Join(dir, "absent"), filepath.Join(dir, "dest"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absent")
	})
}

func TestRunInstall(t *testing.T) {
	t.Run("no destinations is a no-op", func(t *testing.T) {
		assert.NoError(t, runInstall("", ""))
	})

	t.Run("installs the supervisor from /proc/self/exe", func(t *testing.T) {
		if _, err := os.Stat("/proc/self/exe"); err != nil {
			t.Skip("/proc/self/exe not available in this sandbox")
		}
		dest := filepath.Join(t.TempDir(), "supervisor")
		require.NoError(t, runInstall(dest, ""))

		info, err := os.Stat(dest)
		require.NoError(t, err)
		assert.Positive(t, info.Size())
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	})

	// #673: a chart asking for the readiness prober against an agent image that
	// predates it must fail the initContainer loudly. The alternative — skipping
	// the copy — starts a pod whose readiness probe can never succeed, which
	// under maxUnavailable:0 wedges the rollout with no explanation.
	t.Run("a missing readiness prober hard-fails", func(t *testing.T) {
		if _, err := os.Stat(readinessBinarySource); err == nil {
			t.Skipf("%s exists in this environment; cannot exercise the skew path", readinessBinarySource)
		}
		err := runInstall("", filepath.Join(t.TempDir(), "proxy-ready"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), readinessBinarySource)
	})
}
