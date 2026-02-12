package cmd

import (
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/install"
)

type AgentConfig struct {
	// Debug
	Debug bool
	// XDS nodeID
	ProxyServiceNodeID string
	ProxyRegion        string
	ProxyZone          string

	MountedLocalStorageDir string

	InstallConfig   *install.InstallerConfig
	CNIServerConfig *cniServer.CNIServerConfig
}

func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Debug:                  false,
		ProxyServiceNodeID:     constants.DefaultProxyID,
		InstallConfig:          install.NewInstallerConfig(),
		CNIServerConfig:        cniServer.NewCNIServerConfig(),
		MountedLocalStorageDir: constants.DefaultHostCNIRegistryDir,
	}
}
