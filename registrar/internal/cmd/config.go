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

// RegistrarConfig holds configuration for the Aether registrar.
type RegistrarConfig struct {
	manager.Config

	// ClusterName is the Kubernetes cluster name
	ClusterName string

	// RegistryBackend selects the registry backend ("kubernetes", "dynamodb", "etcd", or "cloudmap")
	RegistryBackend string

	// EtcdEndpoints is the list of etcd endpoints when using the etcd backend
	EtcdEndpoints []string

	// CloudMapNamespace is the AWS Cloud Map HTTP namespace for service discovery
	CloudMapNamespace string

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
		RegistryBackend:         "kubernetes",
		EtcdEndpoints:           []string{"localhost:2379"},
		CloudMapNamespace:       "aether",
		SyncInterval:            defaultSyncInterval,
		GRPCAddress:             defaultGRPCAddress,
		SpireEnabled:            true,
		SpireWorkloadSocketPath: DefaultSpireWorkloadSocketPath,
		SpireTrustDomain:        DefaultSpireTrustDomain,
	}
}
