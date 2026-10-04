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

// allowedModules is the COMPLETE set of modules mesh-dns may link: the 30 it
// linked when this guard was added. It is an allow-list, so a new module fails
// the build by name — including one that arrives transitively through a
// dependency bump.
//
// mesh-dns was carved out of the agent (#583) so the resolver every managed
// pod's :53 is DNAT'd to survives agent rolls and does not pull
// controller-runtime, go-control-plane, the CNI server and SPIRE onto every node
// for a process that executes none of them. Adding a module is a deliberate
// act: justify it in the PR.
var allowedModules = map[string]bool{
	// The daemon: DNS server, snapshot file watch, CLI.
	"github.com/miekg/dns":         true,
	"github.com/fsnotify/fsnotify": true,
	"github.com/spf13/cobra":       true,
	"github.com/spf13/pflag":       true,
	"golang.org/x/net":             true,
	"golang.org/x/sys":             true,
	"golang.org/x/text":            true,

	// common/log + telemetry: OTel SDK, slog bridge, runtime metrics, OTLP
	// log/metric exporters over gRPC.
	"go.opentelemetry.io/otel":                                          true,
	"go.opentelemetry.io/otel/metric":                                   true,
	"go.opentelemetry.io/otel/trace":                                    true,
	"go.opentelemetry.io/otel/log":                                      true,
	"go.opentelemetry.io/otel/sdk":                                      true,
	"go.opentelemetry.io/otel/sdk/log":                                  true,
	"go.opentelemetry.io/otel/sdk/metric":                               true,
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc":       true,
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc": true,
	"go.opentelemetry.io/contrib/bridges/otelslog":                      true,
	"go.opentelemetry.io/contrib/instrumentation/runtime":               true,
	"go.opentelemetry.io/proto/otlp":                                    true,
	"go.opentelemetry.io/auto/sdk":                                      true,
	"github.com/grpc-ecosystem/grpc-gateway/v2":                         true,
	"github.com/cenkalti/backoff/v5":                                    true,
	"github.com/cespare/xxhash/v2":                                      true,
	"github.com/go-logr/logr":                                           true,
	"github.com/go-logr/stdr":                                           true,
	"github.com/google/uuid":                                            true,
	"google.golang.org/grpc":                                            true,
	"google.golang.org/protobuf":                                        true,
	"google.golang.org/genproto/googleapis/api":                         true,
	"google.golang.org/genproto/googleapis/rpc":                         true,
}

// maxModules is the module budget: the size of the allow-list above. It is
// asserted separately so a change to the list has to change this number too,
// which makes a growing budget visible in review.
const maxModules = 30

// forbiddenPackages must never be reachable from mesh-dns, under any module:
// the agent payloads the #583 carve-out removed.
var forbiddenPackages = []string{
	"sigs.k8s.io/controller-runtime",
	"k8s.io/client-go",
	"k8s.io/apimachinery",
	"k8s.io/api/",
	"sigs.k8s.io/gateway-api",
	"github.com/envoyproxy/go-control-plane",
	"github.com/spiffe/go-spiffe",
	"github.com/containernetworking/cni",
}

// maxBinaryBytes is a bloat ceiling, not a target: mesh-dns is ~16.7MiB.
const maxBinaryBytes = 24 * 1024 * 1024

// TestMeshDNSLinksOnlyAllowedModules asserts the allow-list and the module
// budget against the linked ELF that ships in the mesh-dns image, via the
// module list rules_go embeds in its build info. scripts/check-mesh-dns-deps.sh
// is the complementary build-graph half.
func TestMeshDNSLinksOnlyAllowedModules(t *testing.T) {
	path := meshDNSBinary(t)

	info, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "reading build info from %s", path)
	require.Equal(t, "aethermesh.dev/agent/cmd/mesh-dns", info.Path)
	// Control: the dependency table must be populated, or every assertion
	// below passes vacuously.
	require.NotEmpty(t, info.Deps, "no module deps in the build info — the scan cannot be trusted")

	linked := make([]string, 0, len(info.Deps))
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
		assert.True(t, allowedModules[dep.Path],
			"mesh-dns links module %s, which is not on the allow-list. It is the node's resolver and "+
				"must stay the slim binary #583 carved out of the agent. Fix the import, or justify the "+
				"module and add it to allowedModules (and maxModules).", dep.Path)
	}
	sort.Strings(linked)
	t.Logf("mesh-dns links %d modules: %v", len(linked), linked)

	assert.Len(t, allowedModules, maxModules, "allowedModules and maxModules disagree; change both together")
	assert.LessOrEqual(t, len(linked), maxModules,
		"mesh-dns links %d modules, over the budget of %d", len(linked), maxModules)

	binary, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/agent/internal/meshdns")),
		"%s does not contain the meshdns import path — the package scan cannot be trusted", path)
	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"mesh-dns links %s. It answers DNS from a snapshot file the agent writes; it has no "+
				"Kubernetes client, no xDS server, no CNI and no SPIRE identity. Fix the import; do "+
				"not relax this test.", pkg)
	}

	if raceInstrumented {
		t.Logf("%s is %d bytes under -race; the %d-byte ceiling applies to plain builds only", "mesh-dns", len(binary), maxBinaryBytes)
	} else {
		assert.LessOrEqual(t, len(binary), maxBinaryBytes,
			"mesh-dns is %d bytes, over the %d-byte ceiling", len(binary), maxBinaryBytes)
	}
	t.Logf("mesh-dns is %d bytes", len(binary))
}

// meshDNSBinary locates the linked binary in the test's runfiles.
func meshDNSBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"agent/cmd/mesh-dns/mesh-dns_/mesh-dns",
		"agent/cmd/mesh-dns/mesh-dns",
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
	t.Fatalf("mesh-dns binary not found in runfiles; looked in %v", candidates)
	return ""
}
