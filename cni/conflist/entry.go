package conflist

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// EntryFilename is the basename of the DURABLE copy of aether's rendered plugin
// entry, which cni-install leaves next to the conflist it chained the entry into
// (issue #680).
//
// It exists so the agent's re-assert loop can recover a known-good entry it never
// got to observe: cni-install renders the entry once per agent POD start and, up
// to #680, kept it in memory only, so a competing writer stripping the conflist
// between that write and the loop's first check left the loop with nothing to
// re-append for the life of the agent process. cni-install is the single writer
// of this file; the agent only ever reads it.
//
// The name is deliberately BOTH dot-prefixed and extension-less as far as every
// CNI config loader is concerned. Loaders select config files by
// libcni.ConfFiles, which matches filepath.Ext against a caller-supplied list —
// {".conf", ".conflist"} here and in kubelet, {".conf", ".conflist", ".json"} in
// containerd's go-cni, {".conf", ".json"} in libcni's own deprecated LoadConf.
// filepath.Ext(".aether-cni-entry") is ".aether-cni-entry", which matches none of
// them, so a lone aether plugin object can never be mistaken for a network
// config. A ".json" suffix would NOT be safe: containerd would load it as a
// single-plugin network named "aether".
const EntryFilename = ".aether-cni-entry"

// EntryPath returns the durable entry's path inside a CNI network-config
// directory.
func EntryPath(confDir string) string {
	return filepath.Join(confDir, EntryFilename)
}

// ParseEntry validates the durable entry file's contents as a single aether
// plugin entry and returns its canonical marshalling, ready to hand to Insert.
// Anything that is not a JSON object typed AetherPluginType is rejected: the
// file is a recovery input to a repair that rewrites the node's live network
// config, so it is trusted only after it has been proven to be what it claims.
func ParseEntry(data []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("error loading the durable aether CNI entry (JSON error): %v", err)
	}
	if t, _ := m["type"].(string); t != AetherPluginType {
		return nil, fmt.Errorf("durable aether CNI entry is not a %q plugin entry", AetherPluginType)
	}
	entry, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal the durable aether CNI entry: %w", err)
	}
	return entry, nil
}
