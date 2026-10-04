package main

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // Mirrors the tool: GNU build-ID convention, not security.
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// synthELF builds a minimal little-endian ELF64 image carrying a single
// SHT_NOTE section named .note.gnu.build-id whose descriptor is id, plus a
// `filler` payload so that two images can be made to differ outside the note.
// It is a stand-in for a linked Go binary: the tool only ever reads the section
// header table and rewrites 20 bytes, so nothing else needs to be real.
func synthELF(t *testing.T, id []byte, filler ...byte) []byte {
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
	fillerOff := shstrtabOff + uint32(len(shstrtab))
	shoff := fillerOff + uint32(len(filler))

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
	img = append(img, filler...)
	img = append(img, shdr(0, 0 /*SHT_NULL*/, 0, 0)...)
	img = append(img, shdr(noteNameOff, 7 /*SHT_NOTE*/, noteOff, uint32(len(note)))...)
	img = append(img, shdr(1, 3 /*SHT_STRTAB*/, shstrtabOff, uint32(len(shstrtab)))...)
	return img
}

func mustSet(t *testing.T, img []byte) []byte {
	t.Helper()
	id, err := setContentBuildID(img)
	if err != nil {
		t.Fatalf("setContentBuildID: %v", err)
	}
	return id
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

// The definition of the ID is load-bearing and documented in the package
// comment: SHA-1 over the image with the descriptor field replaced by zeroes.
// Pin it so a refactor cannot quietly change what gets uploaded to Pyroscope.
func TestContentBuildIDIsSHA1OfTheZeroedImage(t *testing.T) {
	img := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), 1, 2, 3, 4)

	off, err := buildIDOffset(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	zeroed := append([]byte(nil), img...)
	copy(zeroed[off:off+buildIDLen], make([]byte, buildIDLen))
	want := sha1.Sum(zeroed) //nolint:gosec // See above.

	got, err := contentBuildID(img)
	if err != nil {
		t.Fatalf("contentBuildID: %v", err)
	}
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("contentBuildID = %x, want sha1(zeroed image) = %x", got, want)
	}
	// contentBuildID must not disturb the image it hashes.
	if !bytes.Equal(img, synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), 1, 2, 3, 4)) {
		t.Fatal("contentBuildID mutated its input")
	}
}

func TestSetContentBuildIDReplacesOnlyTheDescriptor(t *testing.T) {
	original := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen))
	patched := append([]byte(nil), original...)

	id := mustSet(t, patched)
	if len(id) != buildIDLen {
		t.Fatalf("build ID is %d bytes, want %d", len(id), buildIDLen)
	}
	got, err := readBuildID(bytes.NewReader(patched))
	if err != nil {
		t.Fatalf("readBuildID after set: %v", err)
	}
	if !bytes.Equal(got, id) {
		t.Fatalf("note is %x, want the returned id %x", got, id)
	}
	if len(patched) != len(original) {
		t.Fatalf("set resized the image: %d -> %d", len(original), len(patched))
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
}

// Deterministic: the same input bytes always produce the same ID (so the ID is
// reproducible and the remote cache stays warm).
func TestSetContentBuildIDIsDeterministic(t *testing.T) {
	a := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), 7, 7, 7)
	b := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), 7, 7, 7)
	if !bytes.Equal(mustSet(t, a), mustSet(t, b)) {
		t.Fatal("identical inputs produced different build IDs")
	}
	if !bytes.Equal(a, b) {
		t.Fatal("identical inputs produced different images")
	}
}

// Idempotent: zeroing → hashing → writing, re-run on an already-written binary,
// yields the identical ID and the identical image. This is what makes the ID
// self-verifying (`buildid verify` recomputes it from the shipped bytes).
func TestSetContentBuildIDIsIdempotent(t *testing.T) {
	img := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), 9, 9)
	first := mustSet(t, img)
	once := append([]byte(nil), img...)

	second := mustSet(t, img)
	if !bytes.Equal(first, second) {
		t.Fatalf("re-running changed the id: %x -> %x", first, second)
	}
	if !bytes.Equal(once, img) {
		t.Fatal("re-running changed the image")
	}

	// The ID is independent of whatever ID the image arrived with.
	fresh := synthELF(t, bytes.Repeat([]byte{0xff}, buildIDLen), 9, 9)
	if !bytes.Equal(mustSet(t, fresh), first) {
		t.Fatal("the id depends on the previous descriptor value")
	}
}

// The headline #653 property: binaries that differ anywhere outside the
// descriptor get different IDs.
func TestSetContentBuildIDIsPairwiseDistinct(t *testing.T) {
	seen := map[string]int{}
	for i := range 8 {
		img := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), byte(i))
		seen[string(mustSet(t, img))]++
	}
	if len(seen) != 8 {
		t.Fatalf("8 distinct images produced %d distinct build IDs: %v", len(seen), seen)
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
	if _, err := setContentBuildID(img); !errors.Is(err, errNoBuildIDNote) {
		t.Fatalf("setContentBuildID err = %v, want errNoBuildIDNote", err)
	}
}

func writeELF(t *testing.T, path string, filler ...byte) {
	t.Helper()
	img := synthELF(t, bytes.Repeat([]byte{0x49}, buildIDLen), filler...)
	if err := os.WriteFile(path, img, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunSetAndVerify(t *testing.T) {
	dir := t.TempDir()
	inA, outA := filepath.Join(dir, "a"), filepath.Join(dir, "a_out")
	inB, outB := filepath.Join(dir, "b"), filepath.Join(dir, "b_out")
	writeELF(t, inA, 1)
	writeELF(t, inB, 2)

	for _, pair := range [][2]string{{inA, outA}, {inB, outB}} {
		if err := runSet([]string{"-in", pair[0], "-out", pair[1]}); err != nil {
			t.Fatalf("runSet(%s): %v", pair[0], err)
		}
	}

	if err := runVerify([]string{outA, outB}); err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	// The un-rewritten inputs still carry the linker's constant note, so they
	// fail self-verification.
	if err := runVerify([]string{inA, inB}); err == nil {
		t.Fatal("expected binaries whose note is not their content hash to fail")
	}

	info, err := os.Stat(outA)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("output is not executable: %v", info.Mode())
	}
}

// Two copies of the same binary share an ID by construction; the guard must
// still reject the set, because that is exactly the #653 failure mode.
func TestRunVerifyRejectsSharedBuildIDs(t *testing.T) {
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
	writeELF(t, one, 3)
	writeELF(t, two, 3)
	for _, p := range []string{one, two} {
		if err := runSet([]string{"-in", p, "-out", p}); err != nil {
			t.Fatalf("runSet: %v", err)
		}
	}

	err := runVerify([]string{one, two})
	if err == nil {
		t.Fatal("expected two identical binaries to fail the distinctness check")
	}
	if !strings.Contains(err.Error(), "is shared by 2 binaries") {
		t.Fatalf("error does not name the collision: %v", err)
	}
}

func TestRunSetNonELFCopies(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "not-an-elf")
	out := filepath.Join(dir, "not-an-elf_out")
	payload := []byte("#!/bin/sh\necho hello\n")
	if err := os.WriteFile(in, payload, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runSet([]string{"-in", in, "-out", out}); err != nil {
		t.Fatalf("runSet: %v", err)
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
	one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
	writeELF(t, one, 4)
	writeELF(t, two, 5)
	var ids []string
	for _, p := range []string{one, two} {
		if err := runSet([]string{"-in", p, "-out", p}); err != nil {
			t.Fatalf("runSet: %v", err)
		}
		buf, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		id, err := readBuildID(bytes.NewReader(buf))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, string(id))
	}

	marker := filepath.Join(dir, "marker.txt")
	if err := runVerify([]string{"-marker", marker, one, two}); err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(string(body), hex.EncodeToString([]byte(id))) {
			t.Fatalf("marker %q does not report build ID %x", body, id)
		}
	}
}
