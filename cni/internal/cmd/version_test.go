package cmd

import (
	"bytes"
	"testing"

	"aethermesh.dev/common/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionFlag: `cni-install --version` prints this binary's build ID and
// exits without installing anything (#1429). It is given source and target
// directories that do not exist, so a run that reached the installer would
// return an error; returning nil shows it did not.
func TestVersionFlag(t *testing.T) {
	cmd := GetCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version", "--cni-bin-dir", "/nonexistent/src", "--cni-bin-target-dir", "/nonexistent/dst"})
	t.Cleanup(func() {
		// The command is a package global: leave it as other tests expect it.
		cmd.SetOut(nil)
		cmd.SetArgs(nil)
		for _, name := range []string{"version", "cni-bin-dir", "cni-bin-target-dir"} {
			if f := cmd.Flags().Lookup(name); f != nil {
				_ = f.Value.Set(f.DefValue)
				f.Changed = false
			}
		}
	})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, buildinfo.Describe("cni-install")+"\n", out.String())
	assert.Contains(t, out.String(), "cni-install build-id "+buildinfo.Version()+"\n")
}
