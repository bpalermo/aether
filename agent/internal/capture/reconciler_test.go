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

// projectWithAnnotations reconciles ONE mesh Service carrying annotations and
// returns what the sink was given for it.
func projectWithAnnotations(t *testing.T, annotations map[string]string) (CaptureTCPService, *fakeSink) {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "aether-test",
			Labels:      map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.9"},
	}
	sink := &fakeSink{}
	r := &Reconciler{Client: fake.NewClientBuilder().WithObjects(svc).Build(), Sink: sink, Log: slog.New(slog.DiscardHandler)}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	require.Len(t, sink.tcpServices, 1)
	return sink.tcpServices[0], sink
}

// TestIsHTTPAppProtocol used to pin how the reconciler read the Service's
// aether.io/app-protocol annotation as "HTTP or not". That reading is gone
// (proposal 043, Phase 1): the annotation said one thing about the whole
// service, and it was the wrong thing for a service with a raw-TCP port and an
// HTTP port (#1672). What replaced it is asserted over the same values: the
// annotation changes NOTHING in what is delivered. Name and address only; the
// ports and their classes come from the registry endpoints, in the cache.
func TestIsHTTPAppProtocol(t *testing.T) {
	want := CaptureTCPService{ServiceName: "aether-test/svc", ClusterIP: "10.96.0.9"}
	cases := []string{
		"http", "HTTP", "h2", "grpc", "http2",
		"", // empty = the 2.6 default
		"tcp", "TCP",
		"ws", // unrecognised
		"grpc-web",
	}
	for _, proto := range cases {
		t.Run(proto, func(t *testing.T) {
			got, sink := projectWithAnnotations(t, map[string]string{
				aetherlabels.AnnotationMeshService:     "svc",
				aetherlabels.AnnotationMeshAppProtocol: proto,
			})
			assert.Equal(t, want, got, "aether.io/app-protocol=%q must not change what is delivered", proto)
			assert.Equal(t, map[string]string{"aether-test/svc": "svc.aether-test.svc.cluster.local"}, sink.got)
			assert.Equal(t, map[string]string{"aether-test/svc": "10.96.0.9"}, sink.records)
		})
	}
	// Negative control: the comparison above can fail. A different address
	// IS a different delivery.
	got, _ := projectWithAnnotations(t, nil)
	other := got
	other.ClusterIP = "10.96.0.10"
	assert.NotEqual(t, want, other)
}

// TestReconcile_PortAnnotationIsNotRead: neither is aether.io/port. Both
// annotations are frozen at their chart-2.6 values for 2.6 agents and have no
// reader in this one.
func TestReconcile_PortAnnotationIsNotRead(t *testing.T) {
	base, _ := projectWithAnnotations(t, map[string]string{aetherlabels.AnnotationMeshService: "svc"})
	for _, port := range []string{"", "8080", "9000", "0", "not-a-port"} {
		got, _ := projectWithAnnotations(t, map[string]string{
			aetherlabels.AnnotationMeshService: "svc",
			aetherlabels.AnnotationMeshPort:    port,
		})
		assert.Equal(t, base, got, "aether.io/port=%q", port)
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

	// Nothing but the name and the address is delivered (proposal 043, Phase
	// 1). Which of the two gets raw-TCP chains is decided by the cache from
	// the registry endpoints, per port; the "tcp" and "http" annotations above
	// are not read, so the two entries differ in name and address only.
	assert.Equal(t, CaptureTCPService{ServiceName: "aether-test/svc-tcp", ClusterIP: "10.96.0.99"}, byName["aether-test/svc-tcp"])
	assert.Equal(t, CaptureTCPService{ServiceName: "aether-test/svc-http", ClusterIP: "10.96.0.50"}, byName["aether-test/svc-http"])
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

// TestIsUDPAppProtocol used to pin the carve-out that kept a datagram service
// off the TCP floor by reading "udp" out of the Service's annotation. The
// carve-out now lives where the floor is decided, in the cache, from the
// endpoints' own port classes. Here the same values assert what replaced the
// predicate: no spelling of the annotation, "udp" included, changes the
// delivery.
func TestIsUDPAppProtocol(t *testing.T) {
	want := CaptureTCPService{ServiceName: "aether-test/svc", ClusterIP: "10.96.0.9"}
	cases := []string{
		"udp", "UDP", "Udp", "tcp", "http", "",
		"ws",      // unrecognised
		"quic",    // not a protocol the mesh knows
		"udplite", // a prefix of nothing
	}
	for _, proto := range cases {
		t.Run(proto, func(t *testing.T) {
			got, _ := projectWithAnnotations(t, map[string]string{
				aetherlabels.AnnotationMeshService:     "svc",
				aetherlabels.AnnotationMeshAppProtocol: proto,
			})
			assert.Equal(t, want, got)
		})
	}
}

// TestReconcile_UDPServiceGetsNoTCPFloor is the projection-level consequence.
//
// A UDP service is still DELIVERED (it has a routable VIP, so it needs a
// mesh-DNS record and a capture authority), and the delivery carries nothing
// that could ask for a TCP floor: a portless /32 chain would put a tcp_proxy
// in front of the VIP naming a tcp: cluster that need not exist. Before
// proposal 043 that was a PrimaryIsTCP flag read from the annotation, which
// "udp" had to be carved out of; the field no longer exists, and the cache
// builds raw-TCP chains only from ports the endpoints declare raw TCP.
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
	assert.Equal(t, CaptureTCPService{ServiceName: "aether-test/svc-udp", ClusterIP: "10.96.0.77"}, got,
		"a UDP service's delivery is its name and address, with nothing that asks for a TCP floor")

	// And it keeps the things every VIP-bearing mesh Service gets.
	assert.Equal(t, "10.96.0.77", sink.records["aether-test/svc-udp"],
		"mesh-DNS must still resolve a UDP service's name")
}
