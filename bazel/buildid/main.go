package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "set":
		err = runSet(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "buildid: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  buildid set    -in FILE -out FILE
  buildid verify [-marker FILE] FILE...
`)
	os.Exit(2)
}

// runSet copies -in to -out, rewriting the GNU build-ID note to the content hash
// of the image (see the package comment for the exact definition).
//
// A non-ELF input — a Mach-O host build, or a data file riding in an image
// layer — is copied unchanged rather than treated as an error, so the rule can
// be applied uniformly to everything that enters an image.
//
// An ELF input that carries no build-ID note at all is a hard error: that would
// silently ship an unsymbolizable binary.
func runSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	in := fs.String("in", "", "input binary")
	out := fs.String("out", "", "output binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("set: -in and -out are required")
	}

	buf, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	info, err := os.Stat(*in)
	if err != nil {
		return err
	}

	if isELF(buf) {
		if _, err := setContentBuildID(buf); err != nil {
			return fmt.Errorf("%s: %w", *in, err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "buildid: %s is not an ELF image; leaving it untouched\n", *in)
	}

	return os.WriteFile(*out, buf, info.Mode().Perm())
}

// runVerify is the build-time acceptance guard for #651 and #653. Over the set
// of binaries that ship in released images it asserts:
//
//  1. every one carries a 20-byte GNU build-ID note (#651: no note, no symbols);
//  2. every note equals the SHA-1 recomputed from that binary's own content —
//     self-verification, i.e. the ID really does identify *this* ELF and the
//     `content_build_id` rule actually ran on it;
//  3. the notes are pairwise distinct (#653: seven released ELFs must not share
//     one ID, or a symbol upload for one silently resolves all of them).
//
// Property 3 follows from 2 for distinct binaries, but is asserted directly
// because it is the property the Pyroscope workflow depends on and the one the
// previous, commit-stamped guard actively violated.
func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	marker := fs.String("marker", "", "file to write on success")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		return errors.New("verify: no files given")
	}

	var report bytes.Buffer
	var problems []string
	ids := map[string][]string{}
	for _, path := range files {
		id, err := checkOne(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		fmt.Fprintf(&report, "%s: %s\n", path, id)
		ids[id] = append(ids[id], path)
	}
	problems = append(problems, collisions(ids)...)

	if len(problems) > 0 {
		return fmt.Errorf("GNU build-ID check failed (see #651, #653):\n  %s", strings.Join(problems, "\n  "))
	}
	if *marker != "" {
		return os.WriteFile(*marker, report.Bytes(), 0o644)
	}
	_, err := os.Stdout.Write(report.Bytes())
	return err
}

// checkOne returns the hex build-ID of path after asserting that it equals the
// hash recomputed from path's own bytes.
func checkOne(path string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !isELF(buf) {
		return "", errors.New("not an ELF image")
	}
	got, err := readBuildID(bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	want, err := contentBuildID(buf)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(got, want) {
		return "", fmt.Errorf("build-ID is %x but its content hashes to %x — the note does not identify this binary", got, want)
	}
	return fmt.Sprintf("%x", got), nil
}

// collisions reports every build-ID claimed by more than one binary.
func collisions(ids map[string][]string) []string {
	var out []string
	for id, paths := range ids {
		if len(paths) > 1 {
			sort.Strings(paths)
			out = append(out, fmt.Sprintf("build-ID %s is shared by %d binaries: %s", id, len(paths), strings.Join(paths, ", ")))
		}
	}
	sort.Strings(out)
	return out
}
