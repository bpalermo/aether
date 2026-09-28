package main

import (
	"bytes"
	"debug/buildinfo"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allowedModules is the COMPLETE set of modules identity-ready may link (#1053).
// It is an allow-list, not a deny-list: identity-ready runs as an init
// container in every mesh-managed pod, so each module added here is paid on
// every pod start across the fleet. gRPC + protobuf + go-spiffe's GENERATED
// Workload API client are the floor for talking to the SPIRE agent; everything
// else in the list is what those three pull in.
//
// Adding a module is a deliberate act: justify it in the PR. In particular,
// go-spiffe's workloadapi/x509svid packages are deliberately NOT used (they
// pull go-jose for JWT-SVIDs, which a "has an X.509 SVID been issued" check
// does not need), and nothing from k8s.io, controller-runtime, cobra or OTel
// belongs here.
var allowedModules = map[string]bool{
	"github.com/spiffe/go-spiffe/v2":            true, // proto/spiffe/workload only
	"github.com/golang/protobuf":                true, // go-spiffe's generated code
	"google.golang.org/grpc":                    true,
	"google.golang.org/protobuf":                true,
	"google.golang.org/genproto/googleapis/rpc": true, // grpc status
	"golang.org/x/net":                          true, // grpc http2
	"golang.org/x/sys":                          true,
	"golang.org/x/text":                         true, // x/net idna
}

// forbiddenPackages are package paths that must never appear in the binary,
// even under an allowed module: go-spiffe's high-level client is the go-jose
// path the allow-list comment above rules out.
var forbiddenPackages = []string{
	"github.com/spiffe/go-spiffe/v2/workloadapi",
	"github.com/spiffe/go-spiffe/v2/svid",
	"github.com/go-jose",
	"k8s.io/",
	"github.com/spf13/cobra",
	"go.opentelemetry.io/otel",
}

// maxBinaryBytes is a bloat ceiling, not a target: ~11.5MB today (gRPC is most
// of it), against the 65MB+ agent binary the image also carries.
const maxBinaryBytes = 16 * 1024 * 1024

// TestIdentityReadyLinksOnlyAllowedModules asserts the allow-list against the
// linked ELF that actually ships, via the module list rules_go embeds in its
// build info.
func TestIdentityReadyLinksOnlyAllowedModules(t *testing.T) {
	path := identityReadyBinary(t)

	info, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "reading build info from %s", path)
	require.Equal(t, "aethermesh.dev/agent/cmd/identity-ready", info.Path)
	// Control: the dependency table must be populated, or every assertion
	// below passes vacuously.
	require.NotEmpty(t, info.Deps, "no module deps in the build info — the scan cannot be trusted")

	var linked []string
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
		assert.True(t, allowedModules[dep.Path],
			"identity-ready links module %s, which is not on the allow-list. It runs in EVERY mesh pod's "+
				"init; keep it to gRPC + the generated Workload API client. Fix the import, or justify the "+
				"module and add it to allowedModules.", dep.Path)
	}
	sort.Strings(linked)
	t.Logf("identity-ready links %d modules: %v", len(linked), linked)

	binary, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(binary, []byte("github.com/spiffe/go-spiffe/v2/proto/spiffe/workload")),
		"%s does not contain the Workload API proto import path — the package scan cannot be trusted", path)
	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)), "identity-ready links %s; it must not", pkg)
	}

	assert.LessOrEqual(t, len(binary), maxBinaryBytes,
		"identity-ready is %d bytes, over the %d-byte ceiling", len(binary), maxBinaryBytes)
	t.Logf("identity-ready is %d bytes", len(binary))
}

// identityReadyBinary locates the linked binary in the test's runfiles.
func identityReadyBinary(t *testing.T) string {
	t.Helper()

	relPaths := []string{
		"agent/cmd/identity-ready/identity-ready_/identity-ready",
		"agent/cmd/identity-ready/identity-ready",
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
	t.Fatalf("identity-ready binary not found in runfiles; looked in %v", candidates)
	return ""
}
