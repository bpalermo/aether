package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The conflist mutation itself (insert/parse/filename resolution) is shared with
// the agent's re-assert loop and tested in aethermesh.dev/cni/conflist. What is
// installer-specific — waiting for the primary CNI config to appear — is here.

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func TestGetCNIConfigFilepath(t *testing.T) {
	ctx := context.Background()
	validList := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`

	t.Run("returns the named file when it exists", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		got, err := getCNIConfigFilepath(ctx, "10-flannel.conflist", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), got)
	})

	t.Run("falls back from a missing .conf to its .conflist sibling", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		got, err := getCNIConfigFilepath(ctx, "10-flannel.conf", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), got)
	})

	t.Run("falls back from a missing .conflist to its .conf sibling", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-mynet.conf", `{"name":"mynet","type":"bridge"}`)
		got, err := getCNIConfigFilepath(ctx, "10-mynet.conflist", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-mynet.conf"), got)
	})

	t.Run("auto-discovers the config when no name is given", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		got, err := getCNIConfigFilepath(ctx, "", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), got)
	})
}
