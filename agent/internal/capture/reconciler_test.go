package capture

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/common/constants"
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
			Labels: map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{
				constants.AnnotationMeshService:     "svc-tcp",
				constants.AnnotationMeshPort:        "9000",
				constants.AnnotationMeshAppProtocol: "tcp",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.99"},
	}
	httpSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-http", Namespace: "aether-test",
			Labels: map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{
				constants.AnnotationMeshService:     "svc-http",
				constants.AnnotationMeshPort:        "8080",
				constants.AnnotationMeshAppProtocol: "http",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.50"},
	}
	headless := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-headless", Namespace: "aether-test",
			Labels: map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{
				constants.AnnotationMeshService:     "svc-headless",
				constants.AnnotationMeshAppProtocol: "tcp",
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	c := fake.NewClientBuilder().WithObjects(tcpSvc, httpSvc, headless).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, Sink: sink, Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	require.Len(t, sink.tcpServices, 1, "only the non-headless TCP service should produce a TCP floor entry")
	assert.Equal(t, "aether-test/svc-tcp", sink.tcpServices[0].ServiceName)
	assert.Equal(t, "10.96.0.99", sink.tcpServices[0].ClusterIP)
}

func TestReconcile_ProjectsAuthoritiesAndDNSRecords(t *testing.T) {
	mesh := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-1", Namespace: "aether-test",
			Labels:      map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{constants.AnnotationMeshService: "svc-1", constants.AnnotationMeshPort: "8080"},
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
