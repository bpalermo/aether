package install

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"aethermesh.dev/agent/constants"
	"aethermesh.dev/cni/config"
	"aethermesh.dev/cni/conflist"
	"aethermesh.dev/cni/internal/util"
	"aethermesh.dev/common/file"
)

/*
	{
	  "name": "cbr0",
	  "cniVersion": "0.3.1",
	  "plugins": [
	    {
	      "type": "flannel",
	      "delegate": {
	        "hairpinMode": true,
	        "isDefaultGateway": true
	      }
	    },
	    {
	      "type": "portmap",
	      "capabilities": {
	        "portMappings": true
	      }
	    }
	  ]
	}
*/

// podAnnotationsCapability is the CNI capability key that makes containerd pass
// the pod's annotations to the plugin via runtimeConfig. The plugin reads the
// redirect-all opt-in annotation from there (proposal 022, M2a).
const podAnnotationsCapability = "io.kubernetes.cri.pod-annotations"

// confMode is the mode both the conflist and the durable aether entry beside it
// are written with. The agent's re-assert loop writes the conflist back with the
// same mode (agent/internal/cniconflist).
const confMode = os.FileMode(0o644)

func createCNIConfigFile(ctx context.Context, logger *slog.Logger, cfg *InstallerConfig) (string, error) {
	pluginConfig := config.AetherConf{}

	pluginConfig.Name = "aether"
	pluginConfig.Type = conflist.AetherPluginType
	pluginConfig.CNIVersion = "0.0.1"
	pluginConfig.AgentCNIPath = constants.DefaultCNISocketPath
	pluginConfig.OTLPEndpoint = cfg.OTLPEndpoint
	pluginConfig.CaptureRedirectAllDefault = cfg.CaptureRedirectAllDefault
	pluginConfig.MeshDNSEnabled = cfg.MeshDNSEnabled
	pluginConfig.HostIP = cfg.HostIP
	// Declare the pod-annotations capability so containerd populates
	// runtimeConfig["io.kubernetes.cri.pod-annotations"] on each ADD. The plugin
	// reads the capture.aether.io/redirect-all annotation from it to scope the
	// redirect-all spike (proposal 022, M2a) to opt-in pods.
	pluginConfig.Capabilities = map[string]bool{podAnnotationsCapability: true}

	marshalledJSON, err := json.MarshalIndent(pluginConfig, "", "  ")
	if err != nil {
		return "", err
	}
	marshalledJSON = append(marshalledJSON, "\n"...)

	return writeCNIConfig(ctx, logger, marshalledJSON, cfg)
}

// writeCNIConfig will
// 1. read in the existing CNI config file from the primary config
// 2. append the `aether`-specific entry
// 3. write the combined result back out to the same path, overwriting the original
func writeCNIConfig(ctx context.Context, logger *slog.Logger, pluginConfig []byte, cfg *InstallerConfig) (string, error) {
	// get the CNI config file path for the primary CNI config
	cniConfigFilepath, err := getCNIConfigFilepath(ctx, logger, cfg.CNIConfName, cfg.MountedCNINetDir)
	if err != nil {
		return "", err
	}

	if !file.Exists(cniConfigFilepath) {
		return "", fmt.Errorf("CNI config file %s removed during configuration", cniConfigFilepath)
	}
	// This section overwrites an existing plugins list entry for aether-cni
	existingCNIConfig, err := os.ReadFile(cniConfigFilepath)
	if err != nil {
		return "", err
	}
	merged, err := conflist.Insert(pluginConfig, existingCNIConfig)
	if err != nil {
		return "", err
	}

	if err = file.AtomicWrite(cniConfigFilepath, merged, confMode); err != nil {
		logger.ErrorContext(ctx, "failed to write CNI config file", "error", err, "filepath", cniConfigFilepath)
		return cniConfigFilepath, err
	}

	logger.InfoContext(ctx, "Wrote CNI config", "filepath", cniConfigFilepath)

	writeDurableEntry(ctx, logger, filepath.Dir(cniConfigFilepath), merged)

	return cniConfigFilepath, nil
}

// writeDurableEntry persists the rendered aether plugin entry, on its own, next
// to the conflist it was just chained into, so the agent's re-assert loop can
// prime a known-good entry it never got to observe (issue #680): a competing
// writer stripping the conflist between this install and the loop's first check
// used to leave that loop with nothing to re-append for the life of the agent
// PROCESS, unrepairable until the pod itself was recreated.
//
// cni-install is the file's only writer, and it writes it unconditionally — the
// agent's --cni-conflist-reassert kill switch governs the loop, not the
// artifact. A failure here is logged and swallowed: the entry is chained on disk
// either way, and losing a safety net must never fail the install that just
// meshed the node.
//
// The entry is read back out of the MERGED conflist rather than from the
// pre-merge render, so the durable copy is byte-identical to what is actually
// chained (Insert drops the entry's own cniVersion: the enclosing conflist owns
// it) and a re-assert from the file reproduces this install exactly.
func writeDurableEntry(ctx context.Context, logger *slog.Logger, confDir string, merged []byte) {
	entry, err := extractAetherEntry(merged)
	if err != nil {
		logger.ErrorContext(ctx, "failed to extract the aether entry from the CNI config just written; the re-assert loop cannot prime from disk", "error", err)
		return
	}

	path := conflist.EntryPath(confDir)
	if err := file.AtomicWrite(path, entry, confMode); err != nil {
		logger.ErrorContext(ctx, "failed to write the durable aether CNI entry; the re-assert loop cannot prime from disk", "error", err, "filepath", path)
		return
	}
	logger.InfoContext(ctx, "Wrote the durable aether CNI entry", "filepath", path)
}

// extractAetherEntry returns the aether plugin entry as it sits in a merged
// conflist.
func extractAetherEntry(merged []byte) ([]byte, error) {
	chain, err := conflist.Parse(merged)
	if err != nil {
		return nil, err
	}
	entry, present, err := chain.AetherEntry()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("no %q entry in the CNI config just written", conflist.AetherPluginType)
	}
	return entry, nil
}

// getCNIConfigFilepath waits indefinitely for a main CNI config file to exist before returning
// Or until canceled by parent context
func getCNIConfigFilepath(ctx context.Context, logger *slog.Logger, cniConfName, mountedCNINetDir string) (string, error) {
	watcher, err := util.CreateFileWatcher(logger, mountedCNINetDir)
	if err != nil {
		return "", err
	}
	defer watcher.Close()

	cniConfName, err = resolveConfName(ctx, logger, cniConfName, mountedCNINetDir, watcher)
	if err != nil {
		return "", err
	}

	cniConfigFilepath := filepath.Join(mountedCNINetDir, cniConfName)

	cniConfigFilepath, err = waitForConfigFile(ctx, logger, cniConfigFilepath, watcher)
	if err != nil {
		return "", err
	}

	logger.InfoContext(ctx, "CNI config file exists, proceeding", "filepath", cniConfigFilepath)
	return cniConfigFilepath, nil
}

// resolveConfName waits until a CNI config filename is known, either from the provided
// name or by discovering the first valid config file in mountedCNINetDir.
func resolveConfName(ctx context.Context, logger *slog.Logger, cniConfName, mountedCNINetDir string, watcher *util.Watcher) (string, error) {
	for len(cniConfName) == 0 {
		cniConfNames, err := conflist.ConfigFilenames(mountedCNINetDir)
		if err == nil || len(cniConfNames) > 0 {
			return cniConfNames[0], nil
		}
		logger.ErrorContext(ctx, "aether CNI is configured as chained plugin, but cannot find existing CNI network config", "error", err)
		logger.InfoContext(ctx, "waiting for CNI network config file to be written in dir", "dir", mountedCNINetDir)
		if err := watcher.Wait(ctx); err != nil {
			return "", err
		}
	}
	return cniConfName, nil
}

// waitForConfigFile waits until the given CNI config filepath exists, following
// .conf/.conflist alternates as needed.
func waitForConfigFile(ctx context.Context, logger *slog.Logger, cniConfigFilepath string, watcher *util.Watcher) (string, error) {
	for !file.Exists(cniConfigFilepath) {
		if strings.HasSuffix(cniConfigFilepath, ".conf") && file.Exists(cniConfigFilepath+"list") {
			logger.InfoContext(ctx, "file doesn't exist, but %[1]slist does; Using it as the CNI config file instead.", "filepath", cniConfigFilepath)
			cniConfigFilepath += "list"
		} else if strings.HasSuffix(cniConfigFilepath, ".conflist") && file.Exists(cniConfigFilepath[:len(cniConfigFilepath)-4]) {
			logger.InfoContext(ctx, "file doesn't exist, but %s does; Using it as the CNI config file instead.", "filepath", cniConfigFilepath, "alternate", cniConfigFilepath[:len(cniConfigFilepath)-4])
			cniConfigFilepath = cniConfigFilepath[:len(cniConfigFilepath)-4]
		} else {
			logger.InfoContext(ctx, "CNI config file %s does not exist. Waiting for file to be written...", "filepath", cniConfigFilepath)
			if err := watcher.Wait(ctx); err != nil {
				return "", err
			}
		}
	}
	return cniConfigFilepath, nil
}
