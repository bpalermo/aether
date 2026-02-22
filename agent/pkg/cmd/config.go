package cmd

import (
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
)

type AgentConfig struct {
	// Debug
	Debug bool
	// XDS nodeID
	ProxyServiceNodeID string

	NodeName    string
	ClusterName string

	MountedLocalStorageDir string

	CNIServerConfig *cniServer.CNIServerConfig
}

func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Debug:                  false,
		ProxyServiceNodeID:     constants.DefaultProxyID,
		CNIServerConfig:        cniServer.NewCNIServerConfig(),
		MountedLocalStorageDir: constants.DefaultHostCNIRegistryDir,
	}
}
