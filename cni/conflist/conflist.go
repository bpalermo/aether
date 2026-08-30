// Package conflist reads and mutates the host's chained CNI configuration.
//
// Aether installs itself as a CHAINED plugin inside another CNI's conflist
// (typically flannel's 10-flannel.conflist): the aether-cni entry is appended to
// that file's plugin list. Two components need that mutation:
//
//   - the cni-install init container, which appends the entry once at agent pod
//     start (cni/internal/install), and
//   - the node agent's re-assert loop, which re-appends it whenever a competing
//     writer strips it (issue #645: kube-flannel's init container does an
//     unconditional `cp -f` of its ConfigMap template over the conflist on every
//     flannel pod recreation, silently unmeshing the node).
//
// Both share this package so the on-disk shape can never diverge between the
// one-shot installer and the re-assert loop. It is deliberately free of logging
// and I/O policy: callers own those.
package conflist

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containernetworking/cni/libcni"
)

// AetherPluginType is the "type" of aether's entry in a conflist plugin list.
// It is also the name of the plugin binary in the CNI bin dir.
const AetherPluginType = "aether-cni"

// Chain is a parsed view of a conflist's plugin list. Use Parse to build one.
type Chain struct {
	plugins []map[string]any
}

// Parse decodes a marshalled conflist and returns its plugin list. It rejects a
// regular (single-plugin, top-level "type") CNI config: aether can only chain
// into a plugin list.
func Parse(data []byte) (*Chain, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("error loading existing CNI config (JSON error): %v", err)
	}
	if _, ok := m["type"]; ok {
		return nil, fmt.Errorf("regular CNI config is not supported")
	}
	raw, err := getPlugins(m)
	if err != nil {
		return nil, fmt.Errorf("existing CNI config: %v", err)
	}
	plugins := make([]map[string]any, 0, len(raw))
	for _, rawPlugin := range raw {
		p, err := getPlugin(rawPlugin)
		if err != nil {
			return nil, fmt.Errorf("existing CNI plugin: %v", err)
		}
		plugins = append(plugins, p)
	}
	return &Chain{plugins: plugins}, nil
}

// AetherEntry returns the marshalled aether plugin entry found in the chain.
// The bool reports whether aether is chained at all.
func (c *Chain) AetherEntry() ([]byte, bool, error) {
	for _, p := range c.plugins {
		if p["type"] != AetherPluginType {
			continue
		}
		entry, err := json.Marshal(p)
		if err != nil {
			return nil, true, fmt.Errorf("marshal chained aether entry: %w", err)
		}
		return entry, true, nil
	}
	return nil, false, nil
}

// HasAether reports whether the aether entry is present in the chain.
func (c *Chain) HasAether() bool {
	for _, p := range c.plugins {
		if p["type"] == AetherPluginType {
			return true
		}
	}
	return false
}

// HasBasePlugin reports whether the chain carries a primary CNI plugin — any
// entry that is not aether's own. It is the guardrail for the re-assert loop:
// aether chains ONTO someone else's network config (flannel on Talos) and must
// never manufacture a standalone one, so a file with no base plugin is left
// untouched for the installer/boot ordering to own.
func (c *Chain) HasBasePlugin() bool {
	for _, p := range c.plugins {
		if p["type"] != AetherPluginType {
			return true
		}
	}
	return false
}

// Insert appends aetherPlugin to existing's plugin list, replacing any entry
// already there (a re-install must not duplicate it). The aether entry's own
// cniVersion is dropped: the enclosing conflist owns the version.
func Insert(aetherPlugin, existing []byte) ([]byte, error) {
	var aetherMap map[string]any
	if err := json.Unmarshal(aetherPlugin, &aetherMap); err != nil {
		return nil, fmt.Errorf("error loading Aether CNI config (JSON error): %v", err)
	}
	delete(aetherMap, "cniVersion")

	var existingMap map[string]any
	if err := json.Unmarshal(existing, &existingMap); err != nil {
		return nil, fmt.Errorf("error loading existing CNI config (JSON error): %v", err)
	}
	if _, ok := existingMap["type"]; ok {
		return nil, fmt.Errorf("regular CNI config is not supported")
	}

	plugins, err := getPlugins(existingMap)
	if err != nil {
		return nil, fmt.Errorf("existing CNI config: %v", err)
	}
	for i, rawPlugin := range plugins {
		p, err := getPlugin(rawPlugin)
		if err != nil {
			return nil, fmt.Errorf("existing CNI plugin: %v", err)
		}
		if p["type"] == AetherPluginType {
			plugins = append(plugins[:i], plugins[i+1:]...)
			break
		}
	}

	existingMap["plugins"] = append(plugins, aetherMap)

	return Marshal(existingMap)
}

// Marshal renders a CNI config map the way the installer writes it: indented,
// newline-terminated.
func Marshal(cniConfigMap map[string]any) ([]byte, error) {
	cniConfig, err := json.MarshalIndent(cniConfigMap, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(cniConfig, "\n"...), nil
}

// ConfigFilenames follows the same semantics as kubelet: it returns the
// basenames of every usable .conf/.conflist in confDir, sorted, so that the
// FIRST entry is the config kubelet actually uses.
// https://github.com/kubernetes/kubernetes/blob/954996e231074dc7429f7be1256a579bedd8344c/pkg/kubelet/dockershim/network/cni/cni.go#L144-L184
func ConfigFilenames(confDir string) ([]string, error) {
	files, err := libcni.ConfFiles(confDir, []string{".conf", ".conflist"})
	switch {
	case err != nil:
		return nil, err
	case len(files) == 0:
		return nil, fmt.Errorf("no networks found in %s", confDir)
	}

	sort.Strings(files)

	var validFiles []string
	for _, confFile := range files {
		if strings.HasSuffix(confFile, ".conflist") {
			confList, err := libcni.ConfListFromFile(confFile)
			if err != nil || len(confList.Plugins) == 0 {
				continue
			}
		}
		validFiles = append(validFiles, filepath.Base(confFile))
	}

	if len(validFiles) == 0 {
		return nil, fmt.Errorf("no valid networks found in %s", confDir)
	}

	return validFiles, nil
}

// ActivePath returns the full path of the CNI config kubelet uses in confDir:
// the lexicographically first usable one.
func ActivePath(confDir string) (string, error) {
	names, err := ConfigFilenames(confDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(confDir, names[0]), nil
}

// getPlugins returns the plugin list of an unmarshalled CNI config map.
func getPlugins(cniConfigMap map[string]any) ([]any, error) {
	plugins, ok := cniConfigMap["plugins"].([]any)
	if !ok {
		return nil, fmt.Errorf("error reading plugin list from CNI config")
	}
	return plugins, nil
}

// getPlugin asserts a raw plugin entry as an object.
func getPlugin(rawPlugin any) (map[string]any, error) {
	plugin, ok := rawPlugin.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("error reading plugin from CNI config plugin list")
	}
	return plugin, nil
}
