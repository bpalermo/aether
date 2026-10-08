// Package buildinfo answers "which build is this process?" from the running
// executable itself, with nothing linked in at build time (#1378, #1429).
//
// # Why the version is the build ID
//
// Every component used to link the commit in (`-X …Version=<git describe>`
// under `--stamp`). That made every binary, and so every image layer, config,
// manifest and index, differ from one commit to the next even when no line of
// its code had changed: a docs-only commit rolled every workload. The version
// is now the binary's GNU build ID, the note //bazel/buildid's
// `content_build_id` rewrites to a hash of the binary's own bytes. It changes
// exactly when the binary changes, it is the key the continuous profiler
// already files this binary's symbols under, and reading it needs no input
// from the build.
//
// Which commit built a given image is answered outside the binary: by the
// image's signature certificate, its provenance attestation and its
// `dev-<sha>` tag (docs/verifying-releases.md).
//
// # Why it is parsed by hand
//
// This package is linked by the proxy supervisor (PID 1 of the proxy
// container), the mesh-DNS daemon and the CSI node plugin, each of which has a
// test that fails when its link set grows. `debug/elf` would do the same job
// and costs binary size for DWARF and compression support nothing here uses;
// finding one note in the header tables of a 64-bit ELF is a few dozen lines.
// The package imports the standard library only, and must stay that way.
package buildinfo

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
)

// Unknown is the version of a process whose executable carries no readable GNU
// build ID: a binary for a platform without ELF, or one linked with the note
// turned off.
const Unknown = "dev"

// ErrNoBuildID reports an executable that is not a 64-bit ELF or carries no GNU
// build ID note.
var ErrNoBuildID = errors.New("no GNU build ID note")

const (
	elfHeaderLen = 64 // Elf64_Ehdr
	elfClass64   = 2  // EI_CLASS: ELFCLASS64
	elfDataLSB   = 1  // EI_DATA: little-endian
	elfDataMSB   = 2  // EI_DATA: big-endian
	progHdrLen   = 56 // Elf64_Phdr
	sectHdrLen   = 64 // Elf64_Shdr
	ptNote       = 4  // PT_NOTE
	shtNote      = 7  // SHT_NOTE
	noteHdrLen   = 12 // namesz, descsz, type
	ntGNUBuildID = 3  // NT_GNU_BUILD_ID
	gnuOwner     = "GNU\x00"

	// maxTableEntries and maxNoteBytes bound what a damaged header can make
	// this read. A Go binary has about ten program headers, a few dozen
	// sections and notes of a few dozen bytes.
	maxTableEntries = 1024
	maxNoteBytes    = 1 << 20
	// maxIDLen is the longest descriptor accepted: SHA-1 is 20 bytes, and no
	// linker writes more than a 64-byte digest.
	maxIDLen = 64
)

// Version returns the running executable's GNU build ID in lowercase hex (40
// characters for the SHA-1 the release images carry), or Unknown when it has
// none. The executable is read once per process.
//
// A binary that did not go through an image build (`bazel run`, `go run`, a
// test binary) still carries the note the Go linker writes; under rules_go that
// note is the same for every binary, because it is derived from a constant Go
// build ID. Only the binaries inside an image have a content-derived one.
var Version = sync.OnceValue(func() string {
	id, err := executableBuildID()
	if err != nil {
		return Unknown
	}
	return id
})

func executableBuildID() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return FileBuildID(path)
}

// FileBuildID returns the GNU build ID of the ELF file at path.
func FileBuildID(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // The path is this process's own executable, or a test's.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return BuildID(f)
}

// BuildID returns the GNU build ID of the 64-bit ELF image in r, in lowercase
// hex.
//
// It reads the note sections first and the note segments second, and needs
// both. Go's own linker, which links the static binaries the images ship, puts
// `.note.gnu.build-id` in a section but leaves it out of the PT_NOTE segment
// (that segment covers `.note.go.buildid` alone), so a walk of the program
// headers finds nothing in exactly the binaries that matter. A binary whose
// section table was stripped has only the segments.
func BuildID(r io.ReaderAt) (string, error) {
	var hdr [elfHeaderLen]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return "", fmt.Errorf("%w: reading the ELF header: %w", ErrNoBuildID, err)
	}
	if string(hdr[:4]) != "\x7fELF" || hdr[4] != elfClass64 {
		return "", fmt.Errorf("%w: not a 64-bit ELF file", ErrNoBuildID)
	}
	var bo binary.ByteOrder
	switch hdr[5] {
	case elfDataLSB:
		bo = binary.LittleEndian
	case elfDataMSB:
		bo = binary.BigEndian
	default:
		return "", fmt.Errorf("%w: unknown ELF byte order %d", ErrNoBuildID, hdr[5])
	}

	// Elf64_Shdr and Elf64_Phdr keep the three fields read here at the same
	// places (file offset aside): size at 32, alignment at 48.
	tables := []noteTable{
		{
			what: "section", off: bo.Uint64(hdr[40:48]), entsize: bo.Uint16(hdr[58:60]), num: bo.Uint16(hdr[60:62]),
			minEntsize: sectHdrLen, typeAt: 4, noteType: shtNote, offsetAt: 24,
		},
		{
			what: "program", off: bo.Uint64(hdr[32:40]), entsize: bo.Uint16(hdr[54:56]), num: bo.Uint16(hdr[56:58]),
			minEntsize: progHdrLen, typeAt: 0, noteType: ptNote, offsetAt: 8,
		},
	}
	var firstErr error
	for _, t := range tables {
		id, err := t.buildID(r, bo)
		if err == nil {
			return id, nil
		}
		if firstErr == nil || errors.Is(firstErr, errNotInTable) {
			firstErr = err
		}
	}
	if errors.Is(firstErr, errNotInTable) {
		return "", ErrNoBuildID
	}
	return "", fmt.Errorf("%w: %w", ErrNoBuildID, firstErr)
}

// errNotInTable is a header table that was read in full and names no GNU build
// ID note: the plain "there is none" answer, as opposed to a damaged file.
var errNotInTable = errors.New("no note in this table")

// noteTable is one of the two ELF header tables that can point at notes.
type noteTable struct {
	what       string
	off        uint64 // file offset of the table
	entsize    uint16
	num        uint16
	minEntsize uint16
	typeAt     int    // offset of the entry's type field
	noteType   uint32 // the type value that marks notes
	offsetAt   int    // offset of the entry's file-offset field
}

func (t noteTable) buildID(r io.ReaderAt, bo binary.ByteOrder) (string, error) {
	if t.num == 0 {
		return "", errNotInTable
	}
	if t.entsize < t.minEntsize || t.num > maxTableEntries {
		return "", fmt.Errorf("implausible %s header table (%d entries of %d bytes)", t.what, t.num, t.entsize)
	}
	entry := make([]byte, t.minEntsize)
	for i := range uint64(t.num) {
		if _, err := r.ReadAt(entry, int64(t.off+i*uint64(t.entsize))); err != nil { //nolint:gosec // A wrapped offset fails the read, which is the handled case.
			return "", fmt.Errorf("reading %s header %d: %w", t.what, i, err)
		}
		if bo.Uint32(entry[t.typeAt:t.typeAt+4]) != t.noteType {
			continue
		}
		offset, size, align := bo.Uint64(entry[t.offsetAt:t.offsetAt+8]), bo.Uint64(entry[32:40]), bo.Uint64(entry[48:56])
		if size > maxNoteBytes {
			continue
		}
		notes := make([]byte, size)
		if _, err := r.ReadAt(notes, int64(offset)); err != nil { //nolint:gosec // As above.
			return "", fmt.Errorf("reading the notes at %d: %w", offset, err)
		}
		if id, ok := findBuildID(notes, bo, align); ok {
			return id, nil
		}
	}
	return "", errNotInTable
}

// findBuildID scans one note section or segment for the NT_GNU_BUILD_ID note
// owned by "GNU". Either holds a sequence of notes:
//
//	namesz uint32 | descsz uint32 | type uint32 | name (padded) | desc (padded)
//
// Name and descriptor are padded to 4 bytes, or to 8 where the section or
// segment is 8-aligned (`.note.gnu.property` in a 64-bit binary).
//
// A note that runs past the end ends the scan: the notes are truncated or
// damaged, and nothing after that point can be trusted.
func findBuildID(seg []byte, bo binary.ByteOrder, segAlign uint64) (string, bool) {
	pad := uint64(4)
	if segAlign == 8 {
		pad = 8
	}
	align := func(n uint64) uint64 { return (n + pad - 1) &^ (pad - 1) }
	for len(seg) >= noteHdrLen {
		namesz, descsz, typ := uint64(bo.Uint32(seg[0:4])), uint64(bo.Uint32(seg[4:8])), bo.Uint32(seg[8:12])
		nameEnd := noteHdrLen + namesz
		descStart := align(nameEnd)
		descEnd := descStart + descsz
		if descEnd > uint64(len(seg)) {
			return "", false
		}
		if typ == ntGNUBuildID && string(seg[noteHdrLen:nameEnd]) == gnuOwner && descsz > 0 && descsz <= maxIDLen {
			return hex.EncodeToString(seg[descStart:descEnd]), true
		}
		next := align(descEnd)
		if next > uint64(len(seg)) {
			return "", false
		}
		seg = seg[next:]
	}
	return "", false
}

// Describe is what a component prints for `--version`: its name and build ID,
// the main package and the Go toolchain, and the main module's version when the
// build recorded one. A Bazel build records the main package and every
// dependency's version but no main module, so the release binaries print no
// module line; a binary built with the go command prints one.
func Describe(component string) string {
	var mainPackage, module, moduleVersion string
	if bi, ok := debug.ReadBuildInfo(); ok {
		mainPackage, module, moduleVersion = bi.Path, bi.Main.Path, bi.Main.Version
	}
	return describe(component, Version(), mainPackage, module, moduleVersion)
}

func describe(component, buildID, mainPackage, module, moduleVersion string) string {
	out := fmt.Sprintf("%s build-id %s\n", component, buildID)
	if mainPackage != "" {
		out += fmt.Sprintf("package %s\n", mainPackage)
	}
	if module != "" {
		if moduleVersion == "" {
			moduleVersion = "(devel)"
		}
		out += fmt.Sprintf("module %s %s\n", module, moduleVersion)
	}
	return out + fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
