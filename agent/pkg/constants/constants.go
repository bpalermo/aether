// Package constants defines agent-specific constants for socket paths and directory defaults.
package constants

import "github.com/bpalermo/aether/constants"

const (
	// DefaultProxyID is the default xDS node ID for the Envoy proxy
	DefaultProxyID = "proxy"

	// DefaultHostCNIRegistryDir is the default directory for storing CNI registry data on the host
	DefaultHostCNIRegistryDir = "/host" + constants.CNIDefaultRegistryPath

	// DefaultXdsSocketPath is the default Unix domain socket path for the xDS server
	DefaultXdsSocketPath = "/run/aether/xds.sock"
	// DefaultCNISocketPath is the default Unix domain socket path for the CNI server
	DefaultCNISocketPath = "/run/aether/cni.sock"

	// DefaultSpireAdminSocketPath is the default path to the SPIRE agent admin socket
	DefaultSpireAdminSocketPath = "/tmp/spire-agent/private/admin.sock"
	// DefaultSpireTrustDomain is the default SPIFFE trust domain used for SDS secret naming
	DefaultSpireTrustDomain = "ROOTCA"
)
