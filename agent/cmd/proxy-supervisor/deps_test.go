package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenPackages must never be reachable from proxy-supervisor.
//
// These are exactly the payloads that made the pre-#772 supervisor 65MiB: it was
// a subcommand of the agent, so the proxy pod staged and ran the agent's whole
// link set to fork a child process. The supervisor talks to Envoy over its admin
// endpoint and to a file on disk; it has no Kubernetes client, no xDS server and
// no SPIRE identity of its own, and acquiring any of them would mean the carve-out
// has silently been undone.
//
// Unlike //agent/cmd/proxy-ready this binary is NOT stdlib-only — cobra, fsnotify
// and the OTel metric SDK are load-bearing — so the list names the heavyweights
// rather than forbidding everything.
var forbiddenPackages = []string{
	"sigs.k8s.io/controller-runtime",
	"k8s.io/client-go",
	"k8s.io/apimachinery",
	"sigs.k8s.io/gateway-api",
	"github.com/envoyproxy/go-control-plane",
	"github.com/spiffe/go-spiffe",
	"github.com/miekg/dns",
}

// maxBinaryBytes is a bloat ceiling, not a target. The carved-out binary is
// ~15.4MiB and the agent binary it replaced is ~65MiB; this catches a
// re-acquired heavyweight that somehow evades the package-path scan, without
// tripping on toolchain drift.
const maxBinaryBytes = 24 * 1024 * 1024

// TestProxySupervisorLinksNothingHeavy is the linkage guard for #772 phase B2.
//
// The proxy pod's initContainer stages this binary onto a shared volume and the
// proxy container runs it as PID 1, so every linked package is paid in Go package
// init() on every supervisor start and every hot-restart epoch. init() runs before
// main() is entered, so no argv check can skip it — the only fix is to not link
// it, and the only way that stays true is if it is asserted.
//
// This inspects the linked ELF rather than the build graph (`bazel query
// deps(...)` cannot run inside the test sandbox, and the binary is what actually
// ships). Go embeds every linked package's import path in the binary's
// function-name table, so a forbidden package that is linked is a forbidden
// package that is greppable. scripts/check-proxy-supervisor-deps.sh is the
// complementary build-graph half.
func TestProxySupervisorLinksNothingHeavy(t *testing.T) {
	path := supervisorBinary(t)

	binary, err := os.ReadFile(path)
	require.NoError(t, err, "reading the linked proxy-supervisor binary")

	// Control test: prove we are scanning a real Go binary with readable import
	// paths, so a wrong path or a stripped table cannot make every assertion
	// below pass vacuously.
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/agent/internal/proxy/hotrestart")),
		"%s does not contain the hotrestart import path — the scan cannot be trusted", path)

	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"proxy-supervisor links %s. It supervises an Envoy child process: it has no "+
				"Kubernetes client, no xDS server and no SPIRE identity, and linking one "+
				"undoes the #772 carve-out that took this binary from 65MiB to single digits "+
				"of MiB. Fix the import; do not relax this test.", pkg)
	}

	assert.LessOrEqual(t, len(binary), maxBinaryBytes,
		"proxy-supervisor is %d bytes, over the %d-byte ceiling; the point of this binary is to be small",
		len(binary), maxBinaryBytes)
	t.Logf("proxy-supervisor is %d bytes", len(binary))
}

// supervisorBinary locates the linked binary in the test's runfiles.
func supervisorBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"agent/cmd/proxy-supervisor/proxy-supervisor_/proxy-supervisor",
		"agent/cmd/proxy-supervisor/proxy-supervisor",
	}

	var candidates []string
	if srcdir, workspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); srcdir != "" {
		for _, rel := range relPaths {
			candidates = append(candidates, filepath.Join(srcdir, workspace, rel))
		}
	}
	candidates = append(candidates, relPaths...)

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Fatalf("proxy-supervisor binary not found in runfiles; looked in %v", candidates)
	return ""
}
