// Package cmd provides the command-line interface for the aether-controller.
package cmd

import (
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/controller/internal/meshconfig"
)

// ControllerConfig holds configuration for the aether-controller, which runs the
// MeshConfig validating webhook and the reconciler that projects the MeshConfig
// CR into the ConfigMap the agent and registrar mount.
//
// Mesh-wide policy itself lives in the MeshConfig CR; this config is only the
// controller's own operational settings.
type ControllerConfig struct {
	manager.Config

	// MeshConfigMapName is the name of the ConfigMap the reconciler projects each
	// namespace's MeshConfig into. The namespace is the MeshConfig CR's own namespace
	// (co-located), so there is no namespace setting.
	MeshConfigMapName string

	// SpireEnabled serves the validating webhook with a SPIRE-issued X.509 SVID
	// (via the Workload API) instead of the Helm self-signed cert, and injects the
	// SPIRE trust bundle into the webhook's caBundle. The SPIRE registration entry
	// for the controller must carry the webhook Service DNS name as a DNS SAN.
	SpireEnabled bool
	// SpireWorkloadSocketPath is the SPIRE Workload API UDS socket path.
	SpireWorkloadSocketPath string

	// WebhookConfigName is the ValidatingWebhookConfiguration whose caBundle the
	// controller patches with the SPIRE trust bundle (SPIRE mode only).
	WebhookConfigName string
}

// DefaultSpireWorkloadSocketPath is the default SPIRE CSI-mounted socket path.
const DefaultSpireWorkloadSocketPath = "/run/secrets/workload-spiffe-uds/socket"

// NewControllerConfig creates a ControllerConfig with default values.
func NewControllerConfig() *ControllerConfig {
	return &ControllerConfig{
		Config: manager.Config{
			HealthProbeBindAddress: ":8082",
			MetricsEnabled:         true,
			MetricsBindAddress:     ":8080",
			LeaderElection:         true,
			LeaderElectionID:       "aether-controller.config.aether.io",
		},
		MeshConfigMapName:       meshconfig.DefaultMeshConfigMapName,
		SpireWorkloadSocketPath: DefaultSpireWorkloadSocketPath,
	}
}
