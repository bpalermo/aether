package capture

import (
	"context"
	"log/slog"
	"testing"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type fakeSink struct {
	got         map[string]string
	records     map[string]string
	tcpServices []CaptureTCPService
}

func (f *fakeSink) SetCaptureAuthorities(a map[string]string)   { f.got = a }
func (f *fakeSink) SetMeshDNSRecords(r map[string]string)       { f.records = r }
func (f *fakeSink) SetCaptureTCPServices(s []CaptureTCPService) { f.tcpServices = s }

func TestIsHTTPAppProtocol(t *testing.T) {
	cases := []struct {
		proto string
		want  bool
	}{
		{"http", true},
		{"HTTP", true},
		{"h2", true},
		{"grpc", true},
		{"http2", true},
		{"", true}, // empty = default HTTP
		{"tcp", false},
		{"TCP", false},
		{"ws", false}, // unrecognised = TCP
		{"grpc-web", false},
	}
	for _, c := range cases {
		t.Run(c.proto, func(t *testing.T) {
			assert.Equal(t, c.want, isHTTPAppProtocol(c.proto))
		})
	}
}

func TestReconcile_ProjectsTCPServices(t *testing.T) {
	tcpSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-tcp", Namespace: "aether-test",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-tcp",
				aetherlabels.AnnotationMeshPort:        "9000",
				aetherlabels.AnnotationMeshAppProtocol: "tcp",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.99"},
	}
	httpSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-http", Namespace: "aether-test",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-http",
				aetherlabels.AnnotationMeshPort:        "8080",
				aetherlabels.AnnotationMeshAppProtocol: "http",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.50"},
	}
	headless := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-headless", Namespace: "aether-test",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-headless",
				aetherlabels.AnnotationMeshAppProtocol: "tcp",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	c := fake.NewClientBuilder().WithObjects(tcpSvc, httpSvc, headless).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, Sink: sink, Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	// Proposal 037 design (d): EVERY mesh Service with a routable VIP is
	// delivered, not only the non-HTTP ones, because an HTTP-primary service
	// can still serve raw-TCP ports that need chains. The headless one is still
	// skipped — there is no VIP to match on.
	byName := map[string]CaptureTCPService{}
	for _, s := range sink.tcpServices {
		byName[s.ServiceName] = s
	}
	require.Len(t, sink.tcpServices, 2, "both VIP-bearing services are delivered; the headless one is not")
	require.Contains(t, byName, "aether-test/svc-tcp")
	require.Contains(t, byName, "aether-test/svc-http")
	assert.NotContains(t, byName, "aether-test/svc-headless")

	assert.Equal(t, "10.96.0.99", byName["aether-test/svc-tcp"].ClusterIP)

	// PrimaryIsTCP is what gates the PORTLESS /32 floor chain. Getting this
	// wrong for the HTTP service is not a cosmetic error: a per-IP chain
	// outranks the HCM catch-all's application_protocols match, so it would
	// swallow every HTTP request to that VIP.
	assert.True(t, byName["aether-test/svc-tcp"].PrimaryIsTCP)
	assert.False(t, byName["aether-test/svc-http"].PrimaryIsTCP,
		"an HTTP-primary service must never get the portless floor chain")
}

func TestReconcile_ProjectsAuthoritiesAndDNSRecords(t *testing.T) {
	mesh := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-1", Namespace: "aether-test",
			Labels:      map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{aetherlabels.AnnotationMeshService: "svc-1", aetherlabels.AnnotationMeshPort: "8080"},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.42"},
	}
	// A non-mesh Service of the same shape must be ignored (label-scoped List).
	user := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}}

	c := fake.NewClientBuilder().WithObjects(mesh, user).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, Sink: sink, Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"aether-test/svc-1": "svc-1.aether-test.svc.cluster.local"}, sink.got)
	assert.Equal(t, map[string]string{"aether-test/svc-1": "10.96.0.42"}, sink.records, "ClusterIP -> the <svc>.<meshDomain> A record")
}

// TestIsUDPAppProtocol pins the carve-out that keeps a datagram service off the
// TCP floor. Note the asymmetry with isHTTPAppProtocol: an unrecognised value is
// still TCP (the conservative default), so this predicate is deliberately narrow
// -- it is not "everything that is not HTTP".
func TestIsUDPAppProtocol(t *testing.T) {
	cases := []struct {
		proto string
		want  bool
	}{
		{"udp", true},
		{"UDP", true},
		{"Udp", true},
		{"tcp", false},
		{"http", false},
		{"", false},
		{"ws", false},      // unrecognised stays TCP, not UDP
		{"quic", false},    // not a protocol the mesh knows
		{"udplite", false}, // prefix match would be wrong
	}
	for _, c := range cases {
		t.Run(c.proto, func(t *testing.T) {
			assert.Equal(t, c.want, isUDPAppProtocol(c.proto))
		})
	}
}

// TestReconcile_UDPServiceGetsNoTCPFloor is the projection-level consequence.
//
// A UDP service is still DELIVERED (it has a routable VIP, so it needs a
// mesh-DNS record and a capture authority), but it must not be marked
// PrimaryIsTCP: the portless /32 floor chain that flag gates would put a
// tcp_proxy in front of the VIP naming a tcp: cluster that need not exist.
// Before the carve-out, "udp" fell through isHTTPAppProtocol's default branch
// and got exactly that.
func TestReconcile_UDPServiceGetsNoTCPFloor(t *testing.T) {
	udpSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-udp", Namespace: "aether-test",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     "svc-udp",
				aetherlabels.AnnotationMeshPort:        "9001",
				aetherlabels.AnnotationMeshAppProtocol: "udp",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.77"},
	}

	c := fake.NewClientBuilder().WithObjects(udpSvc).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, Sink: sink, Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	byName := map[string]CaptureTCPService{}
	for _, s := range sink.tcpServices {
		byName[s.ServiceName] = s
	}

	got, ok := byName["aether-test/svc-udp"]
	require.True(t, ok, "a UDP service with a routable VIP is still delivered")
	assert.Equal(t, "10.96.0.77", got.ClusterIP)
	assert.False(t, got.PrimaryIsTCP,
		"a UDP service must not get the portless TCP floor chain")

	// And it keeps the things every VIP-bearing mesh Service gets.
	assert.Equal(t, "10.96.0.77", sink.records["aether-test/svc-udp"],
		"mesh-DNS must still resolve a UDP service's name")
}
