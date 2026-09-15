package file

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSyncFile wraps a real *os.File so the write still lands on disk, while letting a
// test observe the Sync/Close ordering and force an fsync failure.
type fakeSyncFile struct {
	*os.File

	syncErr error

	syncs   int
	closes  int
	written int64
	// syncedBytes records how much had been written when Sync was first called, so a
	// test can prove the fsync happened after the payload and not before it.
	syncedBytes int64
	// closedAfterSync records whether Close ran after at least one Sync.
	closedAfterSync bool
}

func (f *fakeSyncFile) Write(p []byte) (int, error) {
	n, err := f.File.Write(p)
	f.written += int64(n)
	return n, err
}

func (f *fakeSyncFile) Sync() error {
	f.syncs++
	f.syncedBytes = f.written
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *fakeSyncFile) Close() error {
	f.closes++
	f.closedAfterSync = f.syncs > 0
	return f.File.Close()
}

// recorder captures what writeAtomic did through its two seams.
type recorder struct {
	file     *fakeSyncFile
	syncErr  error
	dirErr   error
	dirSyncs []string
	// renamedBeforeDirSync records whether the destination already existed when the
	// directory fsync ran, which is the ordering the durability contract requires.
	renamedBeforeDirSync []bool
	dest                 string
}

func (r *recorder) opts(mode os.FileMode, fadvise bool, pattern string) atomicWriteOpts {
	return atomicWriteOpts{
		pattern: pattern,
		mode:    mode,
		fadvise: fadvise,
		createTemp: func(dir, pattern string) (syncFile, error) {
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			r.file = &fakeSyncFile{File: f, syncErr: r.syncErr}
			return r.file, nil
		},
		syncDir: func(dir string) error {
			r.dirSyncs = append(r.dirSyncs, dir)
			r.renamedBeforeDirSync = append(r.renamedBeforeDirSync, Exists(r.dest))
			if r.dirErr != nil {
				return r.dirErr
			}
			return syncDir(dir)
		},
	}
}

func TestWriteAtomicSyncsFileThenDirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.json")
	payload := []byte(`{"records":{"a.b.aether.internal":"10.0.0.1"}}`)

	r := &recorder{dest: dest}
	if err := writeAtomic(dest, bytes.NewReader(payload), r.opts(0o644, true, "snapshot.json.tmp.")); err != nil {
		t.Fatalf("writeAtomic() = %v, want nil", err)
	}

	if r.file == nil {
		t.Fatal("temp file was never created")
	}
	if r.file.syncs != 1 {
		t.Errorf("temp file Sync called %d times, want 1", r.file.syncs)
	}
	if r.file.syncedBytes != int64(len(payload)) {
		t.Errorf("Sync saw %d bytes written, want %d (fsync must follow the payload)", r.file.syncedBytes, len(payload))
	}
	if !r.file.closedAfterSync {
		t.Error("temp file was closed before it was synced")
	}
	if r.file.closes != 1 {
		t.Errorf("temp file Close called %d times, want 1", r.file.closes)
	}

	if len(r.dirSyncs) != 1 {
		t.Fatalf("directory synced %d times, want 1", len(r.dirSyncs))
	}
	if r.dirSyncs[0] != dir {
		t.Errorf("directory synced = %q, want %q", r.dirSyncs[0], dir)
	}
	if !r.renamedBeforeDirSync[0] {
		t.Error("directory was synced before the rename; the new dir entry would not be covered")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("destination content = %q, want %q", got, payload)
	}
	if mode := fileMode(t, dest); mode != 0o644 {
		t.Errorf("destination mode = %v, want 0644", mode)
	}
}

func TestWriteAtomicFileSyncFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "conflist")
	if err := os.WriteFile(dest, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("input/output error")
	r := &recorder{dest: dest, syncErr: sentinel}
	err := writeAtomic(dest, strings.NewReader("new"), r.opts(0o644, false, "conflist.tmp."))
	if err == nil {
		t.Fatal("writeAtomic() = nil, want an error: an unsynced rename must never be reported as success")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("writeAtomic() = %v, want it to wrap %v", err, sentinel)
	}

	// The rename must not have happened, so the previous contents are still there.
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read destination: %v", readErr)
	}
	if string(got) != "previous" {
		t.Errorf("destination content = %q, want the untouched %q", got, "previous")
	}
	if len(r.dirSyncs) != 0 {
		t.Errorf("directory was synced %d times after a failed file sync, want 0", len(r.dirSyncs))
	}
	requireNoTempFiles(t, dir)
}

func TestWriteAtomicDirSyncFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.json")

	sentinel := errors.New("no space left on device")
	r := &recorder{dest: dest, dirErr: sentinel}
	err := writeAtomic(dest, strings.NewReader("payload"), r.opts(0o600, false, "snapshot.json.tmp."))
	if err == nil {
		t.Fatal("writeAtomic() = nil, want an error when the directory fsync fails")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("writeAtomic() = %v, want it to wrap %v", err, sentinel)
	}
	requireNoTempFiles(t, dir)
}

// TestExportedWritersSyncTheDirectory pins the durability contract on the two exported
// entry points, so a future refactor of either cannot quietly drop it again (S23: the
// bug existed because one of two near-identical copies did not fsync).
func TestExportedWritersSyncTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(path string) error
	}{
		{"WriteFileAtomic", func(path string) error { return WriteFileAtomic(path, []byte("x")) }},
		{"AtomicWrite", func(path string) error { return AtomicWrite(path, []byte("x"), 0o644) }},
		{"AtomicWriteReader", func(path string) error {
			return AtomicWriteReader(path, strings.NewReader("x"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "durable.bin")
			if err := tc.write(path); err != nil {
				t.Fatalf("%s() = %v, want nil", tc.name, err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "x" {
				t.Fatalf("read back = %q, %v; want %q, nil", got, err, "x")
			}
			requireNoTempFiles(t, dir)

			// A real directory fsync on the test's tmpfs/ext4 must succeed.
			if err := syncDir(dir); err != nil {
				t.Errorf("syncDir(%q) = %v, want nil", dir, err)
			}
		})
	}
}

func TestSyncDirMissingDirectory(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("syncDir() on a missing directory = nil, want an error")
	}
}

func requireNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temporary file %s was not cleaned up", e.Name())
		}
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}
