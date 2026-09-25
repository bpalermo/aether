package services

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/common/serviceref"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/registrar/internal/server"
	"aethermesh.dev/registry"
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
		serviceref.New(ns, svc).Key(): {registryv1.Service_PROTOCOL_HTTP: {{
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
	assert.Equal(t, "true", svc.Labels[aetherlabels.LabelMeshService])
	assert.Equal(t, "svc-1", svc.Annotations[aetherlabels.AnnotationMeshService])
	assert.Equal(t, "8080", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Empty(t, svc.Spec.Selector, "selectorless: no EndpointSlices, endpoints stay in the registry")
	// Four ports (proposal 037): the two mesh spellings plus the two
	// scheme-default ports. The Service is selectorless and holds no endpoints,
	// so 80/443 carry no data path — they exist so an UNCAPTURED scheme-default
	// dial hits a kube-proxy REJECT and fails fast, instead of hanging.
	byName := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		byName[p.Name] = p.Port
		assert.Equal(t, corev1.ProtocolTCP, p.Protocol, "port %q", p.Name)
	}
	assert.Equal(t, map[string]int32{
		"mesh":     18081,
		"mesh-tcp": 18082,
		"http":     80,
		"https":    443,
	}, byName)
}

// TestGenerator_ConvergesPortsOnExistingService covers the upgrade path: a mesh
// Service created before proposal 037 carries only the "mesh" port, and the
// apply path used to compare ANNOTATIONS alone and return early. It would
// therefore have kept its single port forever — the new spellings would have
// appeared only on a fresh install, which is the worst way to discover a
// missing migration.
func TestGenerator_ConvergesPortsOnExistingService(t *testing.T) {
	pre037 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc-1",
			Namespace: "aether-test",
			Labels:    map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-1",
				aetherlabels.AnnotationMeshPort:        "8080",
				aetherlabels.AnnotationMeshAppProtocol: AppProtocolHTTP,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "mesh", Port: 18081, Protocol: corev1.ProtocolTCP}},
		},
	}
	g, c := newGen(seed("svc-1", "aether-test", 8080), pre037)
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	require.Len(t, svc.Spec.Ports, 4, "the pre-037 Service must gain the new ports, not keep its single one")

	names := make([]string, 0, 4)
	for _, p := range svc.Spec.Ports {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, []string{"mesh", "mesh-tcp", "http", "https"}, names)
}

func TestGenerator_PrunesStale(t *testing.T) {
	stale := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "old", Namespace: "aether-test",
		Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
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
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol],
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
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-1",
				aetherlabels.AnnotationMeshPort:        "8080",
				aetherlabels.AnnotationMeshAppProtocol: "tcp", // stale, should be corrected to "http"
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 18081}}},
	}
	g, c := newGen(seed("svc-1", "aether-test", 8080), existing)
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol],
		"stale app-protocol annotation must be corrected on reconcile")
}

// seedProtocol seeds a snapshot with a single service under the given protocol.
func seedProtocol(svc, ns string, port uint32, protocol registryv1.Service_Protocol) *server.Snapshot {
	s := server.NewSnapshot()
	s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		serviceref.New(ns, svc).Key(): {protocol: {{
			Ip:                 "10.0.0.1",
			Port:               port,
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: ns},
		}}},
	})
	return s
}

// TestGenerator_EmitsTCPMeshService verifies a PROTOCOL_TCP service is projected
// into a selectorless mesh Service annotated as "tcp" — the signal the agent's
// capture reconciler reads to emit the per-ClusterIP TCP-proxy floor chain.
func TestGenerator_EmitsTCPMeshService(t *testing.T) {
	g, c := newGen(seedProtocol("tcp-svc", "aether-test", 9000, registryv1.Service_PROTOCOL_TCP))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "tcp-svc")
	require.NoError(t, err)
	assert.Equal(t, "true", svc.Labels[aetherlabels.LabelMeshService])
	assert.Equal(t, "tcp-svc", svc.Annotations[aetherlabels.AnnotationMeshService])
	assert.Equal(t, "9000", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Equal(t, AppProtocolTCP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol],
		"PROTOCOL_TCP services must be annotated as tcp")
	assert.Empty(t, svc.Spec.Selector, "selectorless: endpoints stay in the registry")
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
	assert.NotEqual(t, "true", got.Labels[aetherlabels.LabelMeshService])
}

// TestGenerator_EmitsUDPMeshService is the UDP arm of TestGenerator_EmitsTCPMeshService.
//
// Before #931 this could not happen at all: PROTOCOL_UDP did not exist, and the
// protocol loop enumerated only HTTP and TCP, so a UDP service produced NO mesh
// Service — no ClusterIP, no mesh-DNS record, and therefore nothing for the
// capture path to match. That failure was silent; the service simply was not
// there.
func TestGenerator_EmitsUDPMeshService(t *testing.T) {
	g, c := newGen(seedProtocol("udp-svc", "aether-test", 9001, registryv1.Service_PROTOCOL_UDP))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "udp-svc")
	require.NoError(t, err)
	assert.Equal(t, "true", svc.Labels[aetherlabels.LabelMeshService])
	assert.Equal(t, "udp-svc", svc.Annotations[aetherlabels.AnnotationMeshService])
	assert.Equal(t, "9001", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Equal(t, AppProtocolUDP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol],
		"PROTOCOL_UDP services must be annotated as udp, not coerced to http")

	// The ServicePorts stay TCP at the Kubernetes level on purpose: the UDP floor
	// reaches backends at their application port through the pod-local nftables
	// redirect, never through kube-proxy. A "mesh-udp" ServicePort would imply a
	// kube-proxy UDP path that does not exist.
	for _, p := range svc.Spec.Ports {
		assert.Equal(t, corev1.ProtocolTCP, p.Protocol,
			"port %q: the mesh protocol is an annotation axis, not a ServicePort protocol", p.Name)
	}
}

// TestProtocolAppProtocolIsTotal guards the map the generator uses to label each
// mesh Service. A missing entry does not fail — it yields "", which apply()
// coerces to AppProtocolHTTP — so a protocol added to the enum but not here
// would silently label its services HTTP and route them down the HCM path.
func TestProtocolAppProtocolIsTotal(t *testing.T) {
	for _, p := range registry.ServedProtocols {
		got, ok := protocolAppProtocol[p]
		assert.True(t, ok, "%v has no app-protocol mapping; its Services would be mislabelled http", p)
		assert.NotEmpty(t, got, "%v maps to the empty string, which apply() coerces to http", p)
	}
}
