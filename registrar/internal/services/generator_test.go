package services

import (
	"context"
	"log/slog"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func seed(svc, ns string, port uint32) *server.Snapshot {
	s := server.NewSnapshot()
	s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		svc: {registryv1.Service_PROTOCOL_HTTP: {{
			Ip:                 "10.0.0.1",
			Port:               port,
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: ns},
		}}},
	})
	return s
}

func newGen(snap *server.Snapshot, objs ...client.Object) (*Generator, client.Client) {
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	return &Generator{Client: c, Snapshot: snap, MeshPort: 18081, Log: slog.New(slog.DiscardHandler)}, c
}

func get(t *testing.T, c client.Client, ns, name string) (*corev1.Service, error) {
	t.Helper()
	s := &corev1.Service{}
	return s, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, s)
}

func TestGenerator_CreatesSelectorlessMeshService(t *testing.T) {
	g, c := newGen(seed("svc-1", "aether-test", 8080))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	assert.Equal(t, "true", svc.Labels[constants.LabelMeshService])
	assert.Equal(t, "svc-1", svc.Annotations[constants.AnnotationMeshService])
	assert.Equal(t, "8080", svc.Annotations[constants.AnnotationMeshPort])
	assert.Empty(t, svc.Spec.Selector, "selectorless: no EndpointSlices, endpoints stay in the registry")
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(18081), svc.Spec.Ports[0].Port)
}

func TestGenerator_PrunesStale(t *testing.T) {
	stale := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "old", Namespace: "aether-test",
		Labels: map[string]string{constants.LabelMeshService: "true"},
	}}
	g, c := newGen(seed("svc-1", "aether-test", 8080), stale)
	g.reconcile(context.Background())

	_, err := get(t, c, "aether-test", "old")
	assert.True(t, apierrors.IsNotFound(err), "managed Service for a vanished mesh service is pruned")
	_, err = get(t, c, "aether-test", "svc-1")
	require.NoError(t, err, "current mesh service still has its VIP")
}

// TestGenerator_AnnotatesHTTPAppProtocol verifies that the generator writes the
// AnnotationMeshAppProtocol annotation on created mesh Services and updates it if the
// protocol changes. HTTP is the only protocol emitted by the current registry (all
// services use PROTOCOL_HTTP), so this test verifies that annotation is always "http".
func TestGenerator_AnnotatesHTTPAppProtocol(t *testing.T) {
	g, c := newGen(seed("svc-1", "aether-test", 8080))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[constants.AnnotationMeshAppProtocol],
		"PROTOCOL_HTTP services must be annotated as http")
}

// TestGenerator_UpdatesStaleAppProtocol verifies that if an existing mesh Service
// has a stale/wrong AnnotationMeshAppProtocol, the generator corrects it on the next
// reconcile.
func TestGenerator_UpdatesStaleAppProtocol(t *testing.T) {
	// Pre-existing managed Service with the wrong app-protocol annotation.
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-1", Namespace: "aether-test",
			Labels: map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{
				constants.AnnotationMeshService:     "svc-1",
				constants.AnnotationMeshPort:        "8080",
				constants.AnnotationMeshAppProtocol: "tcp", // stale, should be corrected to "http"
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 18081}}},
	}
	g, c := newGen(seed("svc-1", "aether-test", 8080), existing)
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[constants.AnnotationMeshAppProtocol],
		"stale app-protocol annotation must be corrected on reconcile")
}

func TestGenerator_DoesNotClobberUserService(t *testing.T) {
	user := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "aether-test"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "svc-1"}},
	}
	g, c := newGen(seed("svc-1", "aether-test", 8080), user)
	g.reconcile(context.Background())

	got, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"app": "svc-1"}, got.Spec.Selector, "a user's Service of the same name is left untouched")
	assert.NotEqual(t, "true", got.Labels[constants.LabelMeshService])
}
