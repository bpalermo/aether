package buildinfo

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// note encodes one ELF note, name and descriptor padded to pad bytes.
func note(bo binary.ByteOrder, owner string, typ uint32, desc []byte, pad int) []byte {
	padTo := func(b []byte) []byte {
		for len(b)%pad != 0 {
			b = append(b, 0)
		}
		return b
	}
	out := make([]byte, 12)
	bo.PutUint32(out[0:4], uint32(len(owner)))
	bo.PutUint32(out[4:8], uint32(len(desc)))
	bo.PutUint32(out[8:12], typ)
	out = padTo(append(out, owner...))
	return padTo(append(out, desc...))
}

// segment is one entry of the program header table, or of the section header
// table when section is set.
type segment struct {
	section bool
	typ     uint32
	align   uint64
	data    []byte
	// filesz overrides the size the header declares (0: len(data)).
	filesz uint64
}

// elf64 lays out a minimal 64-bit ELF image: the header, the program header
// table, the section header table, then each entry's bytes.
func elf64(bo binary.ByteOrder, segs ...segment) []byte {
	var nprog, nsect int
	for _, s := range segs {
		if s.section {
			nsect++
		} else {
			nprog++
		}
	}
	hdr := make([]byte, elfHeaderLen)
	copy(hdr, "\x7fELF")
	hdr[4] = elfClass64
	hdr[5] = elfDataLSB
	if bo == binary.ByteOrder(binary.BigEndian) {
		hdr[5] = elfDataMSB
	}
	bo.PutUint64(hdr[32:40], elfHeaderLen)
	bo.PutUint16(hdr[54:56], progHdrLen)
	bo.PutUint16(hdr[56:58], uint16(nprog))
	bo.PutUint64(hdr[40:48], uint64(elfHeaderLen+progHdrLen*nprog))
	bo.PutUint16(hdr[58:60], sectHdrLen)
	bo.PutUint16(hdr[60:62], uint16(nsect))

	offset := uint64(elfHeaderLen + progHdrLen*nprog + sectHdrLen*nsect)
	var progs, sects, body []byte
	for _, s := range segs {
		size := uint64(len(s.data))
		if s.filesz != 0 {
			size = s.filesz
		}
		if s.section {
			sh := make([]byte, sectHdrLen)
			bo.PutUint32(sh[4:8], s.typ)
			bo.PutUint64(sh[24:32], offset)
			bo.PutUint64(sh[32:40], size)
			bo.PutUint64(sh[48:56], s.align)
			sects = append(sects, sh...)
		} else {
			ph := make([]byte, progHdrLen)
			bo.PutUint32(ph[0:4], s.typ)
			bo.PutUint64(ph[8:16], offset)
			bo.PutUint64(ph[32:40], size)
			bo.PutUint64(ph[48:56], s.align)
			progs = append(progs, ph...)
		}
		body = append(body, s.data...)
		offset += uint64(len(s.data))
	}
	return append(append(append(hdr, progs...), sects...), body...)
}

var sha1ID = []byte{
	0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x23, 0x45, 0x67,
}

const sha1Hex = "00112233445566778899aabbccddeeff01234567"

func TestBuildID_Synthetic(t *testing.T) {
	le, be := binary.ByteOrder(binary.LittleEndian), binary.ByteOrder(binary.BigEndian)
	abiTag := note(le, gnuOwner, 1, make([]byte, 16), 4)
	goNote := note(le, "Go\x00\x00", 4, []byte("redacted"), 4)
	property := note(le, gnuOwner, 5, make([]byte, 16), 8)

	cases := []struct {
		name  string
		image []byte
		want  string
	}{
		{
			name:  "the note alone",
			image: elf64(le, segment{typ: ptNote, align: 4, data: note(le, gnuOwner, ntGNUBuildID, sha1ID, 4)}),
			want:  sha1Hex,
		},
		{
			name: "after other notes in the same segment, as the Go linker lays it out",
			image: elf64(le,
				segment{typ: 1, align: 4096, data: []byte("loadable")},
				segment{typ: ptNote, align: 8, data: property},
				segment{typ: ptNote, align: 4, data: bytes.Join([][]byte{abiTag, goNote, note(le, gnuOwner, ntGNUBuildID, sha1ID, 4)}, nil)},
			),
			want: sha1Hex,
		},
		{
			// What Go's own linker writes, and so what every image ships: the
			// build ID is in a note SECTION that no PT_NOTE segment covers.
			name: "in a section only, the PT_NOTE segment holding the Go note alone",
			image: elf64(le,
				segment{typ: ptNote, align: 4, data: goNote},
				segment{section: true, typ: shtNote, align: 4, data: goNote},
				segment{section: true, typ: shtNote, align: 4, data: note(le, gnuOwner, ntGNUBuildID, sha1ID, 4)},
				segment{section: true, typ: 1, align: 16, data: []byte("text")},
			),
			want: sha1Hex,
		},
		{
			name: "a damaged section table does not hide a note the segments still name",
			image: elf64(le,
				segment{section: true, typ: shtNote, align: 4, data: goNote, filesz: 4096},
				segment{typ: ptNote, align: 4, data: note(le, gnuOwner, ntGNUBuildID, sha1ID, 4)},
			),
			want: sha1Hex,
		},
		{
			name:  "big-endian",
			image: elf64(be, segment{typ: ptNote, align: 4, data: note(be, gnuOwner, ntGNUBuildID, sha1ID, 4)}),
			want:  sha1Hex,
		},
		{
			name:  "an 8-aligned segment pads the owner to 8 bytes",
			image: elf64(le, segment{typ: ptNote, align: 8, data: bytes.Join([][]byte{property, note(le, gnuOwner, ntGNUBuildID, sha1ID, 8)}, nil)}),
			want:  sha1Hex,
		},
		{
			name:  "a 32-byte (sha256) descriptor",
			image: elf64(le, segment{typ: ptNote, align: 4, data: note(le, gnuOwner, ntGNUBuildID, bytes.Repeat([]byte{0xab}, 32), 4)}),
			want:  strings.Repeat("ab", 32),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildID(bytes.NewReader(tc.image))
			if err != nil {
				t.Fatalf("BuildID: %v", err)
			}
			if got != tc.want {
				t.Fatalf("BuildID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildID_None(t *testing.T) {
	le := binary.ByteOrder(binary.LittleEndian)
	full := note(le, gnuOwner, ntGNUBuildID, sha1ID, 4)
	withNote := elf64(le, segment{typ: ptNote, align: 4, data: full})

	elf32 := bytes.Clone(withNote)
	elf32[4] = 1
	badOrder := bytes.Clone(withNote)
	badOrder[5] = 9
	manyHeaders := bytes.Clone(withNote)
	le.PutUint16(manyHeaders[56:58], maxTableEntries+1)
	shortEntries := bytes.Clone(withNote)
	le.PutUint16(shortEntries[54:56], progHdrLen-1)

	cases := []struct {
		name  string
		image []byte
	}{
		{"empty file", nil},
		{"not ELF", bytes.Repeat([]byte("#!/bin/sh\n"), 10)},
		{"32-bit ELF", elf32},
		{"unknown byte order", badOrder},
		{"more program headers than any real binary", manyHeaders},
		{"program header entries shorter than Elf64_Phdr", shortEntries},
		{"no PT_NOTE segment", elf64(le, segment{typ: 1, align: 4096, data: []byte("loadable")})},
		{"a PT_NOTE segment with other notes only", elf64(le, segment{typ: ptNote, align: 4, data: note(le, gnuOwner, 1, make([]byte, 16), 4)})},
		{"a build-ID-typed note owned by someone else", elf64(le, segment{typ: ptNote, align: 4, data: note(le, "Go\x00\x00", ntGNUBuildID, sha1ID, 4)})},
		{"an empty descriptor", elf64(le, segment{typ: ptNote, align: 4, data: note(le, gnuOwner, ntGNUBuildID, nil, 4)})},
		{"a descriptor longer than any digest", elf64(le, segment{typ: ptNote, align: 4, data: note(le, gnuOwner, ntGNUBuildID, make([]byte, maxIDLen+4), 4)})},
		// The three truncations: the descriptor cut short inside the segment,
		// the note header cut short, and a segment that declares more bytes
		// than the file holds.
		{"the descriptor runs past the segment", elf64(le, segment{typ: ptNote, align: 4, data: full[:len(full)-8]})},
		{"the note header is cut short", elf64(le, segment{typ: ptNote, align: 4, data: full[:8]})},
		{"the segment runs past the end of the file", elf64(le, segment{typ: ptNote, align: 4, data: full, filesz: uint64(len(full)) + 64})},
		{"a note segment larger than the read limit", elf64(le, segment{typ: ptNote, align: 4, data: full, filesz: maxNoteBytes + 1})},
		{"the program header table lies past the end of the file", withNote[:elfHeaderLen+8]},
		{"a note section that runs past the end of the file, and no segment", elf64(le, segment{section: true, typ: shtNote, align: 4, data: full, filesz: uint64(len(full)) + 64})},
		{"a note section with other notes only, and no segment", elf64(le, segment{section: true, typ: shtNote, align: 4, data: note(le, "Go\x00\x00", 4, []byte("redacted"), 4)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildID(bytes.NewReader(tc.image))
			if !errors.Is(err, ErrNoBuildID) {
				t.Fatalf("BuildID = %q, %v; want ErrNoBuildID", got, err)
			}
			if got != "" {
				t.Fatalf("BuildID returned %q with an error", got)
			}
		})
	}
}

// referenceBuildID reads the GNU build ID the way debug/elf sees it, from the
// section table: an independent reading of the same file.
func referenceBuildID(t *testing.T, path string) string {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Skipf("the test binary is not an ELF file (%v)", err)
	}
	defer func() { _ = f.Close() }()
	sec := f.Section(".note.gnu.build-id")
	if sec == nil {
		return ""
	}
	data, err := sec.Data()
	if err != nil {
		t.Fatalf("reading .note.gnu.build-id: %v", err)
	}
	namesz, descsz := f.ByteOrder.Uint32(data[0:4]), f.ByteOrder.Uint32(data[4:8])
	start := 12 + (namesz+3)&^3
	return hex.EncodeToString(data[start : start+descsz])
}

// TestVersion_OwnExecutable reads a real linked ELF: this test binary.
func TestVersion_OwnExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the release binaries are Linux ELF files")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	want := referenceBuildID(t, exe)
	if want == "" {
		if got := Version(); got != Unknown {
			t.Fatalf("Version() = %q for a binary with no build ID note, want %q", got, Unknown)
		}
		return
	}
	got, err := FileBuildID(exe)
	if err != nil {
		t.Fatalf("FileBuildID(%s): %v", exe, err)
	}
	if got != want {
		t.Fatalf("FileBuildID = %q, debug/elf reads %q", got, want)
	}
	if v := Version(); v != want {
		t.Fatalf("Version() = %q, want this binary's build ID %q", v, want)
	}
	if v := Version(); v != want {
		t.Fatalf("second Version() = %q, want %q", v, want)
	}
}

func TestFileBuildID_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := FileBuildID(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: got %v, want os.ErrNotExist", err)
	}
	script := filepath.Join(dir, "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileBuildID(script); !errors.Is(err, ErrNoBuildID) {
		t.Fatalf("a shell script: got %v, want ErrNoBuildID", err)
	}
}

// csiVendorVersion is what the CSI plugin reports its version as: a required,
// opaque string. The spec bounds every string field at 128 bytes, and the
// kubelet logs it; lowercase hex of at most 64 bytes fits both.
var printable = regexp.MustCompile(`^[0-9a-f]+$|^dev$`)

func TestVersion_Shape(t *testing.T) {
	v := Version()
	if v == "" || len(v) > 128 || !printable.MatchString(v) {
		t.Fatalf("Version() = %q: want non-empty lowercase hex (or %q) of at most 128 bytes", v, Unknown)
	}
}

func TestDescribe(t *testing.T) {
	goLine := runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
	cases := []struct {
		name                               string
		mainPackage, module, moduleVersion string
		want                               string
	}{
		{
			name:        "a Bazel build records the main package and no module",
			mainPackage: "aethermesh.dev/agent/cmd/agent",
			want:        "agent build-id abc123\npackage aethermesh.dev/agent/cmd/agent\n" + goLine,
		},
		{
			name:        "a go build records the module and its version",
			mainPackage: "aethermesh.dev/agent/cmd/agent", module: "aethermesh.dev", moduleVersion: "v1.2.3",
			want: "agent build-id abc123\npackage aethermesh.dev/agent/cmd/agent\nmodule aethermesh.dev v1.2.3\n" + goLine,
		},
		{
			name:   "a module with no version",
			module: "aethermesh.dev",
			want:   "agent build-id abc123\nmodule aethermesh.dev (devel)\n" + goLine,
		},
		{
			name: "no build information at all",
			want: "agent build-id abc123\n" + goLine,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describe("agent", "abc123", tc.mainPackage, tc.module, tc.moduleVersion); got != tc.want {
				t.Fatalf("describe =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}

	got := Describe("agent")
	if !strings.HasPrefix(got, "agent build-id "+Version()+"\n") || !strings.HasSuffix(got, goLine) {
		t.Fatalf("Describe = %q: want the build ID first and the toolchain last", got)
	}
	if strings.Contains(got, "{{") {
		t.Fatalf("Describe = %q: it is used as a cobra template and must hold no template action", got)
	}
}
