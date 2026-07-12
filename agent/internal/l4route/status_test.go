package l4route

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

type fakeSink struct{}

func (fakeSink) SetTCPServiceRoutes(map[string][]proxy.L4ServiceRoute) {}
func (fakeSink) SetTLSServiceRoutes(map[string][]proxy.L4ServiceRoute) {}
func (fakeSink) SetUDPServiceRoutes(map[string][]proxy.L4Backend)      {}

func l4Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, gatewayv1alpha2.Install(s))
	require.NoError(t, gatewayv1beta1.Install(s))
	return s
}

// A Service-parented TCPRoute with a resolvable backend gets Accepted=True +
// ResolvedRefs=True under the mesh controllerName.
func TestReconcile_TCPRouteStatus(t *testing.T) {
	tr := &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "tr1", Namespace: "ns", Generation: 1},
		Spec: gatewayv1alpha2.TCPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-1"},
			}},
			Rules: []gatewayv1alpha2.TCPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1"}}},
			}},
		},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(l4Scheme(t)).
		WithObjects(tr, svc).
		WithStatusSubresource(&gatewayv1alpha2.TCPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: fakeSink{}, MeshDomain: "mesh", Log: slog.Default(), tcpEnabled: true, tlsEnabled: true, udpEnabled: true, referenceGrantEnabled: true}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1alpha2.TCPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "tr1"}, got))
	require.Len(t, got.Status.Parents, 1)
	assert.Equal(t, gatewaystatus.MeshControllerName, got.Status.Parents[0].ControllerName)
	res := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, res)
	assert.Equal(t, metav1.ConditionTrue, res.Status)
}
