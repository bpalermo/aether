package podmutate

import (
	"fmt"
	"strings"

	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/udspath"
	corev1 "k8s.io/api/core/v1"
)

// udsCarrierDenial returns why a mesh-managed pod's UDS delivery setup cannot
// work on the csi.aether.io carrier (proposal 039 Phase 2), or "" when it can.
// The pod is then DENIED at admission, where the author is looking, instead of
// running Ready-but-undelivered with a resolve-failure count on one node:
//
//   - more than one csi.aether.io volume (one mesh socket volume per pod);
//   - a csi.aether.io volume on a pod with no securityContext.fsGroup — the node
//     plugin would refuse NodePublishVolume (FailedMount) anyway, this just says
//     so before the pod is scheduled;
//   - an endpoint.aether.io/uds-socket annotation naming a volume the pod does
//     not mount as csi.aether.io — in particular the pre-039 emptyDir carrier —
//     or a file name over the AF_UNIX budget.
//
// A pod delivered by an EndpointPolicy carries no annotation, so the webhook
// cannot see its request; the agent reports those (not_csi /
// volume_not_declared) at resolution.
func udsCarrierDenial(pod *corev1.Pod) string {
	vols := udspath.VolumesOf(&pod.Spec)
	if vols.CSIVolumes > 1 {
		return fmt.Sprintf("the pod declares %d `csi: {driver: %s}` volumes; a pod carries at most one mesh socket volume",
			vols.CSIVolumes, udspath.CSIDriver)
	}
	if vols.CSIVolumes == 1 && (pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil) {
		return fmt.Sprintf("volume %q is a %s volume, which requires the pod to set securityContext.fsGroup "+
			"(the socket directory is root:<fsGroup> mode 2770)", vols.CSIVolume, udspath.CSIDriver)
	}
	request := pod.Annotations[annotations.AnnotationEndpointUDSSocket]
	if request == "" {
		return ""
	}
	// Admission sees no UID yet: resolve against a worst-case (36-byte) one
	// under the default root, exactly the budget udspath.MaxFileLen states.
	if _, err := udspath.ResolvePod(udspath.DefaultCSIRoot, strings.Repeat("0", udspath.PodUIDLen), vols, request); err != nil {
		return fmt.Sprintf("%s=%q cannot be delivered (%s): %v",
			annotations.AnnotationEndpointUDSSocket, request, udspath.ReasonOf(err), err)
	}
	return ""
}
