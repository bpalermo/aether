package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		data    []byte
		wantErr bool
	}{
		{
			name: "write to new file successfully",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				return filepath.Join(dir, "test.txt")
			},
			data:    []byte("test content"),
			wantErr: false,
		},
		{
			name: "overwrite existing file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				path := filepath.Join(dir, "existing.txt")
				if err := os.WriteFile(path, []byte("old content"), 0644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			data:    []byte("new content"),
			wantErr: false,
		},
		{
			name: "write empty data",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				return filepath.Join(dir, "empty.txt")
			},
			data:    []byte{},
			wantErr: false,
		},
		{
			name: "write to non-existent directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				return filepath.Join(dir, "nonexistent", "subdir", "file.txt")
			},
			data:    []byte("test"),
			wantErr: true,
		},
		{
			name: "write to read-only directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				readOnlyDir := filepath.Join(dir, "readonly")
				if err := os.Mkdir(readOnlyDir, 0555); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(readOnlyDir, "file.txt")
			},
			data:    []byte("test"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := tt.setup(t)

			err := WriteFileAtomic(filePath, tt.data)

			if (err != nil) != tt.wantErr {
				t.Errorf("WriteFileAtomic() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify the file exists and has correct content
				content, err := os.ReadFile(filePath)
				if err != nil {
					t.Errorf("Failed to read written file: %v", err)
					return
				}

				if string(content) != string(tt.data) {
					t.Errorf("File content = %q, want %q", content, tt.data)
				}

				// Verify no temp files remain
				dir := filepath.Dir(filePath)
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Errorf("Failed to read directory: %v", err)
					return
				}

				for _, entry := range entries {
					if filepath.Ext(entry.Name()) == ".tmp" {
						t.Errorf("Temporary file %s was not cleaned up", entry.Name())
					}
				}
			}
		})
	}
}

func TestWriteFileAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "concurrent.txt")

	// Run multiple goroutines writing to the same file
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(n int) {
			data := []byte(string(rune('A' + n)))
			if err := WriteFileAtomic(filePath, data); err != nil {
				t.Errorf("Concurrent write %d failed: %v", n, err)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify file exists and is readable
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Errorf("Failed to read file after concurrent writes: %v", err)
	}

	// Content should be one of the written values
	if len(content) != 1 || content[0] < 'A' || content[0] > 'J' {
		t.Errorf("Unexpected content after concurrent writes: %q", content)
	}
}
