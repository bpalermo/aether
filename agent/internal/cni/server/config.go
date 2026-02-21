package server

import "github.com/bpalermo/aether/agent/pkg/constants"

type CNIServerConfig struct {
	SocketPath string
}

func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		SocketPath: constants.DefaultCNISocketPath,
	}
}
