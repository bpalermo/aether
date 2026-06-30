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
//
// The registrar is a control-plane component: its telemetry (OTEL) and SPIRE
// posture are aether system config, inherited from the aether umbrella chart's
// globals as flags. MeshConfig is proxy-only and does not apply here. See
// docs/proposals/015_mesh-config.md.
type RegistrarConfig struct {
	manager.Config

	// ClusterName is the Kubernetes cluster name. For the etcd backend it is also
	// this registrar's authoritative cluster partition (proposal 006).
	ClusterName string

	// MeshDomain is the DNS-style domain mesh authorities live under
	// (constants.DefaultMeshDomain). Used by the cross-cluster config export
	// controller (proposal 026) to resolve backend cluster names <svc>.<meshDomain>.
	MeshDomain string

	// Region is this registrar's region — the etcd backend's authoritative
	// partition root (proposal 006). Shared by every registrar on the same
	// regional etcd; empty falls back to the etcd backend's default.
	Region string

	// RegistryBackend selects the registry backend ("kubernetes", "dynamodb", or "etcd")
	RegistryBackend string

	// EtcdEndpoints is the list of etcd endpoints when using the etcd backend
	EtcdEndpoints []string

	// SyncInterval is how often the registrar polls the external registry
	SyncInterval time.Duration

	// GenerateMeshServices makes the leader registrar project the mesh catalog into
	// selectorless k8s Services on the mesh port (transparent-capture VIP/name
	// handles, proposal 018 Phase 3a). Default off.
	GenerateMeshServices bool

	// EnableMCS turns on Kubernetes Multi-Cluster Services (MCS-API) phase 1
	// (proposals 018 + 006): the leader registrar watches ServiceExport objects
	// and records exports in the origin-partitioned registry, and materializes a
	// ServiceImport (ClusterSetIP) + a local clusterset VIP Service for every
	// service exported anywhere in the clusterset. Requires a registry backend
	// with a cross-cluster plane (etcd); no-ops for kubernetes/dynamodb. Default
	// off — adds nothing (and no ServiceExport/ServiceImport RBAC) unless enabled.
	EnableMCS bool

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
			// Leader election: the registrar runs multiple replicas (HA endpoint
			// stream), but the leader-only runnables — the mesh-Service VIP
			// generator and the MCS ServiceImport generator (both
			// NeedLeaderElection()=true) — must run on exactly ONE replica. Without
			// leader election controller-runtime ignores NeedLeaderElection and runs
			// every runnable on every replica, so two replicas' transiently-divergent
			// registry snapshots fight create-vs-prune on the selectorless mesh
			// Services every sync interval. That flap forces a full agent xDS snapshot
			// rebuild each cycle; a captured request landing in the rebuild window
			// misses the cap_http GAMMA vhost and falls through to the ORIGINAL_DST
			// passthrough (kube-proxy round-robin), dropping the HTTPRoute feature
			// (header mutation / redirect / weight). The per-replica
			// syncer/broadcaster/write-behind stay NeedLeaderElection()=false, so the
			// endpoint stream remains served by every replica.
			LeaderElection:   true,
			LeaderElectionID: "aether-registrar.registry.aether.io",
		},
		RegistryBackend:         "kubernetes",
		EtcdEndpoints:           []string{"localhost:2379"},
		SyncInterval:            defaultSyncInterval,
		GRPCAddress:             defaultGRPCAddress,
		SpireEnabled:            true,
		SpireWorkloadSocketPath: DefaultSpireWorkloadSocketPath,
		SpireTrustDomain:        DefaultSpireTrustDomain,
	}
}
