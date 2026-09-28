package podmutate

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/common/constants/annotations"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const testSocket = "/run/secrets/workload-spiffe-uds/socket"

func testGate() *IdentityGate {
	return &IdentityGate{
		Image:          "quay.io/aethermesh/agent@sha256:abc",
		PullPolicy:     corev1.PullIfNotPresent,
		WorkloadSocket: testSocket,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
	}
}

func gatedMutator() *Mutator {
	return NewMutator("2", slog.New(slog.DiscardHandler)).WithIdentityGate(testGate())
}

// nsRequest is request() with the admission namespace set, as the apiserver sends it.
func nsRequest(ns string, pod *corev1.Pod) admission.Request {
	raw, _ := json.Marshal(pod)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Namespace: ns, Object: runtime.RawExtension{Raw: raw}}}
}

func patchJSON(t *testing.T, resp admission.Response) string {
	t.Helper()
	b, err := json.Marshal(resp.Patches)
	require.NoError(t, err)
	return string(b)
}

// TestIdentityGate_InjectedByDefault: a fresh pod in a managed namespace gets the
// init container and its CSI volume alongside the label and DNS options.
func TestIdentityGate_InjectedByDefault(t *testing.T) {
	resp := gatedMutator().Handle(context.Background(), nsRequest("aether-test", &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app"}}},
	}))
	require.True(t, resp.Allowed)
	p := patchJSON(t, resp)
	assert.Contains(t, p, IdentityGateContainerName)
	assert.Contains(t, p, IdentityGateVolumeName)
	assert.Contains(t, p, spiffeCSIDriver)
	assert.Contains(t, p, IdentityGateCommand)
	assert.True(t, patchesLabel(resp), "the label is still added")
}

// TestIdentityGate_Shape pins the injected container: first in line, the agent
// image running /identity-ready against the mounted Workload API socket, the CSI
// volume mounted into the init container ONLY, restricted-profile security, tiny
// resources, and the termination message a stuck pod is diagnosed by.
func TestIdentityGate_Shape(t *testing.T) {
	g := testGate()
	g.Timeout = 2 * time.Minute
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "migrate"}},
		Containers:     []corev1.Container{{Name: "app"}},
	}}
	changed, reason := g.inject(pod, "aether-test")
	require.True(t, changed, reason)

	require.Len(t, pod.Spec.InitContainers, 2)
	c := pod.Spec.InitContainers[0]
	assert.Equal(t, IdentityGateContainerName, c.Name, "the gate runs FIRST, ahead of the pod's own init containers")
	assert.Equal(t, "migrate", pod.Spec.InitContainers[1].Name)
	assert.Equal(t, g.Image, c.Image)
	assert.Equal(t, corev1.PullIfNotPresent, c.ImagePullPolicy)
	assert.Equal(t, []string{"/identity-ready"}, c.Command)
	assert.Equal(t, []string{"--spire-workload-socket=" + testSocket, "--timeout=2m0s"}, c.Args)
	assert.Nil(t, c.RestartPolicy, "a plain init container, not a native sidecar: it must EXIT to release the pod")
	assert.Equal(t, corev1.TerminationMessageFallbackToLogsOnError, c.TerminationMessagePolicy)
	require.Len(t, c.VolumeMounts, 1)
	assert.Equal(t, corev1.VolumeMount{Name: IdentityGateVolumeName, MountPath: "/run/secrets/workload-spiffe-uds", ReadOnly: true}, c.VolumeMounts[0])
	assert.Empty(t, pod.Spec.Containers[0].VolumeMounts, "app containers never get the Workload API socket")

	sc := c.SecurityContext
	require.NotNil(t, sc)
	assert.True(t, *sc.RunAsNonRoot)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	assert.True(t, *sc.ReadOnlyRootFilesystem)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
	assert.Equal(t, "16Mi", c.Resources.Requests.Memory().String())

	require.Len(t, pod.Spec.Volumes, 1)
	v := pod.Spec.Volumes[0]
	assert.Equal(t, IdentityGateVolumeName, v.Name)
	require.NotNil(t, v.CSI)
	assert.Equal(t, "csi.spiffe.io", v.CSI.Driver)
	assert.True(t, *v.CSI.ReadOnly)
}

// TestIdentityGate_NoTimeoutByDefault: without a timeout no --timeout arg is
// passed, so identity-ready waits forever (fail closed).
func TestIdentityGate_NoTimeoutByDefault(t *testing.T) {
	pod := &corev1.Pod{}
	changed, _ := testGate().inject(pod, "aether-test")
	require.True(t, changed)
	assert.Equal(t, []string{"--spire-workload-socket=" + testSocket}, pod.Spec.InitContainers[0].Args)
}

// TestIdentityGate_OptOutAnnotation: aether.io/identity-gate=false skips the gate
// but not the rest of mesh injection.
func TestIdentityGate_OptOutAnnotation(t *testing.T) {
	resp := gatedMutator().Handle(context.Background(), nsRequest("aether-test", &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: map[string]string{annotations.AnnotationIdentityGate: "false"}},
	}))
	require.True(t, resp.Allowed)
	p := patchJSON(t, resp)
	assert.NotContains(t, p, IdentityGateContainerName)
	assert.NotContains(t, p, IdentityGateVolumeName)
	assert.True(t, patchesLabel(resp), "the pod is still meshed")

	// Any value other than "false" keeps the gate.
	resp = gatedMutator().Handle(context.Background(), nsRequest("aether-test", &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: map[string]string{annotations.AnnotationIdentityGate: "true"}},
	}))
	assert.Contains(t, patchJSON(t, resp), IdentityGateContainerName)
}

// TestIdentityGate_ChartDisabled: with no gate configured (the chart's
// controller.webhook.identityGate.enabled=false) nothing is injected.
func TestIdentityGate_ChartDisabled(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler)).WithIdentityGate(nil)
	resp := m.Handle(context.Background(), nsRequest("aether-test", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
	require.True(t, resp.Allowed)
	assert.NotContains(t, patchJSON(t, resp), IdentityGateContainerName)
	assert.True(t, patchesLabel(resp))
}

// TestIdentityGate_Idempotent: re-admission of an already-gated, already-managed
// pod (both pod webhooks can fire for one pod) produces no patch, and inject
// never adds a second copy.
func TestIdentityGate_Idempotent(t *testing.T) {
	m := gatedMutator()
	pod := managedPod(dnsOpts("ndots", "2", "timeout", resolverTimeout, "attempts", resolverAttempts)...)
	changed, _ := m.Gate.inject(pod, "aether-test")
	require.True(t, changed)

	resp := m.Handle(context.Background(), nsRequest("aether-test", pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "a gated, managed pod needs no second patch")

	changed, reason := m.Gate.inject(pod, "aether-test")
	assert.False(t, changed)
	assert.Equal(t, "already injected", reason)
	assert.Len(t, pod.Spec.InitContainers, 1)
	assert.Len(t, pod.Spec.Volumes, 1)
}

// TestIdentityGate_NonMeshPodsUntouched: a pod that opts out of the mesh, a
// hostNetwork pod, and a pod in a mesh-ignored namespace (SPIRE's own, whose
// gating would deadlock SPIRE's bootstrap) never get the gate.
func TestIdentityGate_NonMeshPodsUntouched(t *testing.T) {
	m := gatedMutator()

	resp := m.Handle(context.Background(), nsRequest("aether-test", &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{aetherlabels.LabelAetherManaged: "false"}},
	}))
	assert.Empty(t, resp.Patches, "an unmeshed pod gets no patch at all")

	resp = m.Handle(context.Background(), nsRequest("aether-test", &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{HostNetwork: true},
	}))
	assert.NotContains(t, patchJSON(t, resp), IdentityGateContainerName, "hostNetwork pods are never meshed")

	for _, ns := range []string{"spire-mgmt", "aether-system", "kube-system"} {
		resp = m.Handle(context.Background(), nsRequest(ns, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
		assert.NotContains(t, patchJSON(t, resp), IdentityGateContainerName, "namespace %s is mesh-ignored", ns)
	}
}

// TestIdentityGate_ExistingVolumeNameIsReused: a pod re-created from a spec that
// kept the volume but lost the container (e.g. hand-edited) gets the container
// back without a duplicate volume (duplicate names are rejected by the apiserver).
func TestIdentityGate_ExistingVolumeNameIsReused(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: IdentityGateVolumeName}}}}
	changed, _ := testGate().inject(pod, "aether-test")
	require.True(t, changed)
	assert.Len(t, pod.Spec.Volumes, 1)
	assert.Len(t, pod.Spec.InitContainers, 1)
}

func TestIdentityGate_Validate(t *testing.T) {
	require.NoError(t, testGate().Validate())
	for name, mut := range map[string]func(*IdentityGate){
		"no image":        func(g *IdentityGate) { g.Image = "" },
		"relative socket": func(g *IdentityGate) { g.WorkloadSocket = "socket" },
		"socket at root":  func(g *IdentityGate) { g.WorkloadSocket = "/socket" },
		"negative":        func(g *IdentityGate) { g.Timeout = -time.Second },
	} {
		g := testGate()
		mut(g)
		err := g.Validate()
		assert.Error(t, err, name)
		assert.True(t, err == nil || strings.HasPrefix(err.Error(), "identity gate:"), name)
	}
}
