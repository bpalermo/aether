package install

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
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

// TestCopyBinaries_InstallSemantics pins what an install leaves on the host,
// independent of which atomic-write helper does it: the source's exact mode,
// the install owner, the source's bytes, nothing else in the target directory,
// and a replacement by rename (a new inode; the old one is never written in
// place, so a runtime that is exec'ing the previous plugin keeps a whole file).
func TestCopyBinaries_InstallSemantics(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o750, 0o500} {
		t.Run(mode.String(), func(t *testing.T) {
			in, src, dst := newCopyFixture(t, "plugin-v2", mode)
			target := filepath.Join(dst, "aether-cni")
			require.NoError(t, os.WriteFile(target, []byte("plugin-v1"), 0o644))
			old, err := os.Open(target)
			require.NoError(t, err)
			t.Cleanup(func() { _ = old.Close() })
			oldInfo, err := old.Stat()
			require.NoError(t, err)

			_, err = in.copyBinaries(src, dst)
			require.NoError(t, err)

			info, err := os.Stat(target)
			require.NoError(t, err)
			assert.Equal(t, mode, info.Mode(), "the installed file carries the source's mode")
			st, ok := info.Sys().(*syscall.Stat_t)
			require.True(t, ok)
			assert.Equal(t, os.Getuid(), int(st.Uid))
			assert.Equal(t, os.Getgid(), int(st.Gid))
			got, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, "plugin-v2", string(got))

			assert.False(t, os.SameFile(oldInfo, info), "the install replaces the directory entry; it does not write the old file in place")
			stale, err := io.ReadAll(old)
			require.NoError(t, err)
			assert.Equal(t, "plugin-v1", string(stale), "a holder of the previous file still reads all of it")

			entries, err := os.ReadDir(dst)
			require.NoError(t, err)
			require.Len(t, entries, 1, "no temporary file is left beside the plugin")
			assert.Equal(t, "aether-cni", entries[0].Name())
		})
	}
}

// TestCopyBinaries_OwnershipFailureLeavesThePreviousInstall: the plugin is
// published only once it has its owner. An unprivileged test cannot chown to
// root, which is exactly the failure wanted here: the install reports it, the
// previous plugin is untouched and no temporary file is left behind.
func TestCopyBinaries_OwnershipFailureLeavesThePreviousInstall(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can chown to root; the failure cannot be provoked")
	}
	in, src, dst := newCopyFixture(t, "plugin-v2", 0o755)
	binOwner.uid, binOwner.gid = 0, 0 // newCopyFixture's cleanup restores it
	target := filepath.Join(dst, "aether-cni")
	require.NoError(t, os.WriteFile(target, []byte("plugin-v1"), 0o755))

	_, err := in.copyBinaries(src, dst)
	// EPERM on a host, EINVAL in a user namespace that does not map uid 0.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chown")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "plugin-v1", string(got))
	entries, err := os.ReadDir(dst)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the failed install's temporary file is removed")
}
