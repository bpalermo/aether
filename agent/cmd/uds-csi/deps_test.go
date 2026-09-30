package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenPackages must never be reachable from uds-csi.
//
// The node plugin runs privileged, as root, with Bidirectional mount
// propagation into the host: it is the most privileged process aether runs, so
// what it links is attack surface, not just megabytes. Its whole job is two
// gRPC services on Unix sockets (CSI Identity/Node for the kubelet, and the
// kubelet plugin-registration API) plus mount(2)/umount2(2).
// It makes no Kubernetes API calls, so it has no business linking a Kubernetes
// client; it has no xDS server and no SPIRE identity (SPIRE is not involved at
// all — it must work with spire.enabled=false).
//
// The CSI spec module, grpc and protobuf are load-bearing, so this names the
// heavyweights rather than forbidding everything (the proxy-supervisor shape).
var forbiddenPackages = []string{
	"sigs.k8s.io/controller-runtime",
	"k8s.io/client-go",
	"k8s.io/apimachinery",
	"k8s.io/api/",
	"sigs.k8s.io/gateway-api",
	"github.com/envoyproxy/go-control-plane",
	"github.com/spiffe/go-spiffe",
	"github.com/miekg/dns",
	"github.com/spf13/cobra",
	"go.opentelemetry.io/otel/sdk",
}

// maxBinaryBytes is a bloat ceiling, not a target: the plugin is ~11MiB (grpc +
// protobuf + the CSI spec). It catches a re-acquired heavyweight that evades the
// package-path scan without tripping on toolchain drift.
const maxBinaryBytes = 20 * 1024 * 1024

// TestUDSCSILinksNothingHeavy inspects the linked ELF rather than the build
// graph (`bazel query deps(...)` cannot run inside the test sandbox, and the
// binary is what ships). Go embeds every linked package's import path in the
// binary's function-name table, so a forbidden package that is linked is a
// forbidden package that is greppable. scripts/check-uds-csi-deps.sh is the
// complementary build-graph half.
func TestUDSCSILinksNothingHeavy(t *testing.T) {
	path := udsCSIBinary(t)

	binary, err := os.ReadFile(path)
	require.NoError(t, err, "reading the linked uds-csi binary")

	// Control test: prove we are scanning a real Go binary with readable import
	// paths, so a wrong path or a stripped table cannot make every assertion
	// below pass vacuously.
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/agent/internal/udscsi")),
		"%s does not contain the udscsi import path — the scan cannot be trusted", path)
	require.True(t, bytes.Contains(binary, []byte("github.com/container-storage-interface/spec/lib/go/csi")),
		"%s does not contain the CSI spec import path — the scan cannot be trusted", path)

	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"uds-csi links %s. It is a privileged node plugin that talks only to the kubelet "+
				"(two Unix sockets) and the kernel; every package it links is attack surface in "+
				"the most privileged aether process. Fix the import; do not relax this test.", pkg)
	}

	assert.LessOrEqual(t, len(binary), maxBinaryBytes,
		"uds-csi is %d bytes, over the %d-byte ceiling; the point of this binary is to be small",
		len(binary), maxBinaryBytes)
	t.Logf("uds-csi is %d bytes", len(binary))
}

// udsCSIBinary locates the linked binary in the test's runfiles.
func udsCSIBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"agent/cmd/uds-csi/uds-csi_/uds-csi",
		"agent/cmd/uds-csi/uds-csi",
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
	t.Fatalf("uds-csi binary not found in runfiles; looked in %v", candidates)
	return ""
}
