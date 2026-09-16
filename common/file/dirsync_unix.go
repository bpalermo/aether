//go:build !windows

package file

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// syncDir fsyncs a directory so that a rename into it is durable.
//
// os.Rename is atomic with respect to readers, but the directory entry it creates lives
// in the page cache like any other write. Without this fsync a power cut can roll the
// rename back — or leave the name in place pointing at a file whose data was never
// flushed, which is how the CNI conflist and the mesh-DNS snapshot came back missing or
// empty after the 2026-08-29 outage.
//
// Opening the directory O_RDONLY and fsyncing the descriptor is the portable POSIX way
// to do this and works on every filesystem this project runs on (ext4, xfs, overlayfs,
// tmpfs). A filesystem that does not implement it at all is treated as best-effort: the
// added durability is not worth failing the CNI conflist write over.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() {
		_ = d.Close()
	}()

	if err := d.Sync(); err != nil {
		if dirSyncUnsupported(err) {
			slog.Default().With("logger", "file").Debug(
				"directory fsync not supported by this filesystem, continuing", "dir", dir, "error", err)
			return nil
		}
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

// dirSyncUnsupported reports whether err means "this filesystem does not fsync
// directories" rather than "this fsync failed". Real I/O failures (EIO, ENOSPC) are not
// in the list and are surfaced to the caller.
func dirSyncUnsupported(err error) bool {
	for _, e := range []error{syscall.EINVAL, syscall.ENOTSUP, syscall.ENOSYS, syscall.EPERM, syscall.EACCES} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
