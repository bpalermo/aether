package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aetherPlugin is the marshalled aether plugin entry as createCNIConfigFile
// produces it: a single-plugin object carrying its own cniVersion (which
// insertCNIConfig strips before appending it to the host conflist's plugin list).
const aetherPlugin = `{"name":"aether","type":"aether-cni","cniVersion":"0.0.1","agentCNIPath":"/run/aether/cni.sock"}`

// pluginTypes returns the ordered "type" of each plugin in a marshalled conflist.
func pluginTypes(t *testing.T, conflist []byte) []string {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(conflist, &m))
	raw, ok := m["plugins"].([]any)
	require.True(t, ok, "result must have a plugins list")
	types := make([]string, 0, len(raw))
	for _, p := range raw {
		types = append(types, p.(map[string]any)["type"].(string))
	}
	return types
}

func TestInsertCNIConfig(t *testing.T) {
	t.Run("appends aether to an existing plugins list", func(t *testing.T) {
		existing := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"},{"type":"portmap"}]}`
		out, err := insertCNIConfig([]byte(aetherPlugin), []byte(existing))
		require.NoError(t, err)
		assert.Equal(t, []string{"flannel", "portmap", "aether-cni"}, pluginTypes(t, out))

		// The appended aether entry must NOT carry cniVersion (the conflist owns it).
		var m map[string]any
		require.NoError(t, json.Unmarshal(out, &m))
		aether := m["plugins"].([]any)[2].(map[string]any)
		_, hasVersion := aether["cniVersion"]
		assert.False(t, hasVersion, "cniVersion must be stripped from the chained plugin entry")
		// The host conflist's own top-level cniVersion is preserved.
		assert.Equal(t, "0.3.1", m["cniVersion"])
	})

	t.Run("re-install overwrites the existing aether entry (no duplicate)", func(t *testing.T) {
		existing := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"},{"type":"aether-cni","cniVersion":"stale"},{"type":"portmap"}]}`
		out, err := insertCNIConfig([]byte(aetherPlugin), []byte(existing))
		require.NoError(t, err)
		// Exactly one aether-cni entry, moved to the end, with the stale copy gone.
		assert.Equal(t, []string{"flannel", "portmap", "aether-cni"}, pluginTypes(t, out))

		var m map[string]any
		require.NoError(t, json.Unmarshal(out, &m))
		aether := m["plugins"].([]any)[2].(map[string]any)
		assert.Equal(t, "/run/aether/cni.sock", aether["agentCNIPath"], "the re-inserted entry is the fresh config, not the stale one")
	})

	t.Run("rejects a regular (non-list) CNI config", func(t *testing.T) {
		existing := `{"name":"mynet","type":"bridge","cniVersion":"0.3.1"}`
		_, err := insertCNIConfig([]byte(aetherPlugin), []byte(existing))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular CNI config is not supported")
	})

	t.Run("errors on a conflist missing the plugins key", func(t *testing.T) {
		existing := `{"name":"cbr0","cniVersion":"0.3.1"}`
		_, err := insertCNIConfig([]byte(aetherPlugin), []byte(existing))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "existing CNI config")
	})

	t.Run("errors on malformed existing JSON", func(t *testing.T) {
		_, err := insertCNIConfig([]byte(aetherPlugin), []byte(`{not json`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "existing CNI config (JSON error)")
	})

	t.Run("errors on malformed aether JSON", func(t *testing.T) {
		_, err := insertCNIConfig([]byte(`{not json`), []byte(`{"plugins":[]}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Aether CNI config (JSON error)")
	})

	t.Run("inserts into an empty plugins list", func(t *testing.T) {
		out, err := insertCNIConfig([]byte(aetherPlugin), []byte(`{"name":"cbr0","plugins":[]}`))
		require.NoError(t, err)
		assert.Equal(t, []string{"aether-cni"}, pluginTypes(t, out))
	})
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func TestGetConfigFilenames(t *testing.T) {
	validList := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`

	t.Run("returns valid conflist basenames", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		writeFile(t, dir, "README.md", "ignored")
		got, err := getConfigFilenames(dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"10-flannel.conflist"}, got)
	})

	t.Run("includes .conf files without parsing them", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-mynet.conf", `{"name":"mynet","type":"bridge"}`)
		got, err := getConfigFilenames(dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"10-mynet.conf"}, got)
	})

	t.Run("skips a conflist with no plugins", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "empty.conflist", `{"name":"empty","plugins":[]}`)
		_, err := getConfigFilenames(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no valid networks")
	})

	t.Run("errors when no conf files exist", func(t *testing.T) {
		_, err := getConfigFilenames(t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no networks found")
	})
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
