package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aethermesh.dev/cni/conflist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWriteCNIConfigLeavesAnUnchangedChainAlone is issue #1123's conflist
// term: on an agent pod restart the conflist and the durable entry already
// hold exactly what cni-install renders, and rewriting them (write, fsync,
// rename for each) only delays the new agent its node proxy is waiting on. The
// files' identity (same inode) is the observable. A conflist a competing
// writer stripped is still re-chained.
func TestWriteCNIConfigLeavesAnUnchangedChainAlone(t *testing.T) {
	ctx := context.Background()
	rendered := []byte(`{"name":"aether","cniVersion":"0.0.1","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`)
	base := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`

	dir := t.TempDir()
	writeFile(t, dir, "10-flannel.conflist", base)
	confPath := filepath.Join(dir, "10-flannel.conflist")
	cfg := &InstallerConfig{MountedCNINetDir: dir}

	_, err := writeCNIConfig(ctx, discardLogger(), rendered, cfg)
	require.NoError(t, err)
	confBefore, err := os.Stat(confPath)
	require.NoError(t, err)
	entryBefore, err := os.Stat(conflist.EntryPath(dir))
	require.NoError(t, err)

	logger, records := captureLogger()
	_, err = writeCNIConfig(ctx, logger, rendered, cfg)
	require.NoError(t, err)
	confAfter, err := os.Stat(confPath)
	require.NoError(t, err)
	entryAfter, err := os.Stat(conflist.EntryPath(dir))
	require.NoError(t, err)
	assert.True(t, os.SameFile(confBefore, confAfter), "an already-chained conflist must not be rewritten")
	assert.True(t, os.SameFile(entryBefore, entryAfter), "an unchanged durable entry must not be rewritten")
	_, ok := findRecord(records(t), "CNI config already carries this aether entry; not rewriting it")
	assert.True(t, ok, "the skip is logged so an operator can tell it from a write")

	// A competing writer strips the chain (#645): the next install re-chains it.
	writeFile(t, dir, "10-flannel.conflist", base)
	_, err = writeCNIConfig(ctx, discardLogger(), rendered, cfg)
	require.NoError(t, err)
	merged, err := os.ReadFile(confPath)
	require.NoError(t, err)
	chain, err := conflist.Parse(merged)
	require.NoError(t, err)
	_, present, err := chain.AetherEntry()
	require.NoError(t, err)
	assert.True(t, present, "a stripped conflist must be re-chained")
}
