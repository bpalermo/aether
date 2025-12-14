package util

import (
	"encoding/json"
	"fmt"
	"os"
)

// ReadCNIConfigMap reads CNI config from a file and returns the unmarshalled JSON as a map
func ReadCNIConfigMap(path string) (map[string]any, error) {
	cniConfig, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cniConfigMap map[string]any
	if err = json.Unmarshal(cniConfig, &cniConfigMap); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return cniConfigMap, nil
}

// GetPlugins given an unmarshalled CNI config JSON map, return the plugin list asserted as a []interface{}
func GetPlugins(cniConfigMap map[string]any) (plugins []any, err error) {
	plugins, ok := cniConfigMap["plugins"].([]any)
	if !ok {
		err = fmt.Errorf("error reading plugin list from CNI config")
		return plugins, err
	}
	return plugins, err
}

// GetPlugin given the raw plugin interface, return the plugin asserted as a map[string]interface{}
func GetPlugin(rawPlugin any) (plugin map[string]any, err error) {
	plugin, ok := rawPlugin.(map[string]any)
	if !ok {
		err = fmt.Errorf("error reading plugin from CNI config plugin list")
		return plugin, err
	}
	return plugin, err
}

// MarshalCNIConfig marshal the CNI config map and append a new line
func MarshalCNIConfig(cniConfigMap map[string]any) ([]byte, error) {
	cniConfig, err := json.MarshalIndent(cniConfigMap, "", "  ")
	if err != nil {
		return nil, err
	}
	cniConfig = append(cniConfig, "\n"...)
	return cniConfig, nil
}
