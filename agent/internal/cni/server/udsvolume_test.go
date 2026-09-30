package server

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/udspath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func csiVol(name, driver string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: driver}}}
}

func emptyDirVol(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

// TestEnhanceCNIPod_RecordsUDSCarrier pins what the agent learns about the UDS
// carrier at CNI ADD (proposal 039 Phase 2): the name of the pod's single
// csi.aether.io volume, how many it declares, and every volume name — so the
// resolver can tell not_csi from volume_not_declared from multiple_csi_volumes
// without the API server.
func TestEnhanceCNIPod_RecordsUDSCarrier(t *testing.T) {
	tests := []struct {
		name        string
		volumes     []corev1.Volume
		wantCSI     string
		wantCount   uint32
		wantVolumes []string
	}{
		{
			name:        "one csi.aether.io volume",
			volumes:     []corev1.Volume{emptyDirVol("cache"), csiVol("uds", udspath.CSIDriver)},
			wantCSI:     "uds",
			wantCount:   1,
			wantVolumes: []string{"cache", "uds"},
		},
		{
			// The pre-039 carrier: the volume is recorded, but not as the CSI one.
			name:        "emptyDir only",
			volumes:     []corev1.Volume{emptyDirVol("uds")},
			wantVolumes: []string{"uds"},
		},
		{
			name:        "another CSI driver is not the carrier",
			volumes:     []corev1.Volume{csiVol("spiffe", "csi.spiffe.io")},
			wantVolumes: []string{"spiffe"},
		},
		{
			name:        "two csi.aether.io volumes: neither is trusted",
			volumes:     []corev1.Volume{csiVol("a", udspath.CSIDriver), csiVol("b", udspath.CSIDriver)},
			wantCount:   2,
			wantVolumes: []string{"a", "b"},
		},
		{
			name: "no volumes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sPod := validK8sPod("my-pod", "default")
			k8sPod.UID = k8stypes.UID("0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1")
			k8sPod.Spec.Volumes = tt.volumes
			srv := newTestCNIServer(fake.NewClientBuilder().WithObjects(k8sPod).Build(),
				storage.NewMockStorage[*cniv1.CNIPod](), &testRegistry{},
				cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler)), "")

			cniPod := validCNIPod("my-pod", "default", "c1")
			uid, err := srv.enhanceCNIPod(context.Background(), cniPod)
			require.NoError(t, err)
			assert.Equal(t, "0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1", uid)
			assert.Equal(t, tt.wantCSI, cniPod.GetUdsCsiVolume())
			assert.Equal(t, tt.wantCount, cniPod.GetUdsCsiVolumes())
			assert.Equal(t, tt.wantVolumes, cniPod.GetVolumes())
		})
	}
}

// TestAddPod_UDSCarrierSurvivesStorageReplay: the carrier facts are persisted
// with the pod record, and a FRESH storage instance — an agent restart reading
// the node-local files, with no API server — gets them back, so the socket
// resolves on replay exactly as it did at ADD.
func TestAddPod_UDSCarrierSurvivesStorageReplay(t *testing.T) {
	dir := t.TempDir()
	newCNIPod := func() *cniv1.CNIPod { return &cniv1.CNIPod{} }

	k8sPod := validK8sPod("uds-pod", "default")
	k8sPod.UID = k8stypes.UID("0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1")
	k8sPod.Annotations[annotations.AnnotationEndpointPort] = "8080"
	k8sPod.Annotations[annotations.AnnotationEndpointUDSSocket] = "uds/app.sock"
	k8sPod.Spec.Volumes = []corev1.Volume{csiVol("uds", udspath.CSIDriver), emptyDirVol("scratch")}

	srv := newTestCNIServer(fake.NewClientBuilder().WithObjects(k8sPod).Build(),
		storage.NewCachedLocalStorage[*cniv1.CNIPod](dir, newCNIPod), &testRegistry{},
		cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler)), "")
	_, err := srv.AddPod(context.Background(), &cniv1.AddPodRequest{Pod: validCNIPod("uds-pod", "default", "c-uds")})
	require.NoError(t, err)

	fresh := storage.NewCachedLocalStorage[*cniv1.CNIPod](dir, newCNIPod)
	require.NoError(t, fresh.Initialize(context.Background()), "load the node-local files, as an agent start does")
	replayed, err := fresh.GetResource(context.Background(), types.ContainerID("c-uds"))
	require.NoError(t, err)
	assert.Equal(t, "uds", replayed.GetUdsCsiVolume())
	assert.Equal(t, uint32(1), replayed.GetUdsCsiVolumes())
	assert.Equal(t, []string{"uds", "scratch"}, replayed.GetVolumes())

	path, err := udspath.ResolvePod(udspath.DefaultCSIRoot, replayed.GetUid(), udspath.PodVolumes{
		CSIVolume:  replayed.GetUdsCsiVolume(),
		CSIVolumes: replayed.GetUdsCsiVolumes(),
		Names:      replayed.GetVolumes(),
	}, replayed.GetAnnotations()[annotations.AnnotationEndpointUDSSocket])
	require.NoError(t, err)
	assert.Equal(t, "/run/aether/uds/0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1/app.sock", path)
}
