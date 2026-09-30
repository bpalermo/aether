package udspath

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// PodVolumes is what UDS resolution needs to know about a pod's volumes. The
// agent records it on CNI ADD (CNIPod.uds_csi_volume / uds_csi_volumes /
// volumes) so storage replay resolves without the API server; the pod webhook
// computes it from the admitted spec.
type PodVolumes struct {
	// CSIVolume is the name of the pod's single inline csi.aether.io volume,
	// or "" when it declares none or more than one.
	CSIVolume string
	// CSIVolumes counts the pod's inline csi.aether.io volumes. More than one
	// is refused (ReasonMultipleCSIVolumes): one mesh socket volume per pod.
	CSIVolumes uint32
	// Names is the name of every volume the pod declares, whatever its source.
	Names []string
}

// VolumesOf summarizes a pod spec's volumes. A nil spec has none.
func VolumesOf(spec *corev1.PodSpec) PodVolumes {
	var pv PodVolumes
	if spec == nil {
		return pv
	}
	var csi string
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		pv.Names = append(pv.Names, v.Name)
		if v.CSI != nil && v.CSI.Driver == CSIDriver {
			pv.CSIVolumes++
			csi = v.Name
		}
	}
	if pv.CSIVolumes == 1 {
		pv.CSIVolume = csi
	}
	return pv
}

// ResolvePod is ResolveCSI with the pod's volumes, so every failure carries the
// reason an operator can act on:
//
//   - the pod declares more than one csi.aether.io volume: ReasonMultipleCSIVolumes;
//   - the request names a volume the pod DOES declare, but not as its
//     csi.aether.io volume (an emptyDir from before proposal 039 Phase 2):
//     ReasonNotCSI, with a message naming the fix;
//   - the request names a volume the pod does not declare at all:
//     ReasonVolumeNotDeclared.
func ResolvePod(udsRoot, podUID string, vols PodVolumes, request string) (string, error) {
	if vols.CSIVolumes > 1 {
		return "", fail(ReasonMultipleCSIVolumes,
			"the pod declares %d `csi: {driver: %s}` volumes; UDS delivery needs exactly one", vols.CSIVolumes, CSIDriver)
	}
	path, err := ResolveCSI(udsRoot, podUID, vols.CSIVolume, request)
	if ReasonOf(err) != ReasonVolumeNotDeclared {
		return path, err
	}
	volume, _, _ := Split(request)
	if slices.Contains(vols.Names, volume) {
		return "", fail(ReasonNotCSI,
			"uds socket %q names volume %q, which the pod declares but not as `csi: {driver: %s}` "+
				"(an emptyDir socket carrier is no longer supported since proposal 039 Phase 2): "+
				"change the volume's source to `csi: {driver: %s}` and set the pod's securityContext.fsGroup",
			request, volume, CSIDriver, CSIDriver)
	}
	return "", err
}
