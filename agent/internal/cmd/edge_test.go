package cmd

import (
	"testing"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/stretchr/testify/assert"
)

// TestEdgeStorageDir guards the flag-clobber fix: the edge must fall back to the
// pod-local registry dir when --mounted-registry-dir carries the root command's
// node-hostPath default (which doesn't exist in the edge pod), while preserving
// an explicit override.
func TestEdgeStorageDir(t *testing.T) {
	assert.Equal(t, constants.DefaultEdgeRegistryDir, edgeStorageDir(constants.DefaultHostCNIRegistryDir),
		"the unusable node-hostPath default resolves to the pod-local dir")
	assert.Equal(t, "/custom/dir", edgeStorageDir("/custom/dir"),
		"an explicit override is preserved")
}
