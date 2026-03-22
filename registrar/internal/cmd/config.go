// Package cmd provides command-line interface and configuration for the Aether registrar.
package cmd

import "time"

const (
	defaultSyncInterval = 5 * time.Second
	defaultGRPCAddress  = ":8443"
)

// RegistrarConfig holds configuration for the Aether registrar.
type RegistrarConfig struct {
	// Debug enables debug logging
	Debug bool

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
}

const (
	// DefaultSpireWorkloadSocketPath is the default SPIRE CSI-mounted socket path.
	DefaultSpireWorkloadSocketPath = "/run/secrets/workload-spiffe-uds/socket"
)

// NewRegistrarConfig creates a RegistrarConfig with default values.
func NewRegistrarConfig() *RegistrarConfig {
	return &RegistrarConfig{
		RegistryBackend:         "kubernetes",
		EtcdEndpoints:           []string{"localhost:2379"},
		CloudMapNamespace:       "aether",
		SyncInterval:            defaultSyncInterval,
		GRPCAddress:             defaultGRPCAddress,
		SpireEnabled:            true,
		SpireWorkloadSocketPath: DefaultSpireWorkloadSocketPath,
	}
}
