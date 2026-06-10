// Package server implements a gRPC server for the CNI plugin interface.
// It handles pod registration and deregistration, queries node metadata,
// stores pod data locally, and registers endpoints in the service registry.
package server

import "github.com/bpalermo/aether/agent/constants"

// CNIServerConfig holds configuration for the CNIServer.
type CNIServerConfig struct {
	// SocketPath is the Unix domain socket path where the CNI gRPC server listens.
	SocketPath string
	// ProxyHealthSocketPath is the Unix domain socket of the proxy's health
	// gateway listener, probed by the liveness loop for per-pod app health.
	// It must match the path the snapshot cache programs into the gateway
	// listener (the shared default); it is configurable for tests.
	ProxyHealthSocketPath string
}

// NewCNIServerConfig creates a new CNIServerConfig with default values.
// The default socket path is the standard CNI socket path.
func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		SocketPath:            constants.DefaultCNISocketPath,
		ProxyHealthSocketPath: constants.DefaultProxyHealthSocketPath,
	}
}
