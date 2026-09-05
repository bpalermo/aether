package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aethermesh.dev/cni/conflist"
	"github.com/containernetworking/cni/libcni"
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

// TestWriteCNIConfigPersistsTheDurableEntry covers issue #680: cni-install leaves
// a durable copy of the entry it just chained, so the agent's re-assert loop can
// prime from disk when a competing writer beats it to the first check.
func TestWriteCNIConfigPersistsTheDurableEntry(t *testing.T) {
	ctx := context.Background()
	rendered := []byte(`{"name":"aether","cniVersion":"0.0.1","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`)

	newDir := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`)
		return dir
	}

	t.Run("writes the entry beside the conflist", func(t *testing.T) {
		dir := newDir(t)
		_, err := writeCNIConfig(ctx, rendered, &InstallerConfig{MountedCNINetDir: dir})
		require.NoError(t, err)

		durable, err := os.ReadFile(conflist.EntryPath(dir))
		require.NoError(t, err)

		// The durable copy is exactly the entry that ended up chained, so a
		// re-assert from it reproduces this install byte for byte.
		merged, err := os.ReadFile(filepath.Join(dir, "10-flannel.conflist"))
		require.NoError(t, err)
		chain, err := conflist.Parse(merged)
		require.NoError(t, err)
		chained, present, err := chain.AetherEntry()
		require.NoError(t, err)
		require.True(t, present)
		assert.JSONEq(t, string(chained), string(durable))

		// And it is a valid priming input for the re-assert loop.
		parsed, err := conflist.ParseEntry(durable)
		require.NoError(t, err)
		assert.JSONEq(t, string(chained), string(parsed))
	})

	t.Run("is written with the conflist's mode", func(t *testing.T) {
		dir := newDir(t)
		_, err := writeCNIConfig(ctx, rendered, &InstallerConfig{MountedCNINetDir: dir})
		require.NoError(t, err)

		info, err := os.Stat(conflist.EntryPath(dir))
		require.NoError(t, err)
		assert.Equal(t, confMode, info.Mode().Perm())
	})

	// The whole point of the filename: no CNI config loader may ever pick the
	// durable entry up as a network config of its own. libcni selects by
	// filepath.Ext against a caller-supplied extension list, and the widest list
	// in use anywhere is containerd go-cni's {.conf,.conflist,.json} — a superset
	// of kubelet's and of libcni's deprecated LoadConf. Ext(".aether-cni-entry")
	// matches none of them.
	t.Run("is invisible to every CNI config loader", func(t *testing.T) {
		dir := newDir(t)
		_, err := writeCNIConfig(ctx, rendered, &InstallerConfig{MountedCNINetDir: dir})
		require.NoError(t, err)

		names, err := conflist.ConfigFilenames(dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"10-flannel.conflist"}, names)

		for _, exts := range [][]string{
			{".conf", ".conflist"},          // aether, kubelet
			{".conf", ".conflist", ".json"}, // containerd go-cni
			{".conf", ".json"},              // libcni LoadConf
		} {
			files, err := libcni.ConfFiles(dir, exts)
			require.NoError(t, err)
			assert.NotContains(t, files, conflist.EntryPath(dir), "loader extensions %v must not select the durable entry", exts)
		}

		active, err := conflist.ActivePath(dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), active)
	})
}
