// Package cmd provides command-line interface and configuration for the Aether registrar.
package cmd

import (
	"time"

	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/spire"
)

const (
	defaultSyncInterval = 5 * time.Second
	defaultGRPCAddress  = ":8443"
)

// DefaultMeshConfigPath is where the chart mounts the projected MeshConfig
// ConfigMap (written by the aether-controller).
const DefaultMeshConfigPath = "/etc/aether/mesh-config.yaml"

// RegistrarConfig holds configuration for the Aether registrar.
//
// Mesh-wide policy (the embedded manager telemetry fields and SpireEnabled) is
// loaded from the mounted MeshConfig ConfigMap — projected from the MeshConfig
// CR by the aether-controller — exactly like the agent. Only per-instance/
// topology fields below are flags. See docs/proposals/015_mesh-config.md.
type RegistrarConfig struct {
	manager.Config

	// MeshConfigPath is the path to the mounted MeshConfig YAML (ConfigMap).
	MeshConfigPath string

	// ClusterName is the Kubernetes cluster name
	ClusterName string

	// RegistryBackend selects the registry backend ("kubernetes", "dynamodb", or "etcd")
	RegistryBackend string

	// EtcdEndpoints is the list of etcd endpoints when using the etcd backend
	EtcdEndpoints []string

	// SyncInterval is how often the registrar polls the external registry
	SyncInterval time.Duration

	// GRPCAddress is the address for the registrar gRPC server
	GRPCAddress string

	// SpireEnabled controls whether the registrar uses SPIRE for mTLS
	SpireEnabled bool
	// SpireWorkloadSocketPath is the path to the SPIRE Workload API UDS socket
	SpireWorkloadSocketPath string
	// SpireTrustDomain is the SPIFFE trust domain authorized for mTLS peers
	SpireTrustDomain string
}

const (
	// DefaultSpireWorkloadSocketPath is the default SPIRE CSI-mounted socket path.
	DefaultSpireWorkloadSocketPath = "/run/secrets/workload-spiffe-uds/socket"
	// DefaultSpireTrustDomain defaults to the ROOTCA sentinel, authorizing any
	// peer that chains to the SPIRE root CA (no trust-domain restriction).
	DefaultSpireTrustDomain = spire.RootCATrustDomain
)

// NewRegistrarConfig creates a RegistrarConfig with default values.
func NewRegistrarConfig() *RegistrarConfig {
	return &RegistrarConfig{
		Config: manager.Config{
			HealthProbeBindAddress: ":8082",
			MetricsBindAddress:     ":8081",
		},
		MeshConfigPath:          DefaultMeshConfigPath,
		RegistryBackend:         "kubernetes",
		EtcdEndpoints:           []string{"localhost:2379"},
		SyncInterval:            defaultSyncInterval,
		GRPCAddress:             defaultGRPCAddress,
		SpireEnabled:            true,
		SpireWorkloadSocketPath: DefaultSpireWorkloadSocketPath,
		SpireTrustDomain:        DefaultSpireTrustDomain,
	}
}
