//go:build windows

package file

// syncDir is a no-op on Windows, which has no equivalent of fsyncing a directory
// handle: a directory cannot be opened for reading there, so there is nothing to flush.
// Renames are covered by NTFS metadata journalling instead.
func syncDir(string) error {
	return nil
}
