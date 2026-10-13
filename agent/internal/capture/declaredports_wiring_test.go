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

// TestDeclaredPorts_OneClassPerNumberAndNoMeshPort: a Service is an object
// anyone with the rights can edit. A number carried as HTTP and as another
// class is HTTP only (in two classes it put one authority in two virtual
// hosts, and the proxy rejects such a route table whole); and 18081 and 18082
// are no port of a service whatever the Service says.
func TestDeclaredPorts_OneClassPerNumberAndNoMeshPort(t *testing.T) {
	app := func(s string) *string { return &s }
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: "p8080-tcp", Port: 8080, AppProtocol: app("http")},
		{Name: "p8080-udp", Port: 8080, Protocol: corev1.ProtocolUDP},
		{Name: "p9000-tcp", Port: 9000, AppProtocol: app("tcp")},
		{Name: "p18081-tcp", Port: 18081, AppProtocol: app("http")},
		{Name: "p18082-tcp", Port: 18082, AppProtocol: app("tcp")},
		{Name: "p18082-udp", Port: 18082, Protocol: corev1.ProtocolUDP},
	}}}
	http, other := declaredPorts(svc)
	assert.Equal(t, []uint32{8080}, http)
	assert.Equal(t, []uint32{9000}, other, "8080 is HTTP and nothing else; the mesh's ports are no port of the service")
}

// TestReconcile_DeliversTheDeclaredPorts: the reconciler hands the sink the
// real ports of each generated Service. A service the node holds no endpoint
// of is routed and refused by them, so a reconciler that read them and did not
// pass them on would leave every cold request to the authority it spells.
func TestReconcile_DeliversTheDeclaredPorts(t *testing.T) {
	app := func(s string) *string { return &s }
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "aether-test",
			Labels:      map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{aetherlabels.AnnotationMeshService: "svc"},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.9", Ports: []corev1.ServicePort{
			{Name: "mesh", Port: 18081},
			{Name: "mesh-tcp", Port: 18082},
			{Name: "p9090-tcp", Port: 9090, AppProtocol: app("http")},
			{Name: "p8080-tcp", Port: 8080, AppProtocol: app("http")},
			{Name: "p9000-tcp", Port: 9000, AppProtocol: app("tcp")},
			{Name: "p5353-udp", Port: 5353, Protocol: corev1.ProtocolUDP},
		}},
	}
	sink := &fakeSink{}
	r := &Reconciler{Client: fake.NewClientBuilder().WithObjects(svc).Build(), Sink: sink, Log: slog.New(slog.DiscardHandler)}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	require.Len(t, sink.tcpServices, 1)
	assert.Equal(t, []uint32{8080, 9090}, sink.tcpServices[0].DeclaredHTTP)
	assert.Equal(t, []uint32{5353, 9000}, sink.tcpServices[0].DeclaredOther)
}
