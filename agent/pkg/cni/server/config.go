package server

import "github.com/bpalermo/aether/agent/pkg/constants"

type CNIServerConfig struct {
	ClusterName      string
	ProxyID          string
	SocketPath       string
	LocalStoragePath string
}

func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		ClusterName:      constants.DefaultClusterName,
		SocketPath:       constants.DefaultCNISocketPath,
		LocalStoragePath: constants.DefaultHostCNIRegistryDir,
	}
}
