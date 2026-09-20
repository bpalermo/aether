// Package envoybin locates the pinned aether-proxy Envoy binary inside a Bazel
// test's runfiles tree.
//
// The binary is NOT a stock Envoy release: //bazel/proxy_pin extracts
// /usr/local/bin/envoy from the aether-proxy image at the digest
// //charts/aether:values.yaml pins, so a test that runs it exercises the exact
// build the mesh deploys — most upstream extensions compiled out (#709). Moving
// the chart pin moves every gate that uses this; there is no version to keep in
// sync here.
//
// A target that calls Path must declare the architecture-appropriate binary in
// its `data`, e.g. data = ["//test/envoybin:envoy_bin"].
package envoybin

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrUnsupportedArch is returned when no pinned Envoy exists for the running
// architecture. Callers should skip rather than fail.
type ErrUnsupportedArch struct{ GOARCH string }

func (e *ErrUnsupportedArch) Error() string {
	return fmt.Sprintf("no pinned envoy binary for GOARCH=%s", e.GOARCH)
}

// Path returns the absolute path of the pinned Envoy binary.
func Path() (string, error) {
	var repoName string
	switch runtime.GOARCH {
	case "amd64":
		repoName = "pinned_envoy_linux_amd64"
	case "arm64":
		repoName = "pinned_envoy_linux_arm64"
	default:
		return "", &ErrUnsupportedArch{GOARCH: runtime.GOARCH}
	}

	runfiles := os.Getenv("RUNFILES_DIR")
	if runfiles == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("os.Executable: %w", err)
		}
		runfiles = exe + ".runfiles"
	}

	p := filepath.Join(runfiles, canonicalRepo(runfiles, repoName), "envoy")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("envoy binary not found at %s (RUNFILES_DIR=%s): %w", p, runfiles, err)
	}
	return p, nil
}

// canonicalRepo resolves a user-visible repository name to its canonical bzlmod
// name by reading the _repo_mapping file in the runfiles directory.
//
// In Bazel 9 bzlmod, repos created by a module extension have a canonical name
// like "+pinned_proxy+<repo-name>" rather than just "<repo-name>". Falling back
// to the apparent name keeps this working on pre-bzlmod layouts.
func canonicalRepo(runfiles, apparent string) string {
	for _, name := range []string{"_repo_mapping", filepath.Join("_main", "_repo_mapping")} {
		if canonical, ok := lookupMapping(filepath.Join(runfiles, name), apparent); ok {
			return canonical
		}
	}
	return apparent
}

// lookupMapping reads a Bazel repo mapping file ("<from-canonical>,<apparent>,
// <to-canonical>" per line) and returns the canonical name for apparent in the
// main workspace context (an empty from-canonical).
func lookupMapping(path, apparent string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ",", 3)
		if len(parts) != 3 {
			continue
		}
		if from, app, to := parts[0], parts[1], parts[2]; from == "" && app == apparent {
			return to, true
		}
	}
	return "", false
}
