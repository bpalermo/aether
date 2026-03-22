// Package cmd provides command-line interface configuration for the Aether agent.
package cmd

import (
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/constants"
)

// AgentConfig holds configuration for the Aether agent.
type AgentConfig struct {
	// Debug enables debug logging
	Debug bool
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

	// MetricsEnabled enables the controller-runtime Prometheus metrics server
	MetricsEnabled bool
	// MetricsBindAddress is the address for the metrics HTTP server
	MetricsBindAddress string
	// OTelEnabled enables the OTel MeterProvider with Prometheus exporter bridge
	OTelEnabled bool
	// OTLPEndpoint is the OTLP gRPC collector endpoint (e.g. "localhost:4317"); empty disables OTLP export
	OTLPEndpoint string

	// CNIServerConfig holds CNI server configuration
	CNIServerConfig *cniServer.CNIServerConfig
}

// NewAgentConfig creates a new AgentConfig with default values.
func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Debug:                  false,
		MetricsEnabled:         true,
		MetricsBindAddress:     ":8080",
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
