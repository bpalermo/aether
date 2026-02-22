package file

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	ctrl "sigs.k8s.io/controller-runtime"
)

func WriteFileAtomic(filePath string, data []byte) error {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)

	// Write atomically using a temporary file
	tempFile, err := os.CreateTemp(dir, fmt.Sprintf(".%s-*.tmp", base))
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tempFile.Name()

	// Clean up temp file on error
	defer func() {
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	// Write data to the temp file
	if _, err = tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("failed to write to temp file: %w", err)
	}

	// Ensure data is flushed to the disk
	if err = tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err = tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomically rename the temp file to the final destination
	if err = os.Rename(tempPath, filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func Exists(name string) bool {
	// We must explicitly check if the error is due to the file not existing (as opposed to a
	// permissions error).
	_, err := os.Stat(name)
	return !errors.Is(err, fs.ErrNotExist)
}

// AtomicWrite writes atomically by writing to a temporary file in the same directory then renaming
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	return AtomicWriteReader(path, bytes.NewReader(data), mode)
}

func AtomicWriteReader(path string, data io.Reader, mode os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.")
	if err != nil {
		return err
	}
	defer func() {
		if Exists(tmpFile.Name()) {
			if rmErr := os.Remove(tmpFile.Name()); rmErr != nil {
				if err != nil {
					err = fmt.Errorf("%s: %w", rmErr.Error(), err)
				} else {
					err = rmErr
				}
			}
		}
	}()

	if err := os.Chmod(tmpFile.Name(), mode); err != nil {
		return err
	}

	n, err := io.Copy(tmpFile, data)
	if _, err := io.Copy(tmpFile, data); err != nil {
		if closeErr := tmpFile.Close(); closeErr != nil {
			err = fmt.Errorf("%s: %w", closeErr.Error(), err)
		}
		return err
	}
	tryMarkLargeFileAsNotNeeded(n, tmpFile)
	if err := tmpFile.Close(); err != nil {
		return err
	}

	return os.Rename(tmpFile.Name(), path)
}

func tryMarkLargeFileAsNotNeeded(size int64, in *os.File) {
	// Somewhat arbitrary value to doesn't bother with this on small files
	const largeFileThreshold = 16 * 1024
	if size < largeFileThreshold {
		return
	}
	if err := markNotNeeded(in); err != nil {
		// Error is fine, this is just an optimization anyway. Continue
		ctrl.Log.Error(err, "failed to mark not needed, continuing anyways")
	}
}
