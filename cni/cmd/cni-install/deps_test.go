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

// allowedModules is the COMPLETE set of modules cni-install may link. It is an
// allow-list, so a new module fails the build by name, including one that
// arrives transitively through a dependency bump. 9 when this guard was added;
// 8 since the binary copy moved from renameio to //common/file.
//
// cni-install is the agent pod's init container: it runs on every agent start,
// ahead of the agent, with the host's CNI directories mounted read-write. It
// copies one binary and chains one conflist entry, and its start-up is on the
// path to a node's agent serving. Adding a module is a deliberate act: justify
// it in the PR.
var allowedModules = map[string]bool{
	// The installer itself: CLI and the conflist watch. Atomic file replacement
	// is //common/file (stdlib and x/sys), which the rest of the tree uses.
	"github.com/spf13/cobra":       true,
	"github.com/spf13/pflag":       true,
	"github.com/fsnotify/fsnotify": true,
	"golang.org/x/sys":             true,

	// libcni, which //cni/conflist uses to find and parse the node's conflists
	// (libcni.ConfFiles, libcni.ConfListFromFile).
	"github.com/containernetworking/cni": true,

	// Not ours: libcni's pkg/invoke imports go.opentelemetry.io/otel/propagation
	// to hand a trace context to the plugins it execs, and that pulls the OTel
	// API (no SDK, no exporter) and xxhash with it. cni-install exports no
	// telemetry and never calls that code. The three stay allowed because the
	// alternative is a conflist parser of our own; they are named here so that
	// nothing else from OpenTelemetry can ride in beside them.
	"go.opentelemetry.io/otel":       true,
	"go.opentelemetry.io/otel/trace": true,
	"github.com/cespare/xxhash/v2":   true,
}

// maxModules is the module budget: the size of the allow-list above. It is
// asserted separately so a change to the list has to change this number too,
// which makes a growing budget visible in review.
const maxModules = 8

// forbiddenPackages must never be reachable from cni-install, under any
// module. It reads a directory and writes two files: no Kubernetes client
// (controller-runtime's signal handler alone once linked client-go into it,
// #772 phase B1), no xDS, no SPIRE identity, no gRPC (it never talks to the
// agent; the plugin it installs does), and no telemetry of its own. The OTel
// API that libcni links is allowed above; the SDK, the exporters and the
// metric and log APIs are what an export path would need, and are not.
var forbiddenPackages = []string{
	"sigs.k8s.io/controller-runtime",
	"k8s.io/client-go",
	"k8s.io/apimachinery",
	"k8s.io/api/",
	"sigs.k8s.io/gateway-api",
	"github.com/envoyproxy/go-control-plane",
	"github.com/spiffe/go-spiffe",
	"github.com/miekg/dns",
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
	"go.opentelemetry.io/otel/sdk",
	"go.opentelemetry.io/otel/exporters",
	"go.opentelemetry.io/otel/metric",
	"go.opentelemetry.io/otel/log",
}

// maxBinaryBytes is a bloat ceiling, not a target: cni-install is ~7.2MiB on
// amd64 (~6.6MiB on arm64).
const maxBinaryBytes = 9 * 1024 * 1024

// TestCNIInstallLinksOnlyAllowedModules asserts the allow-list and the module
// budget against the linked ELF that ships in the cni-install image, via the
// module list rules_go embeds in its build info, then scans the same file for
// the import paths of the packages it must never link.
func TestCNIInstallLinksOnlyAllowedModules(t *testing.T) {
	path := cniInstallBinary(t)

	info, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "reading build info from %s", path)
	require.Equal(t, "aethermesh.dev/cni/cmd/cni-install", info.Path)
	// Control: the dependency table must be populated, or every assertion
	// below passes vacuously.
	require.NotEmpty(t, info.Deps, "no module deps in the build info: the scan cannot be trusted")

	linked := make([]string, 0, len(info.Deps))
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
		assert.True(t, allowedModules[dep.Path],
			"cni-install links module %s, which is not on the allow-list. It is an init container "+
				"that copies a binary and writes a conflist, on the path to every agent start. Fix "+
				"the import, or justify the module and add it to allowedModules (and maxModules).",
			dep.Path)
	}
	sort.Strings(linked)
	t.Logf("cni-install links %d modules: %v", len(linked), linked)

	assert.Len(t, allowedModules, maxModules, "allowedModules and maxModules disagree; change both together")
	assert.LessOrEqual(t, len(linked), maxModules,
		"cni-install links %d modules, over the budget of %d", len(linked), maxModules)

	binary, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/cni/internal/install")),
		"%s does not contain the installer import path: the package scan cannot be trusted", path)
	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"cni-install links %s. It copies the plugin onto the host and chains it into the "+
				"node's conflist; it has no Kubernetes client, no xDS server, no SPIRE identity, "+
				"no gRPC and no telemetry of its own. Fix the import; do not relax this test.", pkg)
	}

	if raceInstrumented {
		t.Logf("%s is %d bytes under -race; the %d-byte ceiling applies to plain builds only", "cni-install", len(binary), maxBinaryBytes)
	} else {
		assert.LessOrEqual(t, len(binary), maxBinaryBytes,
			"cni-install is %d bytes, over the %d-byte ceiling", len(binary), maxBinaryBytes)
	}
	t.Logf("cni-install is %d bytes", len(binary))
}

// cniInstallBinary locates the linked binary in the test's runfiles.
func cniInstallBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"cni/cmd/cni-install/cni-install_/cni-install",
		"cni/cmd/cni-install/cni-install",
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
	t.Fatalf("cni-install binary not found in runfiles; looked in %v", candidates)
	return ""
}
