//go:build unix

package file

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// optionFile wraps the real temp file and records what the two options act on:
// the fchown (its arguments, and what the file looked like when it ran) and the
// descriptor requests, which only the FADV_DONTNEED hint makes.
type optionFile struct {
	*os.File

	chownErr error

	chowns [][2]int
	// writtenAtChown and modeAtChown are the temp file's size and permission bits
	// when Chown ran; destAtChown is the destination's content at that moment.
	writtenAtChown int64
	modeAtChown    os.FileMode
	destAtChown    string

	dest    string
	written int64
	fds     int
}

func (f *optionFile) Write(p []byte) (int, error) {
	n, err := f.File.Write(p)
	f.written += int64(n)
	return n, err
}

func (f *optionFile) Chown(uid, gid int) error {
	f.chowns = append(f.chowns, [2]int{uid, gid})
	f.writtenAtChown = f.written
	if fi, err := f.Stat(); err == nil {
		f.modeAtChown = fi.Mode().Perm()
	}
	if b, err := os.ReadFile(f.dest); err == nil {
		f.destAtChown = string(b)
	}
	if f.chownErr != nil {
		return f.chownErr
	}
	return f.File.Chown(uid, gid)
}

func (f *optionFile) Fd() uintptr {
	f.fds++
	return f.File.Fd()
}

// withOptionFile returns AtomicWriteReader's own configuration for dest, with the
// temp file wrapped so the test can see it.
func withOptionFile(dest string, mode os.FileMode, chownErr error, options ...WriteOption) (atomicWriteOpts, **optionFile) {
	var created *optionFile
	opts := readerOpts(dest, mode, options)
	opts.createTemp = func(dir, pattern string) (syncFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		created = &optionFile{File: f, dest: dest, chownErr: chownErr}
		return created, nil
	}
	return opts, &created
}

func fileOwnerOf(t *testing.T, path string) (uid, gid int) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no syscall.Stat_t on this platform")
	}
	return int(st.Uid), int(st.Gid)
}

// TestWithOwnerChownsTheTempFileBeforePublishing: the owner is set on the temp
// file, after its mode and before its content, while the destination still
// holds the previous bytes. An unprivileged test can only chown to itself, so
// the arguments are checked on the call and the result on the file.
func TestWithOwnerChownsTheTempFileBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "aether-cni")
	if err := os.WriteFile(dest, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()

	opts, created := withOptionFile(dest, 0o750, nil, WithOwner(uid, gid))
	if err := writeAtomic(dest, strings.NewReader("payload"), opts); err != nil {
		t.Fatalf("writeAtomic() = %v, want nil", err)
	}

	f := *created
	if len(f.chowns) != 1 || f.chowns[0] != [2]int{uid, gid} {
		t.Fatalf("Chown calls = %v, want exactly one with (%d, %d)", f.chowns, uid, gid)
	}
	if f.writtenAtChown != 0 {
		t.Errorf("Chown ran after %d bytes were written, want before any", f.writtenAtChown)
	}
	if f.modeAtChown != 0o750 {
		t.Errorf("temp file mode at Chown = %v, want 0750 (chmod first: chown clears set-id bits)", f.modeAtChown)
	}
	if f.destAtChown != "previous" {
		t.Errorf("destination at Chown = %q, want the previous content (not yet published)", f.destAtChown)
	}

	if got, err := os.ReadFile(dest); err != nil || string(got) != "payload" {
		t.Fatalf("destination = %q, %v; want %q, nil", got, err, "payload")
	}
	if mode := fileMode(t, dest); mode != 0o750 {
		t.Errorf("destination mode = %v, want 0750", mode)
	}
	if gotUID, gotGID := fileOwnerOf(t, dest); gotUID != uid || gotGID != gid {
		t.Errorf("destination owner = %d:%d, want %d:%d", gotUID, gotGID, uid, gid)
	}
	requireNoTempFiles(t, dir)
}

// TestWithOwnerFailureLeavesTheDestination: a file that cannot be given its
// owner is never published under the destination name.
func TestWithOwnerFailureLeavesTheDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "aether-cni")
	if err := os.WriteFile(dest, []byte("previous"), 0o755); err != nil {
		t.Fatal(err)
	}

	denied := errors.New("chown denied")
	opts, created := withOptionFile(dest, 0o755, denied, WithOwner(0, 0))
	err := writeAtomic(dest, strings.NewReader("payload"), opts)
	if !errors.Is(err, denied) {
		t.Fatalf("writeAtomic() = %v, want an error wrapping %v", err, denied)
	}
	if (*created).written != 0 {
		t.Errorf("%d bytes were written after the chown failed, want none", (*created).written)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "previous" {
		t.Errorf("destination = %q, %v; want the previous content", got, err)
	}
	requireNoTempFiles(t, dir)
}

// TestWithoutOwnerNeverChowns: the option is opt-in; the snapshot and conflist
// writers keep the owner the process gives the temp file.
func TestWithoutOwnerNeverChowns(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snapshot.json")
	opts, created := withOptionFile(dest, 0o644, errors.New("must not be called"))
	if err := writeAtomic(dest, strings.NewReader("payload"), opts); err != nil {
		t.Fatalf("writeAtomic() = %v, want nil", err)
	}
	if n := len((*created).chowns); n != 0 {
		t.Errorf("Chown called %d times without WithOwner, want 0", n)
	}
}

// TestKeepInPageCacheSkipsTheEvictionHint: by default a large file is marked
// FADV_DONTNEED after its fsync, which is the only thing that asks the temp
// file for its descriptor. KeepInPageCache must leave it alone; a small file
// never gets the hint either way.
func TestKeepInPageCacheSkipsTheEvictionHint(t *testing.T) {
	large := bytes.Repeat([]byte("x"), 64*1024)
	for _, tc := range []struct {
		name    string
		payload []byte
		options []WriteOption
		wantFds int
	}{
		{"large, default", large, nil, 1},
		{"large, kept", large, []WriteOption{KeepInPageCache()}, 0},
		{"small, default", []byte("x"), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "binary")
			opts, created := withOptionFile(dest, 0o755, nil, tc.options...)
			if err := writeAtomic(dest, bytes.NewReader(tc.payload), opts); err != nil {
				t.Fatalf("writeAtomic() = %v, want nil", err)
			}
			if got := (*created).fds; got != tc.wantFds {
				t.Errorf("descriptor requested %d times, want %d", got, tc.wantFds)
			}
			if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, tc.payload) {
				t.Errorf("destination holds %d bytes, %v; want %d, nil", len(got), err, len(tc.payload))
			}
			requireNoTempFiles(t, dir)
		})
	}
}

// TestAtomicWriteReaderWithOptions drives the exported entry point the way the
// CNI installer does.
func TestAtomicWriteReaderWithOptions(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "aether-cni")
	uid, gid := os.Getuid(), os.Getgid()

	err := AtomicWriteReader(dest, strings.NewReader("plugin"), 0o755, WithOwner(uid, gid), KeepInPageCache())
	if err != nil {
		t.Fatalf("AtomicWriteReader() = %v, want nil", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "plugin" {
		t.Fatalf("destination = %q, %v; want %q, nil", got, err, "plugin")
	}
	if mode := fileMode(t, dest); mode != 0o755 {
		t.Errorf("destination mode = %v, want 0755", mode)
	}
	if gotUID, gotGID := fileOwnerOf(t, dest); gotUID != uid || gotGID != gid {
		t.Errorf("destination owner = %d:%d, want %d:%d", gotUID, gotGID, uid, gid)
	}
	requireNoTempFiles(t, dir)
}
