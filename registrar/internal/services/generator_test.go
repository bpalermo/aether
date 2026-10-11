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
	"aethermesh.dev/registry/endpointmeta"
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
		got, ok := portProtocolAppProtocol[endpointmeta.PortProtocolFromService(p)]
		assert.True(t, ok, "%v has no app-protocol mapping; its Services would be mislabelled http", p)
		assert.NotEmpty(t, got, "%v maps to the empty string, which apply() coerces to http", p)
	}
}

// mixedEndpoint is one pod serving an HTTP and a raw-TCP port, as the registry
// returns it under BOTH protocols (proposal 037): the same row, with the pod's
// primary port and the class of each port.
func mixedEndpoint(ip, ns string, primary uint32, classes map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	ports := make([]uint32, 0, len(classes))
	for p := range classes {
		ports = append(ports, p)
	}
	return &registryv1.ServiceEndpoint{
		Ip:                 ip,
		ClusterName:        "c1",
		Port:               primary,
		Ports:              ports,
		PortProtocols:      classes,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: ns},
	}
}

// seedListings seeds one service with the given endpoints per protocol.
func seedListings(svc, ns string, listings map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) *server.Snapshot {
	s := server.NewSnapshot()
	s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		serviceref.New(ns, svc).Key(): listings,
	})
	return s
}

const (
	classHTTP = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	classTCP  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
)

// TestGenerator_LabelsByPrimaryPortNotByFirstListing is #1672. A pod whose
// primary port is raw TCP and which also serves an HTTP port is listed under
// both protocols. The generator used to give the Service to the first protocol
// it iterated (HTTP), so the mesh Service said "http" about a service whose
// primary port speaks raw TCP, and the agent built it no TCP path.
func TestGenerator_LabelsByPrimaryPortNotByFirstListing(t *testing.T) {
	tcpFirst := mixedEndpoint("10.0.0.1", "aether-test", 9000, map[uint32]registryv1.PortProtocol{9000: classTCP, 8080: classHTTP})
	g, c := newGen(seedListings("tcpfirst", "aether-test", map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		registryv1.Service_PROTOCOL_HTTP: {tcpFirst},
		registryv1.Service_PROTOCOL_TCP:  {tcpFirst},
	}))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "tcpfirst")
	require.NoError(t, err)
	assert.Equal(t, "9000", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Equal(t, AppProtocolTCP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol],
		"the primary port is raw TCP; being listed under HTTP as well only says the pod also serves an HTTP port")
}

// The mirror shape keeps the label it always had.
func TestGenerator_HTTPPrimaryWithTCPSecondaryStaysHTTP(t *testing.T) {
	httpFirst := mixedEndpoint("10.0.0.1", "aether-test", 8080, map[uint32]registryv1.PortProtocol{8080: classHTTP, 9000: classTCP})
	g, c := newGen(seedListings("mixed", "aether-test", map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		registryv1.Service_PROTOCOL_HTTP: {httpFirst},
		registryv1.Service_PROTOCOL_TCP:  {httpFirst},
	}))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "mixed")
	require.NoError(t, err)
	assert.Equal(t, "8080", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
}

// A Service generated by a registrar from before the fix carries "http" for a
// TCP-primary mixed service; the next reconcile corrects it in place (same
// object, same ClusterIP).
func TestGenerator_CorrectsTheLabelOfAnExistingMixedService(t *testing.T) {
	stale := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tcpfirst", Namespace: "aether-test",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "tcpfirst",
				aetherlabels.AnnotationMeshPort:        "9000",
				aetherlabels.AnnotationMeshAppProtocol: AppProtocolHTTP,
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.77", Ports: meshServicePorts(18081)},
	}
	tcpFirst := mixedEndpoint("10.0.0.1", "aether-test", 9000, map[uint32]registryv1.PortProtocol{9000: classTCP, 8080: classHTTP})
	g, c := newGen(seedListings("tcpfirst", "aether-test", map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		registryv1.Service_PROTOCOL_HTTP: {tcpFirst},
		registryv1.Service_PROTOCOL_TCP:  {tcpFirst},
	}), stale)
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "tcpfirst")
	require.NoError(t, err)
	assert.Equal(t, AppProtocolTCP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
	assert.Equal(t, "10.96.0.77", svc.Spec.ClusterIP, "updated in place: the VIP every client resolved must not move")
}

// recordingHandler keeps the WARN records a test wants to read back.
type recordingHandler struct {
	slog.Handler
	warnings *[]string
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	*h.warnings = append(*h.warnings, line)
	return nil
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestGenerator_EndpointsDisagreeAboutThePrimary pins the rule for a service
// whose endpoints declare different primaries (endpointmeta.ServicePrimaryOf:
// the primary of the most endpoints; on a tie HTTP, then the lower port), that
// the answer does not depend on the order the registry returns the rows in,
// and that the disagreement is reported once per distinct state, not on every
// reconcile.
func TestGenerator_EndpointsDisagreeAboutThePrimary(t *testing.T) {
	a := mixedEndpoint("10.0.0.1", "aether-test", 9000, map[uint32]registryv1.PortProtocol{9000: classTCP, 8080: classHTTP})
	b := mixedEndpoint("10.0.0.2", "aether-test", 9000, map[uint32]registryv1.PortProtocol{9000: classTCP, 8080: classHTTP})
	c := mixedEndpoint("10.0.0.3", "aether-test", 8080, map[uint32]registryv1.PortProtocol{8080: classHTTP, 9000: classTCP})

	for _, order := range [][]*registryv1.ServiceEndpoint{{a, b, c}, {c, b, a}} {
		var warnings []string
		g, cl := newGen(seedListings("split", "aether-test", map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
			registryv1.Service_PROTOCOL_HTTP: order,
			registryv1.Service_PROTOCOL_TCP:  order,
		}))
		g.Log = slog.New(recordingHandler{Handler: slog.DiscardHandler, warnings: &warnings})
		g.reconcile(context.Background())
		g.reconcile(context.Background())

		svc, err := get(t, cl, "aether-test", "split")
		require.NoError(t, err)
		assert.Equal(t, AppProtocolTCP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol], "two of three endpoints declare a TCP primary")
		assert.Equal(t, "9000", svc.Annotations[aetherlabels.AnnotationMeshPort])

		require.Len(t, warnings, 1, "reported once, not once per reconcile: %v", warnings)
		assert.Contains(t, warnings[0], "aether-test/split")
		assert.Contains(t, warnings[0], "9000/tcp (2 endpoints), 8080/http (1 endpoint)")
	}

	// A tie, and then the disagreement ends: nothing more is said.
	var warnings []string
	snap := seedListings("split", "aether-test", map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		registryv1.Service_PROTOCOL_HTTP: {a, c},
		registryv1.Service_PROTOCOL_TCP:  {a, c},
	})
	g, cl := newGen(snap)
	g.Log = slog.New(recordingHandler{Handler: slog.DiscardHandler, warnings: &warnings})
	g.reconcile(context.Background())
	svc, err := get(t, cl, "aether-test", "split")
	require.NoError(t, err)
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol], "one each: the tie goes to HTTP")
	require.Len(t, warnings, 1)

	snap.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		serviceref.New("aether-test", "split").Key(): {
			registryv1.Service_PROTOCOL_HTTP: {a},
			registryv1.Service_PROTOCOL_TCP:  {a},
		},
	})
	g.reconcile(context.Background())
	g.reconcile(context.Background())
	svc, err = get(t, cl, "aether-test", "split")
	require.NoError(t, err)
	assert.Equal(t, AppProtocolTCP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
	assert.Len(t, warnings, 1, "an agreeing service is not warned about")
}

// An endpoint with no Kubernetes namespace does not make a mesh Service and
// does not vote for its primary, as before.
func TestGenerator_OnlyNamespacedEndpointsDecide(t *testing.T) {
	bare := &registryv1.ServiceEndpoint{Ip: "10.0.0.9", ClusterName: "c1", Port: 7000}
	namespaced := mixedEndpoint("10.0.0.1", "aether-test", 9000, map[uint32]registryv1.PortProtocol{9000: classTCP})
	g, c := newGen(seedListings("svc", "aether-test", map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		registryv1.Service_PROTOCOL_HTTP: {bare},
		registryv1.Service_PROTOCOL_TCP:  {namespaced},
	}))
	g.reconcile(context.Background())

	svc, err := get(t, c, "aether-test", "svc")
	require.NoError(t, err)
	assert.Equal(t, "9000", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Equal(t, AppProtocolTCP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
}
