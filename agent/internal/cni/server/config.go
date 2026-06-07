// Package server implements a gRPC server for the CNI plugin interface.
// It handles pod registration and deregistration, queries node metadata,
// stores pod data locally, and registers endpoints in the service registry.
package server

import "github.com/bpalermo/aether/agent/constants"

// CNIServerConfig holds configuration for the CNIServer.
type CNIServerConfig struct {
	// SocketPath is the Unix domain socket path where the CNI gRPC server listens.
	SocketPath string
	// EnvoyAdminAddress is the host:port of the Envoy admin interface used to
	// verify that listener configuration has been applied.
	EnvoyAdminAddress string
	// HostMountPrefix is the in-container path under which the host root
	// filesystem is mounted, used to read a pod's netns file (e.g. the host
	// "/var/run/netns/..." path is read at "<prefix>/var/run/netns/..."). Empty
	// means the host paths are used as-is.
	HostMountPrefix string
}

// DefaultHostMountPrefix is the conventional mount point for the host root
// filesystem subtrees the agent consumes (e.g. /host/var/run/netns).
const DefaultHostMountPrefix = "/host"

// NewCNIServerConfig creates a new CNIServerConfig with default values.
// The default socket path is the standard CNI socket path.
func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		SocketPath:        constants.DefaultCNISocketPath,
		EnvoyAdminAddress: "127.0.0.1:9901",
		HostMountPrefix:   DefaultHostMountPrefix,
	}
}
