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

// allowedModules is the COMPLETE set of modules the CNI plugin may link: the
// 37 it linked when this guard was added. It is an allow-list, so a new module
// fails the build by name — including one that arrives transitively through a
// dependency bump.
//
// The plugin is exec'd by the container runtime for every pod ADD and DEL on
// every node, so each linked package is paid in Go package init() on every
// invocation (init() runs before main(); no argv check can skip it). Adding a
// module is a deliberate act: justify it in the PR.
//
// This pins today's set; it does not endorse it. The OTel SDK and OTLP
// exporters (cni/internal/telemetry, ~13 of these modules) are a separate,
// open decision about keeping CNI-ADD traces. Removing modules only needs the
// list trimmed to match.
var allowedModules = map[string]bool{
	// The plugin itself: CNI spec, netlink/nftables capture rules, the agent's
	// CNI gRPC API (protovalidate), and the CRI pod sandbox lookup.
	"github.com/containernetworking/cni":                         true,
	"github.com/google/nftables":                                 true,
	"github.com/mdlayher/netlink":                                true,
	"github.com/mdlayher/socket":                                 true,
	"github.com/vishvananda/netlink":                             true,
	"github.com/vishvananda/netns":                               true,
	"k8s.io/cri-api":                                             true,
	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go": true,

	// gRPC + protobuf.
	"google.golang.org/grpc":                    true,
	"google.golang.org/protobuf":                true,
	"github.com/golang/protobuf":                true,
	"google.golang.org/genproto/googleapis/api": true,
	"google.golang.org/genproto/googleapis/rpc": true,
	"golang.org/x/net":                          true,
	"golang.org/x/sync":                         true,
	"golang.org/x/sys":                          true,
	"golang.org/x/text":                         true,

	// Logging: zap + a rotating file sink (the plugin has no stdout to log to).
	"go.uber.org/zap":                  true,
	"go.uber.org/multierr":             true,
	"gopkg.in/natefinch/lumberjack.v2": true,

	// cni/internal/telemetry: OTel SDK + OTLP trace/metric exporters.
	"go.opentelemetry.io/otel":                                                    true,
	"go.opentelemetry.io/otel/metric":                                             true,
	"go.opentelemetry.io/otel/trace":                                              true,
	"go.opentelemetry.io/otel/sdk":                                                true,
	"go.opentelemetry.io/otel/sdk/metric":                                         true,
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace":                           true,
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc":             true,
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc":           true,
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc": true,
	"go.opentelemetry.io/proto/otlp":                                              true,
	"go.opentelemetry.io/auto/sdk":                                                true,
	"github.com/grpc-ecosystem/grpc-gateway/v2":                                   true,
	"github.com/cenkalti/backoff/v5":                                              true,
	"github.com/cespare/xxhash/v2":                                                true,
	"github.com/go-logr/logr":                                                     true,
	"github.com/go-logr/stdr":                                                     true,
	"github.com/google/uuid":                                                      true,
}

// maxModules is the module budget: the size of the allow-list above. It is
// asserted separately so a change to the list has to change this number too,
// which makes a growing budget visible in review.
const maxModules = 37

// forbiddenPackages must never be reachable from the plugin, under any module.
// It runs no Kubernetes client (the agent does that and answers over the CNI
// gRPC socket), serves no xDS and holds no SPIRE identity. These are the
// payloads that would make a per-pod exec cost what the 65MiB agent costs.
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
}

// maxBinaryBytes is a bloat ceiling, not a target: the plugin is ~17.9MiB.
const maxBinaryBytes = 24 * 1024 * 1024

// TestCNIPluginLinksOnlyAllowedModules asserts the allow-list and the module
// budget against the linked ELF that ships in the cni-install image, via the
// module list rules_go embeds in its build info. scripts/check-cni-deps.sh is
// the complementary build-graph half.
func TestCNIPluginLinksOnlyAllowedModules(t *testing.T) {
	path := cniBinary(t)

	info, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "reading build info from %s", path)
	require.Equal(t, "aethermesh.dev/cni/cmd/cni", info.Path)
	// Control: the dependency table must be populated, or every assertion
	// below passes vacuously.
	require.NotEmpty(t, info.Deps, "no module deps in the build info — the scan cannot be trusted")

	linked := make([]string, 0, len(info.Deps))
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
		assert.True(t, allowedModules[dep.Path],
			"the CNI plugin links module %s, which is not on the allow-list. The runtime execs this "+
				"binary for every pod ADD/DEL, so every module is paid in package init() on each one. "+
				"Fix the import, or justify the module and add it to allowedModules (and maxModules).",
			dep.Path)
	}
	sort.Strings(linked)
	t.Logf("cni links %d modules: %v", len(linked), linked)

	assert.Len(t, allowedModules, maxModules, "allowedModules and maxModules disagree; change both together")
	assert.LessOrEqual(t, len(linked), maxModules,
		"the CNI plugin links %d modules, over the budget of %d", len(linked), maxModules)

	binary, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(binary, []byte("aethermesh.dev/cni/internal/plugin")),
		"%s does not contain the plugin import path — the package scan cannot be trusted", path)
	for _, pkg := range forbiddenPackages {
		assert.False(t, bytes.Contains(binary, []byte(pkg)),
			"the CNI plugin links %s. It talks to the node agent over the CNI gRPC socket and to the "+
				"kernel; it has no Kubernetes client, no xDS server and no SPIRE identity. Fix the "+
				"import; do not relax this test.", pkg)
	}

	assert.LessOrEqual(t, len(binary), maxBinaryBytes,
		"cni is %d bytes, over the %d-byte ceiling", len(binary), maxBinaryBytes)
	t.Logf("cni is %d bytes", len(binary))
}

// cniBinary locates the linked binary in the test's runfiles.
func cniBinary(t *testing.T) string {
	t.Helper()

	// rules_go stages a go_binary under "<name>_/<name>"; keep the plain path as
	// a fallback in case that layout ever changes.
	relPaths := []string{
		"cni/cmd/cni/cni_/cni",
		"cni/cmd/cni/cni",
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
	t.Fatalf("cni binary not found in runfiles; looked in %v", candidates)
	return ""
}
