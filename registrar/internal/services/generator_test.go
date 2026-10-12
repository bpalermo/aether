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
			PortProtocols:      map[uint32]registryv1.PortProtocol{port: registryv1.PortProtocol_PORT_PROTOCOL_HTTP},
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
	assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: "svc-1"}, svc.Annotations)
	assert.Empty(t, svc.Spec.Selector, "selectorless: no EndpointSlices, endpoints stay in the registry")
	// The two mesh ports (the HTTP proxy port and the L4 port), and the
	// service's real port (proposal 043): the seeded row declares 8080 in the
	// HTTP listing. No scheme-default placeholder: the pods declare neither 80
	// nor 443.
	byName := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		byName[p.Name] = p.Port
		assert.Equal(t, corev1.ProtocolTCP, p.Protocol, "port %q", p.Name)
	}
	assert.Equal(t, map[string]int32{
		"mesh":      18081,
		"mesh-tcp":  18082,
		"p8080-tcp": 8080,
	}, byName)
}

// TestGenerator_ConvergesPortsOnExistingService covers the upgrade path: a mesh
// Service created before proposal 037 carries only the "mesh" port, and the
// apply path used to compare ANNOTATIONS alone and return early. It would
// therefore have kept its single port forever — the new ports would have
// appeared only on a fresh install, which is the worst way to discover a
// missing migration. The same reconcile removes the two annotations that
// registrar stamped.
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
	require.Len(t, svc.Spec.Ports, 3, "the pre-037 Service must gain the new ports, not keep its single one")

	names := make([]string, 0, 3)
	for _, p := range svc.Spec.Ports {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, []string{"mesh", "mesh-tcp", "p8080-tcp"}, names)
	assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: "svc-1"}, svc.Annotations)
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

// realPortOf returns the one real port of a generated Service, failing the
// test when it has none or several.
func realPortOf(t *testing.T, svc *corev1.Service) corev1.ServicePort {
	t.Helper()
	var real []corev1.ServicePort
	for _, p := range svc.Spec.Ports {
		if p.Name != "mesh" && p.Name != "mesh-tcp" {
			real = append(real, p)
		}
	}
	require.Len(t, real, 1, "real ports of %s", svc.Name)
	return real[0]
}

// TestGenerator_HTTPServiceSaysSoOnItsPort: what a service serves is on its
// port, as the ServicePort's appProtocol, and nowhere else on the Service.
func TestGenerator_HTTPServiceSaysSoOnItsPort(t *testing.T) {
	g, c := newGen(seed("svc-1", "aether-test", 8080))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	p := realPortOf(t, svc)
	assert.Equal(t, "p8080-tcp", p.Name)
	require.NotNil(t, p.AppProtocol)
	assert.Equal(t, AppProtocolHTTP, *p.AppProtocol)
	assert.NotContains(t, svc.Annotations, aetherlabels.AnnotationMeshAppProtocol)
}

// TestGenerator_RemovesAStaleAppProtocolAnnotation: an existing mesh Service
// that carries aether.io/app-protocol (here a wrong one) loses it on the next
// reconcile. It used to be corrected; nothing reads it, so it is removed.
func TestGenerator_RemovesAStaleAppProtocolAnnotation(t *testing.T) {
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-1", Namespace: "aether-test",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-1",
				aetherlabels.AnnotationMeshPort:        "8080",
				aetherlabels.AnnotationMeshAppProtocol: "tcp",
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 18081}}},
	}
	g, c := newGen(seed("svc-1", "aether-test", 8080), existing)
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc-1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: "svc-1"}, svc.Annotations)
	assert.Equal(t, AppProtocolHTTP, *realPortOf(t, svc).AppProtocol, "the port says what the service serves")
}

// seedProtocol seeds a snapshot with a single service under the given protocol.
func seedProtocol(svc, ns string, port uint32, protocol registryv1.Service_Protocol) *server.Snapshot {
	s := server.NewSnapshot()
	s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		serviceref.New(ns, svc).Key(): {protocol: {{
			Ip:                 "10.0.0.1",
			Port:               port,
			PortProtocols:      map[uint32]registryv1.PortProtocol{port: registryv1.PortProtocol(protocol)},
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: ns},
		}}},
	})
	return s
}

// TestGenerator_EmitsTCPMeshService verifies a PROTOCOL_TCP service is projected
// into a selectorless mesh Service whose port says "tcp".
func TestGenerator_EmitsTCPMeshService(t *testing.T) {
	g, c := newGen(seedProtocol("tcp-svc", "aether-test", 9000, registryv1.Service_PROTOCOL_TCP))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "tcp-svc")
	require.NoError(t, err)
	assert.Equal(t, "true", svc.Labels[aetherlabels.LabelMeshService])
	assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: "tcp-svc"}, svc.Annotations)
	p := realPortOf(t, svc)
	assert.Equal(t, "p9000-tcp", p.Name)
	require.NotNil(t, p.AppProtocol)
	assert.Equal(t, AppProtocolTCP, *p.AppProtocol, "a PROTOCOL_TCP service's port says tcp")
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
	assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: "udp-svc"}, svc.Annotations)

	// The two mesh ServicePorts stay TCP at the Kubernetes level: they name a
	// way in, and there is no "mesh-udp" one. The service's REAL port is a UDP
	// ServicePort since proposal 043: the Service says what the service serves.
	udp := 0
	for _, p := range svc.Spec.Ports {
		if p.Name == "p9001-udp" {
			udp++
			assert.Equal(t, corev1.ProtocolUDP, p.Protocol)
			assert.Equal(t, int32(9001), p.Port)
			assert.Nil(t, p.AppProtocol, "a UDP port has no appProtocol")
			continue
		}
		assert.Equal(t, corev1.ProtocolTCP, p.Protocol, "mesh port %q", p.Name)
	}
	assert.Equal(t, 1, udp, "the declared UDP port is on the Service once")
}

// TestProtocolAppProtocolIsTotal guards the map the generator names a port's
// class with. A missing entry does not fail: it yields "", so a class added to
// the enum but not here would give its ports an empty appProtocol.
func TestProtocolAppProtocolIsTotal(t *testing.T) {
	for _, p := range registry.ServedProtocols {
		got, ok := protocolAppProtocol[p]
		assert.True(t, ok, "%v has no app-protocol mapping; its ports would carry an empty appProtocol", p)
		assert.NotEmpty(t, got, "%v maps to the empty string", p)
	}
}
