package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "stamp":
		err = runStamp(os.Args[2:])
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
  buildid stamp  -in FILE -out FILE [-status-file FILE]
  buildid verify [-status-file FILE] -marker FILE FILE...
`)
	os.Exit(2)
}

// runStamp copies -in to -out, rewriting the GNU build-ID note to the release
// commit SHA read from -status-file.
//
// Fallbacks, all of which produce a byte-identical copy of the input rather than
// an error, so that unstamped and non-Go/non-ELF inputs keep building:
//   - no -status-file (i.e. --nostamp): copy unchanged;
//   - status file with no resolvable commit SHA: copy unchanged;
//   - input is not an ELF image (e.g. a Mach-O host build, or a data file
//     riding in an image layer): copy unchanged.
//
// An ELF input that *is* being stamped but carries no build-ID note is a hard
// error: that would silently ship an unsymbolizable binary.
func runStamp(args []string) error {
	fs := flag.NewFlagSet("stamp", flag.ExitOnError)
	in := fs.String("in", "", "input binary")
	out := fs.String("out", "", "output binary")
	statusFile := fs.String("status-file", "", "Bazel stable workspace status file (empty: do not stamp)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("stamp: -in and -out are required")
	}

	commit, err := readCommit(*statusFile)
	if err != nil {
		return err
	}

	buf, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	info, err := os.Stat(*in)
	if err != nil {
		return err
	}

	switch {
	case commit == "":
		fmt.Fprintf(os.Stderr, "buildid: no release commit in the workspace status; leaving %s untouched\n", *in)
	case !isELF(buf):
		fmt.Fprintf(os.Stderr, "buildid: %s is not an ELF image; leaving it untouched\n", *in)
	default:
		id, err := hex.DecodeString(commit)
		if err != nil {
			return fmt.Errorf("decoding commit %q: %w", commit, err)
		}
		if err := patch(buf, id); err != nil {
			return fmt.Errorf("%s: %w", *in, err)
		}
	}

	return os.WriteFile(*out, buf, info.Mode().Perm())
}

// runVerify asserts that every named ELF binary carries a GNU build-ID note and,
// when the build is stamped, that the note equals the release commit SHA. It is
// the build-time guard for issue #651: wire it into a target that CI builds and
// a regression can never reach a published image.
func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	statusFile := fs.String("status-file", "", "Bazel stable workspace status file (empty: presence check only)")
	marker := fs.String("marker", "", "file to write on success")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		return errors.New("verify: no files given")
	}

	want, err := wantBuildID(*statusFile)
	if err != nil {
		return err
	}

	var report bytes.Buffer
	var failures int
	for _, path := range files {
		got, err := checkOne(path, want)
		if err != nil {
			fmt.Fprintf(&report, "%s: %v\n", path, err)
			failures++
			continue
		}
		fmt.Fprintf(&report, "%s: %s\n", path, got)
	}
	if failures > 0 {
		return fmt.Errorf("%d binary(ies) failed the GNU build-ID check (see #651):\n%s", failures, report.String())
	}
	if *marker != "" {
		return os.WriteFile(*marker, report.Bytes(), 0o644)
	}
	_, err = os.Stdout.Write(report.Bytes())
	return err
}

// wantBuildID returns the build-ID every checked binary must carry, or nil when
// the build is not stamped (presence check only).
func wantBuildID(statusFile string) ([]byte, error) {
	commit, err := readCommit(statusFile)
	if err != nil || commit == "" {
		return nil, err
	}
	want, err := hex.DecodeString(commit)
	if err != nil {
		return nil, fmt.Errorf("decoding commit %q: %w", commit, err)
	}
	return want, nil
}

// checkOne returns the hex build-ID of path, or an error describing why it is
// missing or wrong.
func checkOne(path string, want []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	got, err := readBuildID(f)
	if err != nil {
		return "", err
	}
	if want != nil && !bytes.Equal(got, want) {
		return "", fmt.Errorf("build-ID is %x, want the release commit %x", got, want)
	}
	return fmt.Sprintf("%x", got), nil
}

func isELF(buf []byte) bool {
	return len(buf) >= 4 && bytes.Equal(buf[:4], []byte("\x7fELF"))
}
