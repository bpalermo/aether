package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const chartDaemonSet = "charts/aether/templates/uds-csi-daemonset.yaml"

// argFlag matches a YAML list item that is a long flag, e.g. `- "--root=..."`.
var argFlag = regexp.MustCompile(`(?m)^\s*-\s*"--([a-z0-9-]+)`)

// TestChartFlagsExist pins the flags the chart passes (args AND the liveness
// probe's command) against this binary's flag set. A rename here is otherwise
// discovered as a crash-looping DaemonSet at rollout time.
func TestChartFlagsExist(t *testing.T) {
	raw, err := os.ReadFile(findRepoFile(t, chartDaemonSet))
	require.NoError(t, err)

	matches := argFlag.FindAllStringSubmatch(string(raw), -1)
	// Control: the template passes --debug, --kubelet-root (twice), --root,
	// --size, --inodes and --probe; a scan that found fewer is not scanning it.
	require.GreaterOrEqual(t, len(matches), 7, "the flag scan of %s cannot be trusted", chartDaemonSet)

	fs := newFlagSet(&options{})
	for _, m := range matches {
		assert.NotNil(t, fs.Lookup(m[1]), "the chart passes --%s, which uds-csi does not define", m[1])
	}
}

func TestParseFlagsDerivesSocketsFromTheKubeletRoot(t *testing.T) {
	o, err := parseFlags([]string{"--kubelet-root=/var/lib/k0s/kubelet"})
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/k0s/kubelet/plugins/csi.aether.io/csi.sock", o.csiSocket)
	assert.Equal(t, "/var/lib/k0s/kubelet/plugins_registry/csi.aether.io-reg.sock", o.registrationSocket)

	o, err = parseFlags([]string{"--csi-socket=/x/csi.sock", "--registration-socket=/y/reg.sock"})
	require.NoError(t, err)
	assert.Equal(t, "/x/csi.sock", o.csiSocket)
	assert.Equal(t, "/y/reg.sock", o.registrationSocket)
	assert.Equal(t, "/run/aether/uds", o.root)
	assert.Equal(t, "1Mi", o.size)
	assert.Equal(t, int64(64), o.inodes)

	o, err = parseFlags([]string{"--inodes=16"})
	require.NoError(t, err)
	assert.Equal(t, int64(16), o.inodes)

	_, err = parseFlags([]string{"stray"})
	require.Error(t, err)
}

// findRepoFile resolves a repo-relative path in the Bazel runfiles tree, or
// relative to the package directory under `go test`.
func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	var candidates []string
	if srcdir, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); srcdir != "" {
		candidates = append(candidates, filepath.Join(srcdir, ws, rel))
	}
	candidates = append(candidates, rel, filepath.Join("..", "..", "..", rel))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Fatalf("%s not found; looked in %v", rel, candidates)
	return ""
}
