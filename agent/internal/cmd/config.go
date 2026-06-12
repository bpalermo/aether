// Package cmd provides command-line interface configuration for the Aether agent.
package cmd

import (
	"github.com/bpalermo/aether/agent/constants"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/manager"
)

// AgentConfig holds configuration for the Aether agent.
type AgentConfig struct {
	manager.Config

	// ProxyServiceNodeID is the xDS node ID for identifying the Envoy proxy instance
	ProxyServiceNodeID string

	// NodeName is the Kubernetes node name where the agent runs
	NodeName string
	// ClusterName is the Kubernetes cluster name
	ClusterName string

	// MountedLocalStorageDir is the directory where pod data is stored locally
	MountedLocalStorageDir string

	// RegistrarAddress is the gRPC address of the in-cluster Registrar service
	RegistrarAddress string

	// MeshDomain is the DNS-style domain mesh authorities live under
	// (<service>.<mesh-domain>); see constants.DefaultMeshDomain.
	MeshDomain string

	// SpireEnabled controls whether the SPIRE bridge is started
	SpireEnabled bool
	// SpireTrustDomain is the SPIFFE trust domain for the cluster
	SpireTrustDomain string
	// SpireAdminSocketPath is the path to the SPIRE agent admin socket
	SpireAdminSocketPath string
	// SpireWorkloadSocketPath is the path to the SPIRE Workload API UDS socket
	SpireWorkloadSocketPath string

	// CNIServerConfig holds CNI server configuration
	CNIServerConfig *cniServer.CNIServerConfig
}

// NewAgentConfig creates a new AgentConfig with default values.
func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Config: manager.Config{
			HealthProbeBindAddress: ":8082",
			MetricsEnabled:         true,
			MetricsBindAddress:     ":8080",
		},
		ProxyServiceNodeID:     constants.DefaultProxyID,
		CNIServerConfig:        cniServer.NewCNIServerConfig(),
		MountedLocalStorageDir: constants.DefaultHostCNIRegistryDir,
		RegistrarAddress:       "aether-registrar.aether-system.svc:443",
		MeshDomain:             commonconstants.DefaultMeshDomain,
		SpireEnabled:           true,
		// Empty = follow MeshDomain (resolved in runAgent): mesh addressing
		// and SPIFFE peer authorization share one domain unless explicitly
		// split via --spire-trust-domain.
		SpireTrustDomain:        "",
		SpireAdminSocketPath:    constants.DefaultSpireAdminSocketPath,
		SpireWorkloadSocketPath: constants.DefaultSpireWorkloadSocketPath,
	}
}
