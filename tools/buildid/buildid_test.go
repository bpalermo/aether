package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const releaseSHA = "c46dd35abc0123456789abcdef0123456789abcd"

// synthELF builds a minimal little-endian ELF64 image carrying a single
// SHT_NOTE section named .note.gnu.build-id whose descriptor is id. It is a
// stand-in for a linked Go binary: the stamping tool only ever reads the section
// header table and rewrites 20 bytes, so nothing else needs to be real.
func synthELF(t *testing.T, id []byte) []byte {
	t.Helper()

	const (
		ehdrSize = 64
		shdrSize = 64
	)
	le := binary.LittleEndian

	// Note payload: namesz | descsz | type | "GNU\0" | desc.
	note := make([]byte, 0, 12+4+len(id))
	note = le.AppendUint32(note, 4)
	note = le.AppendUint32(note, uint32(len(id)))
	note = le.AppendUint32(note, noteTypeGNUBuildID)
	note = append(note, gnuNoteName...)
	note = append(note, id...)

	// Section name table: "\0" + ".shstrtab\0" + ".note.gnu.build-id\0".
	shstrtab := append([]byte{0}, ".shstrtab\x00"...)
	shstrtab = append(shstrtab, buildIDSection...)
	shstrtab = append(shstrtab, 0)
	noteNameOff := uint32(len(shstrtab) - len(buildIDSection) - 1)

	noteOff := uint32(ehdrSize)
	shstrtabOff := noteOff + uint32(len(note))
	shoff := shstrtabOff + uint32(len(shstrtab))

	shdr := func(name, typ uint32, off, size uint32) []byte {
		b := make([]byte, 0, shdrSize)
		b = le.AppendUint32(b, name)
		b = le.AppendUint32(b, typ)
		b = le.AppendUint64(b, 0) // sh_flags
		b = le.AppendUint64(b, 0) // sh_addr
		b = le.AppendUint64(b, uint64(off))
		b = le.AppendUint64(b, uint64(size))
		b = le.AppendUint32(b, 0) // sh_link
		b = le.AppendUint32(b, 0) // sh_info
		b = le.AppendUint64(b, 4) // sh_addralign
		b = le.AppendUint64(b, 0) // sh_entsize
		return b
	}

	img := make([]byte, 0, shoff+3*shdrSize)
	ehdr := make([]byte, ehdrSize)
	copy(ehdr, []byte{0x7f, 'E', 'L', 'F', 2 /*ELFCLASS64*/, 1 /*ELFDATA2LSB*/, 1 /*EV_CURRENT*/})
	le.PutUint16(ehdr[16:], 2)  // e_type = ET_EXEC
	le.PutUint16(ehdr[18:], 62) // e_machine = EM_X86_64
	le.PutUint32(ehdr[20:], 1)  // e_version
	le.PutUint64(ehdr[40:], uint64(shoff))
	le.PutUint16(ehdr[52:], ehdrSize) // e_ehsize
	le.PutUint16(ehdr[58:], shdrSize) // e_shentsize
	le.PutUint16(ehdr[60:], 3)        // e_shnum
	le.PutUint16(ehdr[62:], 2)        // e_shstrndx
	img = append(img, ehdr...)
	img = append(img, note...)
	img = append(img, shstrtab...)
	img = append(img, shdr(0, 0 /*SHT_NULL*/, 0, 0)...)
	img = append(img, shdr(noteNameOff, 7 /*SHT_NOTE*/, noteOff, uint32(len(note)))...)
	img = append(img, shdr(1, 3 /*SHT_STRTAB*/, shstrtabOff, uint32(len(shstrtab)))...)
	return img
}

func TestReadBuildID(t *testing.T) {
	want := bytes.Repeat([]byte{0xab}, buildIDLen)
	got, err := readBuildID(bytes.NewReader(synthELF(t, want)))
	if err != nil {
		t.Fatalf("readBuildID: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readBuildID = %x, want %x", got, want)
	}
}

func TestPatchReplacesOnlyTheDescriptor(t *testing.T) {
	original := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen))
	patched := append([]byte(nil), original...)

	want, err := hex.DecodeString(releaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := patch(patched, want); err != nil {
		t.Fatalf("patch: %v", err)
	}

	got, err := readBuildID(bytes.NewReader(patched))
	if err != nil {
		t.Fatalf("readBuildID after patch: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("build ID = %x, want %x", got, want)
	}
	if len(patched) != len(original) {
		t.Fatalf("patch resized the image: %d -> %d", len(original), len(patched))
	}

	// Exactly the 20 descriptor bytes may differ.
	var differing int
	for i := range original {
		if original[i] != patched[i] {
			differing++
		}
	}
	if differing != buildIDLen {
		t.Fatalf("%d bytes differ, want exactly %d", differing, buildIDLen)
	}

	// Patching is idempotent: the same commit yields the same image.
	again := append([]byte(nil), original...)
	if err := patch(again, want); err != nil {
		t.Fatalf("patch (second): %v", err)
	}
	if !bytes.Equal(again, patched) {
		t.Fatal("patching the same commit twice produced different images")
	}
}

func TestPatchWrongIDLength(t *testing.T) {
	img := synthELF(t, bytes.Repeat([]byte{0}, buildIDLen))
	if err := patch(img, []byte{1, 2, 3}); err == nil {
		t.Fatal("expected an error for a short build ID")
	}
}

func TestBuildIDOffsetMissingNote(t *testing.T) {
	img := synthELF(t, bytes.Repeat([]byte{0}, buildIDLen))
	// Rename the note section so the lookup misses it (shstrtab offset 1 is
	// ".shstrtab").
	le := binary.LittleEndian
	shoff := le.Uint64(img[40:])
	le.PutUint32(img[int(shoff)+64:], 1)

	if _, err := readBuildID(bytes.NewReader(img)); !errors.Is(err, errNoBuildIDNote) {
		t.Fatalf("err = %v, want errNoBuildIDNote", err)
	}
}

func TestCommitFromStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{
			name:   "dedicated variable",
			status: "STABLE_GIT_VERSION v0.91.0-3-g" + releaseSHA + "\nSTABLE_GIT_COMMIT " + releaseSHA + "\n",
			want:   releaseSHA,
		},
		{
			name:   "falls back to the describe output",
			status: "STABLE_GIT_VERSION v0.91.0-3-g" + releaseSHA + "\n",
			want:   releaseSHA,
		},
		{
			name:   "falls back to a describe --always bare sha",
			status: "STABLE_GIT_VERSION " + releaseSHA + "\n",
			want:   releaseSHA,
		},
		{
			name:   "dirty tree still resolves the parent commit",
			status: "STABLE_GIT_VERSION v0.91.0-3-g" + releaseSHA + "-dirty\n",
			want:   releaseSHA,
		},
		{
			name:   "no git",
			status: "STABLE_GIT_VERSION unknown\nSTABLE_GIT_COMMIT unknown\n",
			want:   "",
		},
		{
			name:   "empty",
			status: "",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitFromStatus(parseStatus(strings.NewReader(tc.status))); got != tc.want {
				t.Fatalf("commitFromStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// writeStatus writes a stable-status.txt naming the release commit.
func writeStatus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "stable-status.txt")
	body := "BUILD_EMBED_LABEL \nSTABLE_GIT_COMMIT " + releaseSHA + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStampAndVerify(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "binary")
	out := filepath.Join(dir, "binary_stamped")
	if err := os.WriteFile(in, synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen)), 0o755); err != nil {
		t.Fatal(err)
	}
	status := writeStatus(t, dir)

	if err := runStamp([]string{"-in", in, "-out", out, "-status-file", status}); err != nil {
		t.Fatalf("runStamp: %v", err)
	}
	if err := runVerify([]string{"-status-file", status, out}); err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	// The unstamped input must fail the stamped verification.
	if err := runVerify([]string{"-status-file", status, in}); err == nil {
		t.Fatal("expected the unstamped binary to fail verification")
	}
	// Without a status file, verification only asserts the note is present.
	if err := runVerify([]string{in}); err != nil {
		t.Fatalf("runVerify (presence only): %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("stamped output is not executable: %v", info.Mode())
	}
}

func TestRunStampWithoutStatusFileCopies(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "binary")
	out := filepath.Join(dir, "binary_stamped")
	img := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen))
	if err := os.WriteFile(in, img, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runStamp([]string{"-in", in, "-out", out}); err != nil {
		t.Fatalf("runStamp: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, img) {
		t.Fatal("an unstamped build must copy the binary byte-for-byte")
	}
}

func TestRunStampNonELFCopies(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "not-an-elf")
	out := filepath.Join(dir, "not-an-elf_stamped")
	payload := []byte("#!/bin/sh\necho hello\n")
	if err := os.WriteFile(in, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	status := writeStatus(t, dir)

	if err := runStamp([]string{"-in", in, "-out", out, "-status-file", status}); err != nil {
		t.Fatalf("runStamp: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("a non-ELF input must be copied unchanged")
	}
}

func TestRunVerifyWritesMarker(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "binary")
	if err := os.WriteFile(bin, synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen)), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker.txt")
	if err := runVerify([]string{"-marker", marker, bin}); err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), strings.Repeat("49", buildIDLen)) {
		t.Fatalf("marker does not report the build ID: %q", body)
	}
}
