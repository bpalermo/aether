package cmd

import (
	"bytes"
	"testing"

	"aethermesh.dev/common/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionFlag: `prober --version` prints this binary's build ID and exits
// without starting a probe loop (#1429).
func TestVersionFlag(t *testing.T) {
	cmd := GetCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, buildinfo.Describe("prober")+"\n", out.String())
	assert.Contains(t, out.String(), "prober build-id "+buildinfo.Version()+"\n")
}

// TestVersionIsTheBuildID: the value handed to telemetry as service.version is
// the binary's own build ID, not a value linked in from the commit (#1378).
func TestVersionIsTheBuildID(t *testing.T) {
	assert.Equal(t, buildinfo.Version(), Version)
	assert.NotEmpty(t, Version)
}
