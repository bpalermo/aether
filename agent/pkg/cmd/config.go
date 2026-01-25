package cmd

import (
	cniServer "github.com/bpalermo/aether/agent/pkg/cni/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/install"
)

type AgentConfig struct {
	// Debug
	Debug bool
	// ClusterName the cluster where the agent is running in
	ClusterName string
	// XDS nodeID
	ProxyServiceNodeID string

	InstallConfig   *install.InstallerConfig
	CNIServerConfig *cniServer.CNIServerConfig
}

func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Debug:              false,
		ClusterName:        constants.DefaultClusterName,
		ProxyServiceNodeID: constants.DefaultProxyID,
		InstallConfig:      install.NewInstallerConfig(),
		CNIServerConfig:    cniServer.NewCNIServerConfig(),
	}
}
