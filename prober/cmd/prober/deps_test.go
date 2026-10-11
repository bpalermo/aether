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

// allowedModules is the COMPLETE set of modules the prober may link: the 24 it
// linked when this guard was added. It is an allow-list, so a new module fails
// the build by name, including one that arrives transitively through a
// dependency bump.
//
// The prober is a black-box client of the mesh: an HTTP client, a DNS lookup
// and a counter it exports over OTLP. It runs on every node, and what it
// measures is only an external SLI while it shares nothing with the control
// plane it watches. Adding a module is a deliberate act: justify it in the PR.
var allowedModules = map[string]bool{
	// The prober itself: CLI and the HTTP client's x/net pieces.
	"github.com/spf13/cobra": true,
	"github.com/spf13/pflag": true,
	"golang.org/x/net":       true,
	"golang.org/x/sys":       true,
	"golang.org/x/text":      true,

	// common/log + telemetry: the OTel metric SDK and the OTLP metric exporter
	// over gRPC, which is how aether_probe_requests_total leaves the pod.
	"go.opentelemetry.io/otel":                                          true,
	"go.opentelemetry.io/otel/metric":                                   true,
	"go.opentelemetry.io/otel/trace":                                    true,
	"go.opentelemetry.io/otel/log":                                      true,
	"go.opentelemetry.io/otel/sdk":                                      true,
	"go.opentelemetry.io/otel/sdk/metric":                               true,
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc": true,
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
const maxModules = 24

// forbiddenPackages must never be reachable from the prober, under any module.
// It has no Kubernetes API access at all (its targets come from flags), serves
// no xDS, holds no SPIRE identity of its own and is not a CNI or DNS server.
// controller-runtime's signal handler alone once linked client-go into it
// (#772, phase B1).
var forbiddenPackages = []string{
	"sigs.k8s.io/controller-runtime",
	"k8s.io/client-go",
	"k8s.io/apimachinery",
	"k8s.io/api/",
	"sigs.k8s.io/gateway-api",
	"github.com/envoyproxy/go-control-plane",
	"github.com/spiffe/go-spiffe",
	"github.com/containernetworking/cni",
	"github.com/miekg/dns",
}

// maxBinaryBytes is a bloat ceiling, not a target: the prober is ~15.4MiB on
// amd64 (~14.3MiB on arm64).
const maxBinaryBytes = 20 * 1024 * 1024

// TestProberLinksOnlyAllowedModules asserts the allow-list and the module
// budget against the linked ELF that ships in the prober image, via the module
// list rules_go embeds in its build info, then scans the same file for the
// import paths of the packages it must never link.
func TestProberLinksOnlyAllowedModules(t *testing.T) {
	path := proberBinary(t)

	info, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "reading build info from %s", path)
	require.Equal(t, "aethermesh.dev/prober/cmd/prober", info.Path)
	// Control: the dependency table must be populated, or every assertion
	// below passes vacuously.
	require.NotEmpty(t, info.Deps, "no module deps in the build info: the scan cannot be trusted")

	linked := make([]string, 0, len(info.Deps))
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
		assert.True(t, allowedModules[dep.Path],
			"the prober links module %s, which is not on the allow-list. It is a black-box client "+
				"of the mesh and must not grow into the control plane it measures. Fix the import, "+
				"or justify the module and add it to allowedModules (and maxModules).", dep.Path)
	}
	sort.Strings(linked)
	t.Logf("prober links %d modules: %v", len(linked), linked)

	assert.Len(t, allowedModules, maxModules, "allowedModules and maxModules disagree; change both together")
	assert.LessOrEqual(t, len(linked), maxModules,
		"the prober links %d modules, over the budget of %d", len(linked), maxModules)

	binary, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/prober/internal/prober")),
		"%s does not contain the prober import path: the package scan cannot be trusted", path)
	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"the prober links %s. It probes the data plane from the client side with an HTTP "+
				"client and a resolver; it has no Kubernetes client, no xDS server, no CNI and no "+
				"SPIRE identity. Fix the import; do not relax this test.", pkg)
	}

	if raceInstrumented {
		t.Logf("%s is %d bytes under -race; the %d-byte ceiling applies to plain builds only", "prober", len(binary), maxBinaryBytes)
	} else {
		assert.LessOrEqual(t, len(binary), maxBinaryBytes,
			"prober is %d bytes, over the %d-byte ceiling", len(binary), maxBinaryBytes)
	}
	t.Logf("prober is %d bytes", len(binary))
}

// proberBinary locates the linked binary in the test's runfiles.
func proberBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"prober/cmd/prober/prober_/prober",
		"prober/cmd/prober/prober",
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
	t.Fatalf("prober binary not found in runfiles; looked in %v", candidates)
	return ""
}
