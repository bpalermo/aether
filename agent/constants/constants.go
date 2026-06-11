// Package constants defines agent-specific constants for socket paths and directory defaults.
package constants

import "github.com/bpalermo/aether/common/constants"

const (
	// DefaultProxyID is the default xDS node ID for the Envoy proxy
	DefaultProxyID = "proxy"

	// DefaultHostCNIRegistryDir is the default directory for storing CNI registry data on the host
	DefaultHostCNIRegistryDir = "/host" + constants.CNIDefaultRegistryPath

	// DefaultXdsSocketPath is the default Unix domain socket path for the xDS server
	DefaultXdsSocketPath = "/run/aether/xds.sock"
	// DefaultCNISocketPath is the default Unix domain socket path for the CNI server
	DefaultCNISocketPath = "/run/aether/cni.sock"
	// DefaultProxyHealthSocketPath is the Unix domain socket where the proxy's
	// agent-programmed health gateway listener exposes per-pod app health
	// (health_check filters over the health_<pod> clusters, served on worker
	// threads). The liveness loop probes it instead of the admin interface.
	// /run/aether is shared between the agent and proxy containers.
	DefaultProxyHealthSocketPath = "/run/aether/health.sock"

	// DefaultSpireAdminSocketPath is the default path to the SPIRE agent admin socket
	DefaultSpireAdminSocketPath = "/tmp/spire-agent/private/admin.sock"
	// DefaultSpireTrustDomain is the default SPIFFE trust domain used for SDS secret naming
	DefaultSpireTrustDomain = "ROOTCA"
	// DefaultSpireWorkloadSocketPath is the default SPIRE Workload API UDS socket (csi.spiffe.io mount)
	DefaultSpireWorkloadSocketPath = "/run/secrets/workload-spiffe-uds/socket"

)
