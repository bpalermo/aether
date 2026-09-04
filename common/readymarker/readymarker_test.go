package readymarker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckMatchesStat pins Check to os.Stat byte for byte. #673 moved the
// readiness predicate out of the agent binary into a stdlib-only reader; the
// only thing that must NOT change in that move is the answer, so every case
// asserts Check agrees with the os.Stat the supervisor's --readiness-check used
// to do inline.
func TestCheckMatchesStat(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(regular, []byte("ready\n"), 0o644))

	missing := filepath.Join(dir, "absent")

	subdir := filepath.Join(dir, "adir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	danglingLink := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), danglingLink))

	goodLink := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(regular, goodLink))

	// A file under a directory with no search permission: os.Stat fails with
	// EACCES, not ENOENT — Check must still report not-ready rather than
	// treating a non-ENOENT error as success.
	lockedParent := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(lockedParent, 0o755))
	unreadable := filepath.Join(lockedParent, "ready")
	require.NoError(t, os.WriteFile(unreadable, []byte("ready\n"), 0o644))
	require.NoError(t, os.Chmod(lockedParent, 0o000))
	t.Cleanup(func() { _ = os.Chmod(lockedParent, 0o755) })

	tests := []struct {
		name      string
		path      string
		wantReady bool
	}{
		{name: "regular file is ready", path: regular, wantReady: true},
		{name: "missing file is not ready", path: missing, wantReady: false},
		{name: "directory is ready (os.Stat semantics)", path: subdir, wantReady: true},
		{name: "dangling symlink is not ready", path: danglingLink, wantReady: false},
		{name: "symlink to a file is ready", path: goodLink, wantReady: true},
		{name: "empty path is not ready", path: "", wantReady: false},
	}

	// Running as root defeats the permission case (root bypasses the search
	// bit), so only assert it for an unprivileged test process.
	if os.Geteuid() != 0 {
		tests = append(tests, struct {
			name      string
			path      string
			wantReady bool
		}{name: "unreadable parent dir is not ready", path: unreadable, wantReady: false})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, statErr := os.Stat(tc.path)
			err := Check(tc.path)

			assert.Equal(t, statErr == nil, err == nil,
				"Check must agree with os.Stat (stat err: %v, check err: %v)", statErr, err)
			if tc.wantReady {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Error(t, statErr)
			assert.Contains(t, err.Error(), "not ready")
			assert.Contains(t, err.Error(), statErr.Error(),
				"Check must wrap the underlying stat error verbatim")
		})
	}
}
