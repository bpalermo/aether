package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenPackages must never be reachable from mesh-dns-ready. "k8s.io/"
// covers sigs.k8s.io/controller-runtime and the client-go/apimachinery family in
// one pattern; the explicit controller-runtime entry is kept so the failure
// names the usual culprit directly. The last three are what the mesh-dns daemon
// itself links and what this binary exists to stop re-executing every 15s: the
// OTel SDK (exporters, batch log processor, runtime instrumentation), the DNS
// library, and the filesystem watcher.
var forbiddenPackages = []string{
	"sigs.k8s.io/controller-runtime",
	"k8s.io/",
	"google.golang.org/protobuf",
	"github.com/envoyproxy/go-control-plane",
	"github.com/spf13/cobra",
	"go.opentelemetry.io/otel",
	"github.com/miekg/dns",
	"github.com/fsnotify/fsnotify",
}

// maxBinaryBytes is a bloat ceiling, not a target. The stdlib-only binary is
// ~1.7MB and the mesh-dns daemon it replaced on the probe path is ~16.9MB; 8MB
// catches an accidental heavyweight dependency that somehow evades the
// package-path scan, without tripping on toolchain drift. It is deliberately
// TIGHTER than proxy-ready's 16MB ceiling: 16MB would not even notice this
// binary growing into the whole daemon.
const maxBinaryBytes = 8 * 1024 * 1024

// TestMeshDNSReadyLinksNothingHeavy is the linkage guard for #683.
//
// The entire value of this binary is what it does NOT link. The probe it
// replaced re-exec'd /mesh-dns — the 16.9MB resolver daemon — once every 15s per
// pod, and the 2026-09-05 profiling pass attributed ~10 core-seconds per 25
// minutes fleet-wide to that: container exec (runc/libcontainer) plus a second
// runtime.main and Go package init. init() runs before main() is entered, so no
// argv check can skip it: the only fix is to not link those packages, and the
// only way that stays true is if it is asserted.
//
// This inspects the linked ELF rather than the build graph (`bazel query
// deps(...)` cannot run inside the test sandbox, and the binary is what actually
// ships). Go embeds every linked package's import path in the binary's
// function-name table, so a forbidden package that is linked is a forbidden
// package that is greppable.
func TestMeshDNSReadyLinksNothingHeavy(t *testing.T) {
	path := meshDNSReadyBinary(t)

	binary, err := os.ReadFile(path)
	require.NoError(t, err, "reading the linked mesh-dns-ready binary")

	// Control test: prove we are scanning a real Go binary with readable import
	// paths, so a wrong path or a stripped table cannot make every assertion
	// below pass vacuously.
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/common/readymarker")),
		"%s does not contain the readymarker import path — the scan cannot be trusted", path)

	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"mesh-dns-ready links %s. It must stay stdlib-only (plus //common/readymarker): "+
				"an init()-heavy dependency here is exactly the cost #683 removed. "+
				"Fix the import; do not relax this test.", pkg)
	}

	assert.LessOrEqual(t, len(binary), maxBinaryBytes,
		"mesh-dns-ready is %d bytes, over the %d-byte ceiling; the point of this binary is to be small",
		len(binary), maxBinaryBytes)
	t.Logf("mesh-dns-ready is %d bytes", len(binary))
}

// meshDNSReadyBinary locates the linked binary in the test's runfiles.
func meshDNSReadyBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"agent/cmd/mesh-dns-ready/mesh-dns-ready_/mesh-dns-ready",
		"agent/cmd/mesh-dns-ready/mesh-dns-ready",
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
	t.Fatalf("mesh-dns-ready binary not found in runfiles; looked in %v", candidates)
	return ""
}
