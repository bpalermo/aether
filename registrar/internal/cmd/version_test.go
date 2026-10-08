package cmd

import (
	"bytes"
	"testing"

	"aethermesh.dev/common/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionFlag: `registrar --version` prints this binary's build ID and
// exits without running the component (#1429). Nothing here provides the
// configuration, the cluster or the sockets a real start needs, so the command
// returning nil is itself the proof that nothing was started.
func TestVersionFlag(t *testing.T) {
	cmd := GetCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})
	t.Cleanup(func() {
		// The command is a package global: leave it as other tests expect it.
		cmd.SetOut(nil)
		cmd.SetArgs(nil)
		if f := cmd.Flags().Lookup("version"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
	})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, buildinfo.Describe("registrar")+"\n", out.String())
	assert.Contains(t, out.String(), "registrar build-id "+buildinfo.Version()+"\n")
}

// TestVersionIsTheBuildID: the value handed to telemetry as service.version is
// the binary's own build ID, not a value linked in from the commit (#1378).
func TestVersionIsTheBuildID(t *testing.T) {
	assert.Equal(t, buildinfo.Version(), Version)
	assert.NotEmpty(t, Version)
}
