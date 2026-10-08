package main

import (
	"bytes"
	"testing"

	"aethermesh.dev/common/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionFlag: `mesh-dns --version` prints this binary's build ID and exits
// without binding the resolver (#1429).
func TestVersionFlag(t *testing.T) {
	cmd := rootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, buildinfo.Describe("mesh-dns")+"\n", out.String())
	assert.Contains(t, out.String(), "mesh-dns build-id "+buildinfo.Version()+"\n")
}

// TestVersionIsTheBuildID: the service.version of the daemon's telemetry is the
// binary's own build ID, not a value linked in from the commit (#1378).
func TestVersionIsTheBuildID(t *testing.T) {
	assert.Equal(t, buildinfo.Version(), Version)
	assert.NotEmpty(t, Version)
}
