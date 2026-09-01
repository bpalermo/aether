// Package main implements the GNU build-ID tool used by the Bazel
// `content_build_id` rule (see defs.bzl).
//
// # Why this exists
//
// rules_go links every Go binary with `-buildid=redacted` for reproducibility,
// and since Go 1.24 the Go linker derives the ELF `.note.gnu.build-id` note from
// that Go build ID by default (`-B gobuildid`). A constant input yields a
// constant note, so every aether Go binary ever built carried the same GNU
// build-ID (498919aff001d8366249403a2544fba2d833084f) no matter what code was in
// it (issue #651). Profilers that join samples to symbols purely by build-ID —
// the Pyroscope eBPF symbolizer does — then silently symbolize one binary
// against another binary's offsets.
//
// The first fix (#652) rewrote the note to the release commit SHA. That traded
// one collision for another: a commit produces *seven* released ELFs, so all
// seven ended up sharing a single ID (issue #653). A GNU build-ID identifies a
// *linked object*, not a source revision.
//
// # What this tool writes
//
// The ID is derived from the binary's own bytes, which is what `ld --build-id=sha1`
// does natively and what the custom Envoy now gets from the linker:
//
//	build_id = SHA-1( image bytes, with the 20-byte build-ID descriptor
//	                  field replaced by 20 zero bytes )
//
// Zeroing the descriptor before hashing is what makes the value well defined:
// the descriptor is the one field the hash is about to overwrite, so excluding it
// removes the circular dependency. Three properties follow, and all three are
// covered by unit tests:
//
//   - deterministic — the same image bytes always yield the same ID, so the ID is
//     remote-cache-friendly and reproducible;
//   - idempotent — writing the ID changes only the descriptor, which the hash
//     ignores, so re-running the tool on an already-written binary computes and
//     writes exactly the same 20 bytes;
//   - collision-free in practice — two ELFs that differ anywhere outside the
//     descriptor get different IDs, which is precisely the #653 requirement.
//
// Nothing else in the image moves: the descriptor is exactly 20 bytes wide, the
// same width as a SHA-1 digest, so the note is patched in place.
//
// Release traceability does not live in this note. It lives in the `--stamp`
// x_defs version information and in the image tags; the ELF note is the
// symbolizer's key and nothing else.
package main

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // Not security: SHA-1 is the GNU build-ID convention (`ld --build-id=sha1`) and the note is exactly 20 bytes wide.
	"debug/elf"
	"errors"
	"fmt"
	"io"
)

const (
	// buildIDSection is the ELF section holding the GNU build-ID note.
	buildIDSection = ".note.gnu.build-id"
	// buildIDLen is the descriptor width the Go linker emits and the width of a
	// SHA-1 digest. They match on purpose: the note is patched in place, never
	// resized.
	buildIDLen = sha1.Size
	// noteTypeGNUBuildID is NT_GNU_BUILD_ID.
	noteTypeGNUBuildID = 3
	// noteHeaderLen is the fixed part of an ELF note: namesz, descsz, type.
	noteHeaderLen = 12
)

// gnuNoteName is the note owner, NUL-terminated as the ELF note format requires.
var gnuNoteName = []byte("GNU\x00")

// errNoBuildIDNote reports an ELF file that carries no GNU build-ID note at all.
var errNoBuildIDNote = errors.New("no " + buildIDSection + " note")

// buildIDOffset returns the file offset of the 20-byte build-ID descriptor in
// the ELF image held by r, or errNoBuildIDNote when the image has no such note.
//
// ELF note layout (Elf_Nhdr, all fields in the file's byte order):
//
//	namesz uint32 | descsz uint32 | type uint32 | name [namesz]byte (4-aligned) | desc [descsz]byte
func buildIDOffset(r io.ReaderAt) (int64, error) {
	f, err := elf.NewFile(r)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	sec := f.Section(buildIDSection)
	if sec == nil || sec.Type != elf.SHT_NOTE || sec.Size < noteHeaderLen {
		// SHT_NOBITS sections occupy no file bytes, so there would be nothing to
		// patch; treat anything that is not a real note as absent rather than
		// corrupting the image.
		return 0, errNoBuildIDNote
	}

	hdr := make([]byte, noteHeaderLen)
	if _, err := r.ReadAt(hdr, int64(sec.Offset)); err != nil {
		return 0, fmt.Errorf("reading note header: %w", err)
	}
	bo := f.ByteOrder
	namesz, descsz, typ := bo.Uint32(hdr[0:4]), bo.Uint32(hdr[4:8]), bo.Uint32(hdr[8:12])
	if err := checkNoteHeader(namesz, descsz, typ); err != nil {
		return 0, err
	}

	name := make([]byte, namesz)
	if _, err := r.ReadAt(name, int64(sec.Offset)+noteHeaderLen); err != nil {
		return 0, fmt.Errorf("reading note name: %w", err)
	}
	if !bytes.Equal(name, gnuNoteName) {
		return 0, fmt.Errorf("%s: unexpected note owner %q (want %q)", buildIDSection, name, gnuNoteName)
	}

	// The name is padded to a 4-byte boundary; "GNU\0" is already 4 bytes.
	descOff := int64(sec.Offset) + noteHeaderLen + int64(align4(namesz))
	if descOff+buildIDLen > int64(sec.Offset)+int64(sec.Size) {
		return 0, fmt.Errorf("%s: descriptor runs past the section", buildIDSection)
	}
	return descOff, nil
}

// checkNoteHeader rejects a note that is not a 20-byte NT_GNU_BUILD_ID owned by
// "GNU" — the only shape this tool can rewrite without moving other bytes.
func checkNoteHeader(namesz, descsz, typ uint32) error {
	switch {
	case typ != noteTypeGNUBuildID:
		return fmt.Errorf("%s: unexpected note type %d (want %d)", buildIDSection, typ, noteTypeGNUBuildID)
	case namesz != uint32(len(gnuNoteName)):
		return fmt.Errorf("%s: unexpected note namesz %d (want %d)", buildIDSection, namesz, len(gnuNoteName))
	case descsz != buildIDLen:
		return fmt.Errorf("%s: descriptor is %d bytes, want %d — cannot patch in place", buildIDSection, descsz, buildIDLen)
	}
	return nil
}

func align4(n uint32) uint32 {
	return (n + 3) &^ 3
}

// readBuildID returns the current 20-byte build-ID descriptor of an ELF image.
func readBuildID(r io.ReaderAt) ([]byte, error) {
	off, err := buildIDOffset(r)
	if err != nil {
		return nil, err
	}
	id := make([]byte, buildIDLen)
	if _, err := r.ReadAt(id, off); err != nil {
		return nil, err
	}
	return id, nil
}

// contentBuildID returns the content-derived build-ID of the ELF image in buf:
// SHA-1 over the whole image with the build-ID descriptor field zeroed. The
// image is not modified — the zeroes are fed to the hash in place of the
// descriptor's current bytes, which is why the result does not depend on
// whatever ID the image already carries (and hence why writing it is idempotent).
func contentBuildID(buf []byte) ([]byte, error) {
	off, err := buildIDOffset(bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	h := sha1.New() //nolint:gosec // See the package comment: build-ID convention, not security.
	h.Write(buf[:off])
	h.Write(make([]byte, buildIDLen))
	h.Write(buf[off+buildIDLen:])
	return h.Sum(nil), nil
}

// setContentBuildID rewrites the GNU build-ID descriptor of the ELF image in buf
// to the image's content hash and returns the value written.
func setContentBuildID(buf []byte) ([]byte, error) {
	id, err := contentBuildID(buf)
	if err != nil {
		return nil, err
	}
	off, err := buildIDOffset(bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	copy(buf[off:off+buildIDLen], id)
	return id, nil
}

func isELF(buf []byte) bool {
	return len(buf) >= 4 && bytes.Equal(buf[:4], []byte("\x7fELF"))
}
