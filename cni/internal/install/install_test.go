package install

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installAsSelf points the install owner at the test's own uid:gid (the
// production owner is root, which an unprivileged test cannot chown to).
func installAsSelf(t *testing.T) {
	t.Helper()
	prev := binOwner
	binOwner.uid, binOwner.gid = os.Getuid(), os.Getgid()
	t.Cleanup(func() { binOwner = prev })
}

func newCopyFixture(t *testing.T, content string, mode os.FileMode) (*Installer, string, string) {
	t.Helper()
	installAsSelf(t)
	src, dst := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "aether-cni"), []byte(content), mode))
	require.NoError(t, os.Chmod(filepath.Join(src, "aether-cni"), mode))
	return NewInstaller(slog.New(slog.DiscardHandler), &InstallerConfig{}), src, dst
}

// TestCopyBinaries_LeavesAnIdenticalInstallAlone is issue #1123's cni-install
// term: this init container runs on every agent pod start, and on a restart the
// plugin already on the host is byte-for-byte the one being installed. The
// rewrite (copy, fsync, rename, directory sync) then only delays the new agent,
// whose node proxy has no ADS stream until it serves. Identity of the file
// (same inode) is the observable: an unchanged install is not replaced.
func TestCopyBinaries_LeavesAnIdenticalInstallAlone(t *testing.T) {
	in, src, dst := newCopyFixture(t, "plugin-v1", 0o755)

	_, err := in.copyBinaries(src, dst)
	require.NoError(t, err)
	before, err := os.Stat(filepath.Join(dst, "aether-cni"))
	require.NoError(t, err)

	files, err := in.copyBinaries(src, dst)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dst, "aether-cni")}, files, "an unchanged binary is still reported as installed")
	after, err := os.Stat(filepath.Join(dst, "aether-cni"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "an identical install must not be rewritten")
}

// TestCopyBinaries_ReplacesAnyDifference is the safety half: anything that
// differs (content of the same length, permission bits, a missing file) is
// installed exactly as before, atomically.
func TestCopyBinaries_ReplacesAnyDifference(t *testing.T) {
	t.Run("content of the same size", func(t *testing.T) {
		in, src, dst := newCopyFixture(t, "plugin-v1", 0o755)
		_, err := in.copyBinaries(src, dst)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(src, "aether-cni"), []byte("plugin-v2"), 0o755))
		_, err = in.copyBinaries(src, dst)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dst, "aether-cni"))
		require.NoError(t, err)
		assert.Equal(t, "plugin-v2", string(got))
	})

	t.Run("permission bits", func(t *testing.T) {
		in, src, dst := newCopyFixture(t, "plugin-v1", 0o755)
		_, err := in.copyBinaries(src, dst)
		require.NoError(t, err)

		require.NoError(t, os.Chmod(filepath.Join(dst, "aether-cni"), 0o644))
		_, err = in.copyBinaries(src, dst)
		require.NoError(t, err)
		info, err := os.Stat(filepath.Join(dst, "aether-cni"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	})

	t.Run("a truncated install", func(t *testing.T) {
		in, src, dst := newCopyFixture(t, "plugin-v1-with-a-longer-body", 0o755)
		require.NoError(t, os.WriteFile(filepath.Join(dst, "aether-cni"), []byte("plugin-v1"), 0o755))
		_, err := in.copyBinaries(src, dst)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dst, "aether-cni"))
		require.NoError(t, err)
		assert.Equal(t, "plugin-v1-with-a-longer-body", string(got))
	})

	t.Run("absent", func(t *testing.T) {
		in, src, dst := newCopyFixture(t, "plugin-v1", 0o755)
		_, err := in.copyBinaries(src, dst)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dst, "aether-cni"))
		require.NoError(t, err)
		assert.Equal(t, "plugin-v1", string(got))
	})
}
