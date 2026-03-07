// Package server implements a gRPC server for the CNI plugin interface.
// It handles pod registration and deregistration, queries node metadata,
// stores pod data locally, and registers endpoints in the service registry.
package server

import "github.com/bpalermo/aether/agent/pkg/constants"

// CNIServerConfig holds configuration for the CNIServer.
type CNIServerConfig struct {
	// SocketPath is the Unix domain socket path where the CNI gRPC server listens.
	SocketPath string
}

// NewCNIServerConfig creates a new CNIServerConfig with default values.
// The default socket path is the standard CNI socket path.
func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		SocketPath: constants.DefaultCNISocketPath,
	}
}
