// Package cmd provides the command-line interface for the aether-controller.
package cmd

import (
	"time"

	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/common/manager"
	"aethermesh.dev/common/spire"
	"aethermesh.dev/controller/internal/meshconfig"
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
	// SpireWaitWarnAfter is how long the wait for this workload's first SVID may
	// run before it is reported at WARN. Accepted here so the chart can set the
	// same value on every component; the controller's own non-fatal wait lands in
	// PR 2 of issue #740.
	SpireWaitWarnAfter time.Duration

	// WebhookConfigName is the ValidatingWebhookConfiguration whose caBundle the
	// controller patches with the SPIRE trust bundle (SPIRE mode only).
	WebhookConfigName string
	// MutatingWebhookConfigName is the MutatingWebhookConfiguration (pod ndots
	// injection) whose caBundle the controller patches with the SPIRE trust bundle
	// (SPIRE mode only). Empty disables that patch.
	MutatingWebhookConfigName string

	// MeshDomain is the DNS-style domain mesh authorities live under. The
	// pod-mutating webhook derives the dnsConfig ndots it injects into managed
	// pods from it (= the domain's label count; 2 for aether.internal), so mesh
	// FQDNs resolve absolute-first and musl clients stop tripping on the
	// cluster.local search list. Deriving replaces the old --pod-ndots flag,
	// which could drift from the domain it described.
	MeshDomain string

	// IdentityGate* configure the egress identity gate (#1053): the
	// pod-mutating webhook injects an identity-ready init container into every
	// mesh-managed pod that holds the app containers until SPIRE has issued the
	// pod's SVID. Off unless IdentityGate is set (the chart sets it by default).
	IdentityGate bool
	// IdentityGateImage is the image the init container runs /identity-ready
	// from — the agent image (it carries the binary as an extra layer).
	IdentityGateImage string
	// IdentityGateImagePullPolicy is the init container's imagePullPolicy.
	IdentityGateImagePullPolicy string
	// IdentityGateWorkloadSocket is the SPIRE Workload API socket path inside
	// the init container; the csi.spiffe.io volume is mounted at its directory.
	IdentityGateWorkloadSocket string
	// IdentityGateTimeout, when non-zero, makes the gate give up after that long
	// (the init container exits 1). Zero waits forever: fail closed.
	IdentityGateTimeout time.Duration
	// IdentityGate{CPU,Memory}{Request,Limit} are the init container's
	// resources; empty leaves that entry unset.
	IdentityGateCPURequest    string
	IdentityGateMemoryRequest string
	IdentityGateCPULimit      string
	IdentityGateMemoryLimit   string
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
		SpireWaitWarnAfter:      spire.DefaultWaitWarnAfter,
		MeshDomain:              meshconst.DefaultMeshDomain,

		IdentityGateImagePullPolicy: "IfNotPresent",
		IdentityGateWorkloadSocket:  DefaultSpireWorkloadSocketPath,
		IdentityGateCPURequest:      "5m",
		IdentityGateMemoryRequest:   "16Mi",
		IdentityGateMemoryLimit:     "64Mi",
	}
}
