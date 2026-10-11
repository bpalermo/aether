// Package file provides atomic file write utilities and platform-specific optimizations.
// Atomic writes ensure that files are either fully written or not modified, preventing corruption
// from partial writes or concurrent access. Large files are marked as not needed to optimize
// page cache behavior on Linux systems.
//
// Atomicity and durability are two different guarantees and this package provides both.
// The rename is what makes a reader see whole-old or whole-new bytes; it is NOT what makes
// the new bytes survive a power cut. Without an fsync of the file before the rename, and of
// the parent directory after it, a crash can leave the destination present but zero-length —
// the failure mode that took out the CNI conflist and the mesh-DNS snapshot fleet-wide on
// 2026-08-29 (#645, four power incidents on record for that cluster). Every write published
// by this package is therefore synced.
package file

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// syncFile is the subset of *os.File the atomic write path uses.
//
// It exists so tests can inject a file whose Sync fails: the point of the durability
// contract is that an unsynced rename is never reported to the caller as a successful
// write, and that is only provable if the fsync can be made to fail on demand.
type syncFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
	// Fd is the descriptor tryMarkLargeFileAsNotNeeded hands to fadvise(2).
	Fd() uintptr
	// Chown is fchown(2) on the temp file, for WithOwner.
	Chown(uid, gid int) error
}

// fileOwner is the uid:gid WithOwner asks for.
type fileOwner struct{ uid, gid int }

// WriteOption adjusts one AtomicWriteReader call.
type WriteOption func(*atomicWriteOpts)

// WithOwner gives the written file the owner uid:gid. The temp file is chowned
// through its descriptor, after the chmod and before any byte is written, so the
// destination name never points at a file with another owner: a failed chown
// fails the write and leaves the destination as it was.
func WithOwner(uid, gid int) WriteOption {
	return func(o *atomicWriteOpts) { o.owner = &fileOwner{uid: uid, gid: gid} }
}

// KeepInPageCache leaves the written file's pages in the page cache. By default
// AtomicWriteReader marks a large file FADV_DONTNEED once it is synced, which is
// right for a file this process only copies; it is wrong for a file that is read
// or exec'd straight afterwards, such as a binary installed onto the host.
func KeepInPageCache() WriteOption {
	return func(o *atomicWriteOpts) { o.fadvise = false }
}

// atomicWriteOpts carries the per-caller knobs plus the two seams (temp-file creation
// and directory fsync) the tests substitute.
type atomicWriteOpts struct {
	// pattern is the os.CreateTemp pattern used for the temp file, which always lives
	// in the destination's own directory so the rename stays within one filesystem.
	pattern string
	// mode, when non-zero, is chmod'ed onto the temp file before it is published.
	// Zero leaves os.CreateTemp's 0600.
	mode os.FileMode
	// fadvise marks large files FADV_DONTNEED after the copy (page-cache hygiene).
	fadvise bool
	// owner, when set, is fchown'ed onto the temp file before it is published.
	owner *fileOwner

	createTemp func(dir, pattern string) (syncFile, error)
	syncDir    func(dir string) error
}

// osCreateTemp is the production createTemp seam. The explicit error return avoids
// handing back a typed-nil *os.File inside a non-nil interface.
func osCreateTemp(dir, pattern string) (syncFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// WriteFileAtomic writes data to a file atomically using a temporary file and rename.
// It ensures that the file is either fully written with the new data or left unchanged.
// The data is fsynced before the rename and the parent directory is fsynced after it, so
// the new contents survive a power cut rather than coming back zero-length.
func WriteFileAtomic(filePath string, data []byte) error {
	return writeAtomic(filePath, bytes.NewReader(data), atomicWriteOpts{
		pattern:    fmt.Sprintf(".%s-*.tmp", filepath.Base(filePath)),
		createTemp: osCreateTemp,
		syncDir:    syncDir,
	})
}

// Exists checks if a file or directory exists at the given path.
// It returns false only if the file does not exist; other errors (such as permission denied) return true.
func Exists(name string) bool {
	// We must explicitly check if the error is due to the file not existing (as opposed to a
	// permissions error).
	_, err := os.Stat(name)
	return !errors.Is(err, fs.ErrNotExist)
}

// AtomicWrite writes data atomically to a file with the specified permissions.
// It uses a temporary file in the same directory and atomically renames it to the target path.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	return AtomicWriteReader(path, bytes.NewReader(data), mode)
}

// AtomicWriteReader writes data from a reader atomically to a file with the specified permissions.
// It uses a temporary file and atomically renames it, marking large files as not needed for cache
// optimization. The temp file is fsynced before the rename and the parent directory after it: this
// path publishes the mesh-DNS record snapshot and the CNI conflist, both of which must survive an
// unclean power loss. It also installs binaries (the proxy supervisor onto its shared volume, the
// CNI plugin onto the host); WithOwner and KeepInPageCache are for those.
func AtomicWriteReader(path string, data io.Reader, mode os.FileMode, options ...WriteOption) error {
	return writeAtomic(path, data, readerOpts(path, mode, options))
}

// readerOpts is AtomicWriteReader's configuration: its defaults, then the caller's options.
func readerOpts(path string, mode os.FileMode, options []WriteOption) atomicWriteOpts {
	opts := atomicWriteOpts{
		pattern:    filepath.Base(path) + ".tmp.",
		mode:       mode,
		fadvise:    true,
		createTemp: osCreateTemp,
		syncDir:    syncDir,
	}
	for _, option := range options {
		option(&opts)
	}
	return opts
}

// setModeAndOwner gives the still-empty temp file the mode and the owner the caller
// asked for, in that order: chown(2) clears the set-id bits of the file it is applied to.
func setModeAndOwner(tmpFile syncFile, opts atomicWriteOpts) error {
	if opts.mode != 0 {
		if err := os.Chmod(tmpFile.Name(), opts.mode); err != nil {
			return err
		}
	}
	if opts.owner != nil {
		if err := tmpFile.Chown(opts.owner.uid, opts.owner.gid); err != nil {
			return fmt.Errorf("failed to set owner of temp file: %w", err)
		}
	}
	return nil
}

// writeAtomic is the one implementation behind both exported entry points. Keeping a
// single body is deliberate: the bug this fixes existed because two near-identical
// copies drifted and only one of them fsynced.
func writeAtomic(path string, data io.Reader, opts atomicWriteOpts) (retErr error) {
	dir := filepath.Dir(path)

	tmpFile, err := opts.createTemp(dir, opts.pattern)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Whatever happens, never leave the temp file behind. After a successful rename it
	// no longer exists, so this is a no-op on the happy path.
	defer func() {
		if Exists(tmpPath) {
			retErr = joinErrors(os.Remove(tmpPath), retErr)
		}
	}()

	// closeTemp is only used on the error returns below; the happy path closes directly.
	closeTemp := func() {
		if err := tmpFile.Close(); err != nil {
			slog.Default().With("logger", "file").Debug("failed to close temp file during cleanup", "path", tmpPath, "error", err)
		}
	}

	if err := setModeAndOwner(tmpFile, opts); err != nil {
		closeTemp()
		return err
	}

	n, err := io.Copy(tmpFile, data)
	if err != nil {
		closeTemp()
		return fmt.Errorf("failed to write to temp file: %w", err)
	}

	// fsync BEFORE the rename. Ordered/journalled filesystems only promise that no stale
	// data appears; a zero-length file after a crash is a valid outcome without this.
	if err := tmpFile.Sync(); err != nil {
		closeTemp()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	// After the fsync, not before: FADV_DONTNEED only evicts clean pages, so the hint is
	// actually honoured now rather than silently skipping the dirty ones.
	if opts.fadvise {
		tryMarkLargeFileAsNotNeeded(n, tmpFile)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	// fsync the parent directory AFTER the rename: the file's own fsync says nothing
	// about the durability of the directory entry that now points at it.
	if err := opts.syncDir(dir); err != nil {
		return fmt.Errorf("failed to sync directory %s: %w", dir, err)
	}

	return nil
}

// joinErrors combines two errors into one; either may be nil.
func joinErrors(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return fmt.Errorf("%s: %w", a.Error(), b)
}

// tryMarkLargeFileAsNotNeeded attempts to mark a file as not needed in the page cache.
// This is a performance optimization for large files to free up memory.
// It only applies to files larger than a threshold and silently ignores errors.
func tryMarkLargeFileAsNotNeeded(size int64, in syncFile) {
	// Somewhat arbitrary value to doesn't bother with this on small files
	const largeFileThreshold = 16 * 1024
	if size < largeFileThreshold {
		return
	}
	if err := markNotNeeded(in.Fd()); err != nil {
		// Error is fine, this is just an optimization anyway. Continue
		slog.Default().With("logger", "file").Error("failed to mark not needed, continuing anyways", "error", err)
	}
}
