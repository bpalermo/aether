// Package cmd provides command-line interface configuration for the Aether agent.
package cmd

import (
	"github.com/bpalermo/aether/agent/constants"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
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

	// SpireEnabled controls whether the SPIRE bridge is started
	SpireEnabled bool
	// SpireTrustDomain is the SPIFFE trust domain for the cluster
	SpireTrustDomain string
	// SpireAdminSocketPath is the path to the SPIRE agent admin socket
	SpireAdminSocketPath string
	// SpireWorkloadCertDir is the directory containing SPIRE SVID certificates (svid.pem, svid_key.pem, svid_bundle.pem)
	SpireWorkloadCertDir string

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
		SpireEnabled:           true,
		SpireTrustDomain:       constants.DefaultSpireTrustDomain,
		SpireAdminSocketPath:   constants.DefaultSpireAdminSocketPath,
		SpireWorkloadCertDir:   constants.DefaultSpireWorkloadCertDir,
	}
}
