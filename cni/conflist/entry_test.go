package conflist

import (
	"path/filepath"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntryFilenameIsNotALoadableConfig pins the one property the durable entry's
// name exists for (#680): every CNI config loader selects files by
// filepath.Ext against an extension list, and this name matches none of the lists
// in use — kubelet's and ours ({.conf,.conflist}), containerd go-cni's
// ({.conf,.conflist,.json}), or libcni's deprecated LoadConf ({.conf,.json}).
// A ".json" name would be loaded by containerd as a standalone network.
func TestEntryFilenameIsNotALoadableConfig(t *testing.T) {
	assert.NotContains(t, []string{".conf", ".conflist", ".json"}, filepath.Ext(EntryFilename))
	assert.Equal(t, ".", EntryFilename[:1], "the durable entry must also be hidden from a plain directory listing")

	dir := t.TempDir()
	writeFile(t, dir, "10-flannel.conflist", `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`)
	writeFile(t, dir, EntryFilename, `{"name":"aether","type":"aether-cni"}`)

	names, err := ConfigFilenames(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"10-flannel.conflist"}, names)

	for _, exts := range [][]string{
		{".conf", ".conflist"},
		{".conf", ".conflist", ".json"},
		{".conf", ".json"},
	} {
		files, err := libcni.ConfFiles(dir, exts)
		require.NoError(t, err)
		assert.NotContains(t, files, EntryPath(dir), "extensions %v must not select the durable entry", exts)
	}
}

func TestParseEntry(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{
			name: "a rendered aether entry",
			data: `{"name":"aether","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`,
		},
		{
			name:    "not JSON",
			data:    `{not json`,
			wantErr: true,
		},
		{
			name:    "a JSON array",
			data:    `[{"type":"aether-cni"}]`,
			wantErr: true,
		},
		{
			name:    "an empty file",
			data:    ``,
			wantErr: true,
		},
		{
			name:    "someone else's plugin entry",
			data:    `{"name":"cbr0","type":"flannel"}`,
			wantErr: true,
		},
		{
			name:    "a whole conflist rather than one entry",
			data:    `{"name":"cbr0","plugins":[{"type":"aether-cni"}]}`,
			wantErr: true,
		},
		{
			name:    "a non-string type",
			data:    `{"type":42}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEntry([]byte(tc.data))
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, tc.data, string(got))
		})
	}
}

// TestParseEntryRoundTripsThroughInsert proves the durable entry is a usable
// re-assert input: priming from it and inserting reproduces the chained conflist.
func TestParseEntryRoundTripsThroughInsert(t *testing.T) {
	base := []byte(`{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`)
	rendered := []byte(`{"name":"aether","cniVersion":"0.0.1","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`)

	chained, err := Insert(rendered, base)
	require.NoError(t, err)

	chain, err := Parse(chained)
	require.NoError(t, err)
	entry, present, err := chain.AetherEntry()
	require.NoError(t, err)
	require.True(t, present)

	primed, err := ParseEntry(entry)
	require.NoError(t, err)

	again, err := Insert(primed, base)
	require.NoError(t, err)
	assert.Equal(t, string(chained), string(again))
}
