package supervisorcmd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dirEntryNames lists the names directly under dir.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

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

		// The copy stages through a temp file and renames, which must leave
		// nothing behind — under any name, not just the old fixed dest+".tmp".
		assert.ElementsMatch(t, []string{"src", "dest"}, dirEntryNames(t, dir),
			"the staging file must be renamed away")
	})

	// S24: the staging file used to be a FIXED dest+".tmp", so two writers
	// aiming at the same destination interleaved into one temp file and the
	// rename could publish half of each — and what is published here is an
	// executable another container execs as its entrypoint. Every concurrent
	// install must now either fail or produce the whole binary, never a splice.
	t.Run("concurrent installs never publish a partial file", func(t *testing.T) {
		dir := t.TempDir()
		payload := strings.Repeat("aether-supervisor-payload\n", 4096)
		source := filepath.Join(dir, "src")
		require.NoError(t, os.WriteFile(source, []byte(payload), 0o600))
		dest := filepath.Join(dir, "dest")

		const writers = 8
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- installFile(source, dest)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		assert.Equal(t, payload, string(got), "a concurrent install published a spliced file")
		assert.ElementsMatch(t, []string{"src", "dest"}, dirEntryNames(t, dir),
			"every writer must clean up its own staging file")
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
