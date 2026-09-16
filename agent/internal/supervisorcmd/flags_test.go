package supervisorcmd

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chartDaemonSet is the rendered-template source the aether-proxy pod's argv
// comes from. It is read as test data (Bazel `data`) rather than duplicated
// here, so this cannot drift from what actually ships.
const chartDaemonSet = "charts/aether/templates/agent-proxy-daemonset.yaml"

// argFlag matches a YAML list item that is a long flag, e.g. `- "--config=..."`
// or `- "--watch-config=true"`. It deliberately anchors at the start of the
// quoted value so that `--envoy-arg=--service-cluster` contributes `envoy-arg`
// and not `service-cluster` — the latter is Envoy's flag, not ours.
var argFlag = regexp.MustCompile(`(?m)^\s*-\s*"--([a-z0-9-]+)`)

// The supervisor's argv spans the install-supervisor initContainer and the proxy
// container, which is everything from `initContainers:` up to the optional authz
// sidecar. The sidecar's own flags (--server, --addr, --set) live in the same
// file and belong to OPA, not to us.
const (
	argvStart = "initContainers:"
	argvEnd   = "- name: authz"
)

// supervisorArgv slices the template down to the two blocks whose flags the
// supervisor is handed.
func supervisorArgv(t *testing.T, template string) string {
	t.Helper()
	start := strings.Index(template, argvStart)
	require.GreaterOrEqual(t, start, 0,
		"%s has no %q — the template was restructured and this scan cannot be trusted",
		chartDaemonSet, argvStart)
	end := strings.Index(template[start:], argvEnd)
	require.GreaterOrEqual(t, end, 0,
		"%s has no %q — the template was restructured and this scan would pick up "+
			"another container's flags", chartDaemonSet, argvEnd)
	return template[start : start+end]
}

// TestFlagsCoverTheChartContract pins the flag set against the chart.
//
// The aether-proxy DaemonSet passes these names literally, in two places (the
// install-supervisor initContainer's args and the proxy container's command), so
// a rename or a removal here is a chart change — and the failure mode without
// this test is a supervisor that exits on "unknown flag" at pod start, i.e. the
// whole proxy fleet, discovered only at rollout time.
//
// #772 moved this command out of the agent binary into its own one. Every flag
// had to survive that move byte-for-byte; this is what proves it did, and what
// keeps it true afterwards.
func TestFlagsCoverTheChartContract(t *testing.T) {
	raw, err := os.ReadFile(findRepoFile(t, chartDaemonSet))
	require.NoError(t, err, "reading the proxy DaemonSet template")

	seen := map[string]struct{}{}
	for _, m := range argFlag.FindAllStringSubmatch(supervisorArgv(t, string(raw)), -1) {
		seen[m[1]] = struct{}{}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	// Control test: a template that stopped matching (retitled, restructured,
	// moved) must fail loudly rather than pass this test vacuously on an empty
	// set. The chart passes well over a dozen flags.
	require.GreaterOrEqual(t, len(names), 12,
		"only %d flags found in %s — the scan cannot be trusted", len(names), chartDaemonSet)
	require.Contains(t, names, "install-readiness-path", "the initContainer's args were not scanned")

	flags := New("test").Flags()
	for _, name := range names {
		assert.NotNil(t, flags.Lookup(name),
			"the chart passes --%s to the supervisor but the command does not define it; "+
				"every pod would fail to start on 'unknown flag'", name)
	}
	t.Logf("chart passes %d supervisor flags: %v", len(names), names)
}

// TestDeprecatedFlagsStayRegistered guards the backward-compatibility surface.
//
// --readiness-check is how a chart predating #673 probes readiness. It is
// deprecated, not removed: removing it turns a chart/image skew from "a warning
// in the logs" into "every proxy pod is permanently NotReady", and
// maxUnavailable:0 then wedges the rollout with no explanation.
func TestDeprecatedFlagsStayRegistered(t *testing.T) {
	flags := New("test").Flags()

	f := flags.Lookup("readiness-check")
	require.NotNil(t, f, "--readiness-check must stay registered for pre-#673 charts")
	assert.NotEmpty(t, f.Deprecated, "--readiness-check must be marked deprecated so using it warns")
}

// findRepoFile locates a repo-relative path from the test's working directory,
// which is the package dir under both `go test` and Bazel's runfiles tree.
func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Never skip: this test exists precisely to fail when the pinned file is
	// not the one we think it is.
	t.Fatalf("%s not found above the working directory; under Bazel the go_test "+
		"needs data = [\"//charts/aether:templates/agent-proxy-daemonset.yaml\"]", rel)
	return ""
}
