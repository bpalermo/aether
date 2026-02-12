package server

import "github.com/bpalermo/aether/agent/pkg/constants"

type CNIServerConfig struct {
	ClusterName string
	SocketPath  string
}

func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		ClusterName: constants.DefaultClusterName,
		SocketPath:  constants.DefaultCNISocketPath,
	}
}
