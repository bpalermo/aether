package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bpalermo/aether/agent/storage"
	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStorage_KubernetesUIDRoundTrip verifies the persisted UID survives the
// storage write/read cycle the agent relies on across restarts.
func TestStorage_KubernetesUIDRoundTrip(t *testing.T) {
	s := storage.NewCachedLocalStorage[*cniv1.CNIPod](t.TempDir(), func() *cniv1.CNIPod { return &cniv1.CNIPod{} })
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	pod := &cniv1.CNIPod{
		Name:             "web-1",
		Namespace:        "default",
		NetworkNamespace: "/var/run/netns/x",
		ContainerId:      "cid-1",
		KubernetesUid:    "8f7c2e1a-1234-4d4e-9f00-abcdefabcdef",
	}
	if err := s.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod); err != nil {
		t.Fatalf("AddResource() error = %v", err)
	}

	got, err := s.GetResource(context.Background(), types.ContainerID(pod.GetContainerId()))
	if err != nil {
		t.Fatalf("GetResource() error = %v", err)
	}
	if got.GetKubernetesUid() != pod.GetKubernetesUid() {
		t.Errorf("KubernetesUid = %q, want %q", got.GetKubernetesUid(), pod.GetKubernetesUid())
	}
}

// TestStorage_PreUIDFileLoadsWithEmptyUID verifies backward compatibility: a
// pod file written before kubernetes_uid existed loads with an empty UID, the
// signal for the resubscribe path to fall back to an API-server fetch.
func TestStorage_PreUIDFileLoadsWithEmptyUID(t *testing.T) {
	dir := t.TempDir()
	// JSON written by a pre-UID agent: no kubernetesUid key.
	legacy := `{"name":"web-1","namespace":"default","networkNamespace":"/var/run/netns/x","containerId":"cid-legacy"}`
	if err := os.WriteFile(filepath.Join(dir, "cid-legacy.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("writing legacy file: %v", err)
	}

	s := storage.NewCachedLocalStorage[*cniv1.CNIPod](dir, func() *cniv1.CNIPod { return &cniv1.CNIPod{} })
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	got, err := s.GetResource(context.Background(), types.ContainerID("cid-legacy"))
	if err != nil {
		t.Fatalf("GetResource() error = %v", err)
	}
	if got.GetKubernetesUid() != "" {
		t.Errorf("KubernetesUid = %q, want empty for pre-UID file", got.GetKubernetesUid())
	}
	if got.GetName() != "web-1" {
		t.Errorf("Name = %q, want web-1", got.GetName())
	}
}

func TestCountManagedPods(t *testing.T) {
	mkPod := func(ns string, managed bool, ip string) corev1.Pod {
		labels := map[string]string{}
		if managed {
			labels[constants.LabelAetherManaged] = "true"
		}
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Labels: labels},
			Status:     corev1.PodStatus{PodIP: ip},
		}
	}

	pods := []corev1.Pod{
		mkPod("default", true, "10.0.0.1"),     // counted
		mkPod("default", true, "10.0.0.2"),     // counted
		mkPod("default", true, ""),             // no IP yet: not counted
		mkPod("default", false, "10.0.0.3"),    // unmanaged: not counted
		mkPod("kube-system", true, "10.0.0.4"), // ignored namespace: not counted
	}

	if got := countManagedPods(pods); got != 2 {
		t.Errorf("countManagedPods() = %d, want 2", got)
	}
}
