package util

import (
	"fmt"
	"os"
	"path/filepath"
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
			os.Remove(tempPath)
		}
	}()

	// Write data to the temp file
	if _, err = tempFile.Write(data); err != nil {
		tempFile.Close()
		return fmt.Errorf("failed to write to temp file: %w", err)
	}

	// Ensure data is flushed to the disk
	if err = tempFile.Sync(); err != nil {
		tempFile.Close()
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
