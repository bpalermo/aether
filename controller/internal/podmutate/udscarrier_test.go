package podmutate

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"aethermesh.dev/common/constants/annotations"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/udspath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func csiVolume(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: udspath.CSIDriver}}}
}

func emptyDirVolume(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

// udsPod is a mesh-managed pod with the given uds-socket annotation ("" = none),
// fsGroup (nil = unset) and volumes.
func udsPod(socket string, fsGroup *int64, vols ...corev1.Volume) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{aetherlabels.LabelAetherManaged: "true"}},
		Spec:       corev1.PodSpec{Volumes: vols},
	}
	if socket != "" {
		pod.Annotations = map[string]string{annotations.AnnotationEndpointUDSSocket: socket}
	}
	if fsGroup != nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: fsGroup}
	}
	return pod
}

// TestMutator_UDSCarrier pins the admission half of the proposal 039 Phase 2
// cut-over: every pod whose socket cannot be delivered on the csi.aether.io
// carrier is DENIED, with a message that names the fix, and every pod that can
// is admitted (and still mutated).
func TestMutator_UDSCarrier(t *testing.T) {
	gid := int64(65532)
	tests := []struct {
		name     string
		pod      *corev1.Pod
		wantDeny []string // substrings of the denial; nil = admitted
	}{
		{
			name: "csi carrier with fsGroup",
			pod:  udsPod("uds/app.sock", &gid, csiVolume("uds")),
		},
		{
			name: "no uds request, no csi volume",
			pod:  udsPod("", nil, emptyDirVolume("cache")),
		},
		{
			name: "csi volume without an annotation (policy-delivered)",
			pod:  udsPod("", &gid, csiVolume("uds")),
		},
		{
			// The cut-over's headline: the pre-039 shape is refused loudly.
			name:     "emptyDir carrier",
			pod:      udsPod("uds/app.sock", &gid, emptyDirVolume("uds")),
			wantDeny: []string{"not_csi", "csi: {driver: csi.aether.io}", "fsGroup", "emptyDir"},
		},
		{
			name:     "volume not declared",
			pod:      udsPod("nope/app.sock", &gid, csiVolume("uds")),
			wantDeny: []string{"volume_not_declared", `"nope"`},
		},
		{
			name:     "csi volume without fsGroup",
			pod:      udsPod("uds/app.sock", nil, csiVolume("uds")),
			wantDeny: []string{"securityContext.fsGroup"},
		},
		{
			name:     "two csi volumes",
			pod:      udsPod("a/app.sock", &gid, csiVolume("a"), csiVolume("b")),
			wantDeny: []string{"2 `csi: {driver: csi.aether.io}` volumes"},
		},
		{
			name:     "file over the 54-byte budget",
			pod:      udsPod("uds/"+strings.Repeat("f", udspath.MaxFileLen+1), &gid, csiVolume("uds")),
			wantDeny: []string{"path_too_long", "AF_UNIX"},
		},
		{
			name: "file at the 54-byte budget",
			pod:  udsPod("uds/"+strings.Repeat("f", udspath.MaxFileLen), &gid, csiVolume("uds")),
		},
		{
			name:     "file with a path separator",
			pod:      udsPod("uds/a/b.sock", &gid, csiVolume("uds")),
			wantDeny: []string{"bad_file"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMutator("2", slog.New(slog.DiscardHandler))
			resp := m.Handle(context.Background(), request(tt.pod))
			if tt.wantDeny == nil {
				require.True(t, resp.Allowed, "denied: %v", resp.Result)
				return
			}
			require.False(t, resp.Allowed, "admitted a pod whose socket cannot be delivered")
			require.NotNil(t, resp.Result)
			for _, want := range tt.wantDeny {
				assert.Contains(t, resp.Result.Message, want)
			}
		})
	}
}

// TestMutator_UDSCarrierOptOutUntouched: a pod that opts out of the mesh is not
// UDS-delivered, so its volumes are none of the webhook's business.
func TestMutator_UDSCarrierOptOutUntouched(t *testing.T) {
	gid := int64(1)
	pod := udsPod("uds/app.sock", &gid, emptyDirVolume("uds"))
	pod.Labels[aetherlabels.LabelAetherManaged] = "false"
	resp := NewMutator("2", slog.New(slog.DiscardHandler)).Handle(context.Background(), request(pod))
	assert.True(t, resp.Allowed)
}
