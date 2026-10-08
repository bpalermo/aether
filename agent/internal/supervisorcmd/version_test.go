package supervisorcmd

import (
	"bytes"
	"testing"

	"aethermesh.dev/common/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionFlag: `proxy-supervisor --version` prints this binary's build ID
// and exits before RunE, so no Envoy is forked and no file is written (#1429).
// The command is given none of the flags a real start requires.
func TestVersionFlag(t *testing.T) {
	cmd := New(buildinfo.Version())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, buildinfo.Describe("proxy-supervisor")+"\n", out.String())
	assert.Contains(t, out.String(), "proxy-supervisor build-id "+buildinfo.Version()+"\n")
}
