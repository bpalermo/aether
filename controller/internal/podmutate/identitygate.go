package podmutate

import (
	"fmt"
	"path/filepath"
	"time"

	"aethermesh.dev/common/constants"
	"aethermesh.dev/common/constants/annotations"
	corev1 "k8s.io/api/core/v1"
)

const (
	// IdentityGateContainerName is the init container the webhook injects. It is
	// also the idempotency key: a pod that already carries an init container of
	// this name is left alone.
	IdentityGateContainerName = "aether-identity-ready"
	// IdentityGateVolumeName is the csi.spiffe.io volume that exposes the SPIRE
	// agent's Workload API socket — to the init container ONLY.
	IdentityGateVolumeName = "aether-identity-gate-spiffe"
	// IdentityGateCommand is where the binary sits in the agent image
	// (//agent/cmd/agent:agent_image, extra layer).
	IdentityGateCommand = "/identity-ready"
	// spiffeCSIDriver is the SPIFFE CSI driver every aether component already
	// mounts its Workload API socket from.
	spiffeCSIDriver = "csi.spiffe.io"
	// nonrootUID is distroless's nonroot user, the agent image's own USER.
	nonrootUID int64 = 65532
)

// IdentityGate configures the egress identity gate (#1053): an init container
// that holds a mesh-managed pod's app containers until SPIRE has issued the
// pod's X.509 SVID, so the app cannot send before the node proxy has a client
// certificate for it. It is the egress twin of the inbound-readiness promotion
// gate, which holds an endpoint UNHEALTHY until an mTLS handshake with the
// pod's own inbound listener succeeds.
type IdentityGate struct {
	// Image runs /identity-ready: the agent image, already on every node.
	Image string
	// PullPolicy of the init container.
	PullPolicy corev1.PullPolicy
	// WorkloadSocket is the SPIRE Workload API socket path INSIDE the init
	// container; the CSI volume is mounted at its directory.
	WorkloadSocket string
	// Timeout, when non-zero, makes identity-ready give up (exit 1) after that
	// long without an SVID. Zero waits forever: the pod stays in Init (fail
	// closed).
	Timeout time.Duration
	// Resources of the init container.
	Resources corev1.ResourceRequirements
}

// Validate reports a configuration the webhook cannot inject from.
func (g *IdentityGate) Validate() error {
	if g.Image == "" {
		return fmt.Errorf("identity gate: an image is required")
	}
	if g.WorkloadSocket == "" || !filepath.IsAbs(g.WorkloadSocket) || filepath.Dir(g.WorkloadSocket) == "/" {
		return fmt.Errorf("identity gate: workload socket %q must be an absolute path below a directory", g.WorkloadSocket)
	}
	if g.Timeout < 0 {
		return fmt.Errorf("identity gate: timeout must be >= 0, got %s", g.Timeout)
	}
	return nil
}

// gateSkipReason says why a managed pod does NOT get the gate, or "" when it
// does. namespace is the admission request's namespace (a pod's own
// metadata.namespace may be empty at CREATE).
func (g *IdentityGate) gateSkipReason(pod *corev1.Pod, namespace string) string {
	switch {
	case g == nil:
		return "identity gate disabled"
	case pod.Annotations[annotations.AnnotationIdentityGate] == "false":
		return "pod opted out (" + annotations.AnnotationIdentityGate + "=false)"
	case pod.Spec.HostNetwork:
		// No CNI ADD, no capture, no per-pod SVID subscription: not meshed.
		return "hostNetwork pod"
	case constants.IsIgnoredNamespace(namespace):
		// The mesh never intercepts these (SPIRE and aether itself live here);
		// gating them on SPIRE would deadlock SPIRE's own bootstrap.
		return "mesh-ignored namespace"
	case hasInitContainer(pod, IdentityGateContainerName):
		return "already injected"
	}
	return ""
}

// inject adds the gate to pod and reports whether it changed it. The init
// container goes FIRST, so it also holds every other init container (they
// can send through the mesh too) and native sidecars.
func (g *IdentityGate) inject(pod *corev1.Pod, namespace string) (bool, string) {
	if reason := g.gateSkipReason(pod, namespace); reason != "" {
		return false, reason
	}

	if !hasVolume(pod, IdentityGateVolumeName) {
		readOnly := true
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: IdentityGateVolumeName,
			VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
				Driver:   spiffeCSIDriver,
				ReadOnly: &readOnly,
			}},
		})
	}

	args := []string{"--spire-workload-socket=" + g.WorkloadSocket}
	if g.Timeout > 0 {
		args = append(args, "--timeout="+g.Timeout.String())
	}
	pod.Spec.InitContainers = append([]corev1.Container{g.container(args)}, pod.Spec.InitContainers...)
	return true, ""
}

// container is the injected init container: tiny, and compliant with the Pod
// Security "restricted" profile so it never makes a pod inadmissible.
func (g *IdentityGate) container(args []string) corev1.Container {
	no, yes := false, true
	uid := nonrootUID
	return corev1.Container{
		Name:            IdentityGateContainerName,
		Image:           g.Image,
		ImagePullPolicy: g.PullPolicy,
		Command:         []string{IdentityGateCommand},
		Args:            args,
		Resources:       *g.Resources.DeepCopy(),
		VolumeMounts: []corev1.VolumeMount{{
			Name:      IdentityGateVolumeName,
			MountPath: filepath.Dir(g.WorkloadSocket),
			ReadOnly:  true,
		}},
		// The last log line (what it is waiting for, or the timeout) becomes
		// the container's termination message, so `kubectl describe pod` shows
		// why a pod is held in Init without a log lookup.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             &yes,
			RunAsUser:                &uid,
			RunAsGroup:               &uid,
			AllowPrivilegeEscalation: &no,
			ReadOnlyRootFilesystem:   &yes,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

func hasInitContainer(pod *corev1.Pod, name string) bool {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return true
		}
	}
	return false
}

func hasVolume(pod *corev1.Pod, name string) bool {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return true
		}
	}
	return false
}
