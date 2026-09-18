package cmd

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

// chartDaemonSet is the rendered-template source the aether-agent pod's argv
// comes from. It is read as test data (Bazel `data`) rather than duplicated
// here, so this cannot drift from what actually ships.
const chartDaemonSet = "charts/aether/templates/agent-daemonset.yaml"

// argFlag matches a YAML list item that is a long flag, e.g. `- "--node-name=..."`.
// It anchors at the start of the quoted value so a flag whose VALUE contains
// another dashed token contributes only its own name.
var argFlag = regexp.MustCompile(`(?m)^\s*-\s*"--([a-z0-9-]+)`)

// The agent's argv is the `agent` container's args block: everything from its
// name to the port list that follows it. The cni-install initContainer above it
// passes its own, unrelated flags.
const (
	argvStart = "- name: agent"
	argvEnd   = "ports:"
)

// agentArgv slices the template down to the block whose flags the node agent is
// handed.
func agentArgv(t *testing.T, template string) string {
	t.Helper()
	start := strings.Index(template, argvStart)
	require.GreaterOrEqual(t, start, 0,
		"%s has no %q — the template was restructured and this scan cannot be trusted",
		chartDaemonSet, argvStart)
	end := strings.Index(template[start:], argvEnd)
	require.GreaterOrEqual(t, end, 0,
		"%s has no %q after %q — the template was restructured and this scan would "+
			"pick up another container's flags", chartDaemonSet, argvEnd, argvStart)
	return template[start : start+end]
}

// TestChartFlagsExistOnTheAgentCommand pins the agent's flag set against the
// chart, the same way //agent/internal/supervisorcmd pins the proxy's (#772).
//
// The aether-agent DaemonSet passes these names literally, so a rename or a
// removal here is a chart change — and without this test the failure mode is a
// DaemonSet that exits on "unknown flag" at pod start, i.e. every node's data
// plane, discovered only at rollout time. Proposal 036 is exactly such a rename:
// --spire-admin-socket became --spire-broker-socket, and a chart left half-migrated
// would take the whole fleet down.
func TestChartFlagsExistOnTheAgentCommand(t *testing.T) {
	raw, err := os.ReadFile(findRepoFile(t, chartDaemonSet))
	require.NoError(t, err, "reading the agent DaemonSet template")

	seen := map[string]struct{}{}
	for _, m := range argFlag.FindAllStringSubmatch(agentArgv(t, string(raw)), -1) {
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
	require.Contains(t, names, "spire-broker-socket",
		"the chart must pass the SPIFFE Broker Endpoint socket (proposal 036)")
	require.NotContains(t, names, "spire-admin-socket",
		"the Delegated Identity admin socket was retired by proposal 036")

	flags := GetCommand().Flags()
	for _, name := range names {
		assert.NotNil(t, flags.Lookup(name),
			"the chart passes --%s to the agent but the command does not define it; "+
				"every agent pod would fail to start on 'unknown flag'", name)
	}
	t.Logf("chart passes %d agent flags: %v", len(names), names)
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
		"needs data = [\"//charts/aether:templates/agent-daemonset.yaml\"]", rel)
	return ""
}
