// Package main implements the GNU build-ID stamping tool used by the Bazel
// `stamped_build_id` rule (see defs.bzl).
//
// Why this exists: rules_go links every Go binary with `-buildid=redacted` for
// reproducibility, and since Go 1.24 the Go linker derives the ELF
// `.note.gnu.build-id` note from that Go build ID by default (`-B gobuildid`).
// A constant input yields a constant note, so every aether Go binary ever built
// carries the same GNU build-ID (498919aff001d8366249403a2544fba2d833084f) no
// matter what code is in it (issue #651). Profilers that join samples to symbols
// purely by build-ID — the Pyroscope eBPF symbolizer does — then silently
// symbolize a new release against the previous release's offsets.
//
// rules_go does not stamp-expand `gc_linkopts`, so `-B 0x<sha>` cannot be fed to
// the linker from the workspace status. Instead this tool rewrites the note
// in place after the link: the note's descriptor is exactly 20 bytes, which is
// also the width of a git commit SHA-1, so the release commit drops straight in
// without moving a single byte of the ELF.
package main

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// buildIDSection is the ELF section holding the GNU build-ID note.
	buildIDSection = ".note.gnu.build-id"
	// buildIDLen is the descriptor width the Go linker emits (SHA-1 shaped),
	// and the width of a git commit SHA-1. They match on purpose: the note is
	// patched in place, never resized.
	buildIDLen = 20
	// noteTypeGNUBuildID is NT_GNU_BUILD_ID.
	noteTypeGNUBuildID = 3
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
	if sec == nil || sec.Type != elf.SHT_NOTE {
		return 0, errNoBuildIDNote
	}
	// SHT_NOBITS sections occupy no file bytes, so there would be nothing to
	// patch; treat it as absent rather than corrupting the image.
	if sec.Type == elf.SHT_NOBITS || sec.Size < 12 {
		return 0, errNoBuildIDNote
	}

	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, int64(sec.Offset)); err != nil {
		return 0, fmt.Errorf("reading note header: %w", err)
	}
	bo := f.ByteOrder
	namesz := bo.Uint32(hdr[0:4])
	descsz := bo.Uint32(hdr[4:8])
	typ := bo.Uint32(hdr[8:12])

	if typ != noteTypeGNUBuildID {
		return 0, fmt.Errorf("%s: unexpected note type %d (want %d)", buildIDSection, typ, noteTypeGNUBuildID)
	}
	if namesz != uint32(len(gnuNoteName)) {
		return 0, fmt.Errorf("%s: unexpected note namesz %d (want %d)", buildIDSection, namesz, len(gnuNoteName))
	}
	if descsz != buildIDLen {
		return 0, fmt.Errorf("%s: descriptor is %d bytes, want %d — cannot patch in place", buildIDSection, descsz, buildIDLen)
	}

	name := make([]byte, namesz)
	if _, err := r.ReadAt(name, int64(sec.Offset)+12); err != nil {
		return 0, fmt.Errorf("reading note name: %w", err)
	}
	if !bytes.Equal(name, gnuNoteName) {
		return 0, fmt.Errorf("%s: unexpected note owner %q (want %q)", buildIDSection, name, gnuNoteName)
	}

	// The name is padded to a 4-byte boundary; "GNU\0" is already 4 bytes.
	descOff := int64(sec.Offset) + 12 + int64(align4(namesz))
	if descOff+buildIDLen > int64(sec.Offset)+int64(sec.Size) {
		return 0, fmt.Errorf("%s: descriptor runs past the section", buildIDSection)
	}
	return descOff, nil
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

// patch rewrites the GNU build-ID descriptor of the ELF image in buf to id.
func patch(buf []byte, id []byte) error {
	if len(id) != buildIDLen {
		return fmt.Errorf("build ID is %d bytes, want %d", len(id), buildIDLen)
	}
	off, err := buildIDOffset(bytes.NewReader(buf))
	if err != nil {
		return err
	}
	copy(buf[off:off+buildIDLen], id)
	return nil
}

// parseStatus reads a Bazel workspace status file ("KEY value" per line).
func parseStatus(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !found || key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}

// commitFromStatus extracts the release commit SHA from a parsed workspace
// status map. STABLE_GIT_COMMIT is the dedicated variable; STABLE_GIT_VERSION
// (`git describe --long --abbrev=40`) is accepted as a fallback so the stamp
// still resolves if the status script is rolled back. Returns "" when no commit
// can be resolved — the caller then leaves the binary untouched.
func commitFromStatus(status map[string]string) string {
	if sha := status["STABLE_GIT_COMMIT"]; isSHA1(sha) {
		return sha
	}
	// `v0.91.0-12-g<40 hex>[-dirty]` or a bare 40-hex from `--always`.
	for _, field := range strings.Split(status["STABLE_GIT_VERSION"], "-") {
		if isSHA1(field) {
			return field
		}
		if len(field) == 41 && field[0] == 'g' && isSHA1(field[1:]) {
			return field[1:]
		}
	}
	return ""
}

func isSHA1(s string) bool {
	if len(s) != 2*buildIDLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// readCommit resolves the release commit from a workspace status file. An empty
// path (no stamping requested) yields "".
func readCommit(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return commitFromStatus(parseStatus(f)), nil
}
