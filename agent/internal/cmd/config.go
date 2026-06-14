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

	// EdgeTelemetry attaches the edge-telemetry dynamic module (proposal 007)
	// to each pod's outbound HCM for source->destination request metrics. Only
	// enable when the module .so is mounted on the proxy (image volume) — Envoy
	// rejects the listener if the referenced dynamic module is absent.
	EdgeTelemetry bool

	// SpireEnabled controls whether the SPIRE bridge is started
	SpireEnabled bool
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
		ProxyServiceNodeID:      constants.DefaultProxyID,
		CNIServerConfig:         cniServer.NewCNIServerConfig(),
		MountedLocalStorageDir:  constants.DefaultHostCNIRegistryDir,
		RegistrarAddress:        "aether-registrar.aether-system.svc:443",
		MeshDomain:              commonconstants.DefaultMeshDomain,
		SpireEnabled:            true,
		SpireAdminSocketPath:    constants.DefaultSpireAdminSocketPath,
		SpireWorkloadSocketPath: constants.DefaultSpireWorkloadSocketPath,
	}
}
