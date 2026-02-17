package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/file"
	"github.com/bpalermo/aether/agent/pkg/util"
	"github.com/bpalermo/aether/cni/pkg/config"
	"github.com/containernetworking/cni/libcni"
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
func createCNIConfigFile(ctx context.Context, cfg *InstallerConfig) (string, error) {
	pluginConfig := config.AetherConf{}

	pluginConfig.Name = "aether"
	pluginConfig.Type = "aether-cni"
	pluginConfig.CNIVersion = "0.0.1"
	pluginConfig.AgentCNIPath = constants.DefaultCNISocketPath

	marshalledJSON, err := json.MarshalIndent(pluginConfig, "", "  ")
	if err != nil {
		return "", err
	}
	marshalledJSON = append(marshalledJSON, "\n"...)

	return writeCNIConfig(ctx, marshalledJSON, cfg)
}

// writeCNIConfig will
// 1. read in the existing CNI config file from the primary config
// 2. append the `aether`-specific entry
// 3. write the combined result back out to the same path, overwriting the original
func writeCNIConfig(ctx context.Context, pluginConfig []byte, cfg *InstallerConfig) (string, error) {
	// get the CNI config file path for the primary CNI config
	cniConfigFilepath, err := getCNIConfigFilepath(ctx, cfg.CNIConfName, cfg.MountedCNINetDir)
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
	pluginConfig, err = insertCNIConfig(pluginConfig, existingCNIConfig)
	if err != nil {
		return "", err
	}

	if err = file.AtomicWrite(cniConfigFilepath, pluginConfig, os.FileMode(0o644)); err != nil {
		installLog.Error(err, "failed to write CNI config file %v: %v", "filepath", cniConfigFilepath)
		return cniConfigFilepath, err
	}

	installLog.Info("Wrote CNI config", "filepath", cniConfigFilepath)
	return cniConfigFilepath, nil
}

// getCNIConfigFilepath waits indefinitely for a main CNI config file to exist before returning
// Or until canceled by parent context
func getCNIConfigFilepath(ctx context.Context, cniConfName, mountedCNINetDir string) (string, error) {
	watcher, err := util.CreateFileWatcher(mountedCNINetDir)
	if err != nil {
		return "", err
	}
	defer watcher.Close()

	for len(cniConfName) == 0 {
		cniConfNames, err := getConfigFilenames(mountedCNINetDir)
		if err == nil || len(cniConfNames) > 0 {
			cniConfName = cniConfNames[0]
			break
		}
		installLog.Error(err, "aether CNI is configured as chained plugin, but cannot find existing CNI network config")
		installLog.Info("waiting for CNI network config file to be written in dir", "dir", mountedCNINetDir)
		if err := watcher.Wait(ctx); err != nil {
			return "", err
		}
	}

	cniConfigFilepath := filepath.Join(mountedCNINetDir, cniConfName)

	for !file.Exists(cniConfigFilepath) {
		if strings.HasSuffix(cniConfigFilepath, ".conf") && file.Exists(cniConfigFilepath+"list") {
			installLog.Info("file doesn't exist, but %[1]slist does; Using it as the CNI config file instead.", "filepath", cniConfigFilepath)
			cniConfigFilepath += "list"
		} else if strings.HasSuffix(cniConfigFilepath, ".conflist") && file.Exists(cniConfigFilepath[:len(cniConfigFilepath)-4]) {
			installLog.Info("file doesn't exist, but %s does; Using it as the CNI config file instead.", "filepath", cniConfigFilepath, cniConfigFilepath[:len(cniConfigFilepath)-4])
			cniConfigFilepath = cniConfigFilepath[:len(cniConfigFilepath)-4]
		} else {
			installLog.Info("CNI config file %s does not exist. Waiting for file to be written...", cniConfigFilepath)
			if err := watcher.Wait(ctx); err != nil {
				return "", err
			}
		}
	}

	installLog.Info("CNI config file exists, proceeding", "filepath", cniConfigFilepath)

	return cniConfigFilepath, err
}

// getConfigFilenames follows similar semantics as kubelet
// Will return all CNI config filenames in the given directory with .conf or .conflist extensions
// https://github.com/kubernetes/kubernetes/blob/954996e231074dc7429f7be1256a579bedd8344c/pkg/kubelet/dockershim/network/cni/cni.go#L144-L184
func getConfigFilenames(confDir string) ([]string, error) {
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
		var confList *libcni.NetworkConfigList
		if strings.HasSuffix(confFile, ".conflist") {
			confList, err = libcni.ConfListFromFile(confFile)
			if err != nil {
				installLog.Error(err, "error loading CNI config list file", "file", confFile)
				continue
			}
		}

		if confList != nil && len(confList.Plugins) == 0 {
			installLog.Info("CNI config list has no networks, skipping", "name", confList.Name)
			continue
		}
		validFiles = append(validFiles, filepath.Base(confFile))
	}

	if len(validFiles) == 0 {
		return nil, fmt.Errorf("no valid networks found in %s", confDir)
	}

	return validFiles, nil
}

// insertCNIConfig will append newCNIConfig to existingCNIConfig
func insertCNIConfig(newCNIConfig, existingCNIConfig []byte) ([]byte, error) {
	var aetherMap map[string]any
	err := json.Unmarshal(newCNIConfig, &aetherMap)
	if err != nil {
		return nil, fmt.Errorf("error loading Aether CNI config (JSON error): %v", err)
	}

	var existingMap map[string]any
	err = json.Unmarshal(existingCNIConfig, &existingMap)
	if err != nil {
		return nil, fmt.Errorf("error loading existing CNI config (JSON error): %v", err)
	}

	delete(aetherMap, "cniVersion")

	var newMap map[string]any

	if _, ok := existingMap["type"]; ok {
		err := fmt.Errorf("regular CNI config is not supported")
		installLog.Error(err, "existing CNI config is not a network list file")
		return nil, err
	}
	// Assume it is a network list file
	newMap = existingMap
	plugins, err := util.GetPlugins(newMap)
	if err != nil {
		return nil, fmt.Errorf("existing CNI config: %v", err)
	}

	for i, rawPlugin := range plugins {
		p, err := util.GetPlugin(rawPlugin)
		if err != nil {
			return nil, fmt.Errorf("existing CNI plugin: %v", err)
		}
		if p["type"] == "aether-cni" {
			plugins = append(plugins[:i], plugins[i+1:]...)
			break
		}
	}

	newMap["plugins"] = append(plugins, aetherMap)

	return util.MarshalCNIConfig(newMap)
}
