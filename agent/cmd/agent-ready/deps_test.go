package main

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxBinaryBytes is a bloat ceiling, not a target: the stdlib-only binary is
// ~2MB against the agent's ~67MB. 8MB catches an accidental heavyweight import
// without tripping on toolchain drift.
const maxBinaryBytes = 8 * 1024 * 1024

// TestAgentReadyLinksOnlyTheStdlib is the linkage guard (proposal 041, open
// question 2). The kubelet execs this binary every 2s for readiness and every
// 10s for liveness on every node; each exec pays the init() of everything it
// links, which runs before main() and cannot be skipped. So it links nothing
// but the standard library, and that is asserted against the ELF that ships:
// the build info embedded in a Go binary lists every module it links, and the
// list must be empty.
func TestAgentReadyLinksOnlyTheStdlib(t *testing.T) {
	path := agentReadyBinary(t)

	info, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "reading build info from %s", path)
	// Control: this is the binary we think it is, with readable build info, so
	// an empty dependency list below is a real finding and not a broken scan.
	require.Equal(t, "aethermesh.dev/agent/cmd/agent-ready", info.Path)
	var linked []string
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
	}
	assert.Empty(t, linked,
		"agent-ready must stay stdlib-only: it is exec'd every probe period on every node. Fix the import; do not relax this test.")

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.LessOrEqual(t, st.Size(), int64(maxBinaryBytes),
		"agent-ready is %d bytes, over the %d-byte ceiling", st.Size(), maxBinaryBytes)
	t.Logf("agent-ready is %d bytes", st.Size())
}

// agentReadyBinary locates the linked binary in the test's runfiles.
func agentReadyBinary(t *testing.T) string {
	t.Helper()
	relPaths := []string{
		"agent/cmd/agent-ready/agent-ready_/agent-ready",
		"agent/cmd/agent-ready/agent-ready",
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
	t.Fatalf("agent-ready binary not found in runfiles; looked in %v", candidates)
	return ""
}
