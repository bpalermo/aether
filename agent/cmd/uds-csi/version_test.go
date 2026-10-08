package main

import (
	"bytes"
	"context"
	"testing"

	"aethermesh.dev/common/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionFlag: `uds-csi --version` prints this binary's build ID and exits
// without serving (#1429). It is given a kubelet root that does not exist, so
// returning nil shows no socket was created.
func TestVersionFlag(t *testing.T) {
	for _, arg := range []string{"--version", "-version"} {
		var out bytes.Buffer
		require.NoError(t, run(context.Background(), []string{arg, "--kubelet-root", "/nonexistent/kubelet"}, &out))
		assert.Equal(t, buildinfo.Describe("uds-csi")+"\n", out.String())
		assert.Contains(t, out.String(), "uds-csi build-id "+buildinfo.Version()+"\n")
	}
}

// TestVersionIsTheBuildID: GetPluginInfo's vendor_version is the binary's own
// build ID, not a value linked in from the commit (#1378). The CSI spec
// requires the field, treats it as opaque and bounds it at 128 bytes.
func TestVersionIsTheBuildID(t *testing.T) {
	assert.Equal(t, buildinfo.Version(), Version)
	assert.NotEmpty(t, Version)
	assert.LessOrEqual(t, len(Version), 128)
}
