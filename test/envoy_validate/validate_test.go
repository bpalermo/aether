// Package envoy_validate runs "envoy --mode validate" over aether-generated
// bootstrap configs to catch "Envoy would NACK this" regressions before they
// reach production.
//
// The Envoy binary is provided as a Bazel data dependency (http_file in
// MODULE.bazel, pinned to ENVOY_VERSION = 1.38.0).  To run the test:
//
//	bazel test //test/envoy_validate:envoy_validate_test
//	bazel test //test/envoy_validate:envoy_validate_test --test_output=all
//
// What the test catches (examples from production incidents):
//   - ORIGINAL_DST cluster with ROUND_ROBIN lb_policy      → CDS NACK, exit 1
//   - Listener with no address field                        → LDS NACK, exit 1
//   - Malformed / missing SAN in TLS validation context     → config rejected
//   - Unknown TypedConfig @type URL (non-stripped filter)   → config rejected
package envoy_validate

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// envoyBinary returns the path to the Envoy binary from the Bazel runfiles tree.
//
// In Bazel 9 bzlmod, http_file repos created via use_repo_rule have a canonical
// name like "+http_file+<repo-name>" rather than just "<repo-name>".  The repo
// mapping file (runfiles/_repo_mapping or .runfiles.repo_mapping) translates the
// user-visible name to the canonical name.  This function reads that mapping so
// the lookup is robust across Bazel versions.
func envoyBinary(t *testing.T) string {
	t.Helper()

	// Determine the architecture-specific user-visible repo name.
	var repoName string
	switch runtime.GOARCH {
	case "amd64":
		repoName = "envoy_binary_linux_amd64"
	case "arm64":
		repoName = "envoy_binary_linux_arm64"
	default:
		t.Skipf("envoy binary not available for GOARCH=%s", runtime.GOARCH)
	}

	runfiles := os.Getenv("RUNFILES_DIR")
	if runfiles == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		runfiles = exe + ".runfiles"
	}

	// Resolve the canonical repo name from the repo mapping file.
	// Format: "<from-canonical>,<apparent>,<to-canonical>"
	// We want lines starting with "," (main repo context) mapping our user name.
	canonical := canonicalRepo(t, runfiles, repoName)

	p := filepath.Join(runfiles, canonical, "file", "envoy")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("envoy binary not found at %s: %v\n(RUNFILES_DIR=%s)", p, err, runfiles)
	}
	return p
}

// canonicalRepo resolves a user-visible repository name to its canonical bzlmod
// name by reading the _repo_mapping file in the runfiles directory.
func canonicalRepo(t *testing.T, runfiles, apparent string) string {
	t.Helper()

	// Try the direct path first (for when the file is in the runfiles root).
	for _, name := range []string{"_repo_mapping", filepath.Join("_main", "_repo_mapping")} {
		mappingPath := filepath.Join(runfiles, name)
		canonical, ok := lookupMapping(t, mappingPath, apparent)
		if ok {
			return canonical
		}
	}

	// Fall back to the direct name (pre-bzlmod or old Bazel versions).
	t.Logf("no repo mapping found for %q; falling back to direct name", apparent)
	return apparent
}

// lookupMapping reads a Bazel repo mapping file and returns the canonical name
// for the given apparent name in the main workspace context ("" or "_main").
func lookupMapping(t *testing.T, path, apparent string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		from, app, to := parts[0], parts[1], parts[2]
		// Lines with empty from-canonical are from the main workspace context.
		if from == "" && app == apparent {
			return to, true
		}
	}
	return "", false
}

// TestEnvoyValidate generates three representative aether bootstrap configs
// and validates each one with "envoy --mode validate".
//
// Envoy exits 0 when the config is structurally valid; exits 1 on any error.
func TestEnvoyValidate(t *testing.T) {
	envoy := envoyBinary(t)

	outDir := t.TempDir()

	builders := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"node_bootstrap.json", NodeBootstrapJSON},
		{"capture_bootstrap.json", CaptureBootstrapJSON},
		{"capture_route_target_bootstrap.json", CaptureRouteTargetBootstrapJSON},
		{"edge_bootstrap.json", EdgeBootstrapJSON},
	}

	// Write all bootstrap files.
	for _, b := range builders {
		data, err := b.fn()
		if err != nil {
			t.Fatalf("build %s: %v", b.name, err)
		}
		if err := os.WriteFile(filepath.Join(outDir, b.name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", b.name, err)
		}
	}

	// Validate each bootstrap with Envoy.
	for _, b := range builders {
		b := b
		t.Run(b.name, func(t *testing.T) {
			path := filepath.Join(outDir, b.name)
			cmd := exec.Command(envoy, "--mode", "validate", "-c", path)
			out, err := cmd.CombinedOutput()
			t.Logf("envoy --mode validate %s:\n%s", b.name, out)
			if err != nil {
				t.Fatalf("envoy --mode validate failed for %s: %v", b.name, err)
			}
		})
	}
}
