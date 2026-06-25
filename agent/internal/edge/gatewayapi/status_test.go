package gatewayapi

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
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
)

type statusFakeSink struct{}

func (statusFakeSink) SetVirtualHosts([]cache.VirtualHost) {}
func (statusFakeSink) SetEdgeTLSSecrets(context.Context, map[string]cache.EdgeTLSCert) error {
	return nil
}
func (statusFakeSink) SetEdgeTCPRoutes([]proxy.EdgeL4TCPRoute) {}
func (statusFakeSink) SetEdgeTLSRoutes([]proxy.EdgeL4TLSRoute) {}

func statusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, gatewayv1alpha2.Install(s))
	return s
}

// TestReconcile_GatewayAndRouteStatus: an HTTP listener Gateway of our class with
// an attached HTTPRoute (resolvable backend) gets Accepted/Programmed=True, a
// listener status with attachedRoutes=1, and the route gets Accepted=True +
// ResolvedRefs=True under our edge controllerName. The GatewayClass gets Accepted.
func TestReconcile_GatewayAndRouteStatus(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Port:     80,
				Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
	hr := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"},
			}},
			Hostnames: []gatewayv1.Hostname{"api.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1", Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "ns"}}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr, svc).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	// GatewayClass Accepted.
	gotGC := &gatewayv1.GatewayClass{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "aether"}, gotGC))
	acc := meta.FindStatusCondition(gotGC.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)

	// Gateway Accepted + Programmed + listener attachedRoutes=1.
	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "edge"}, gotGW))
	prog := meta.FindStatusCondition(gotGW.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, prog)
	assert.Equal(t, metav1.ConditionTrue, prog.Status)
	require.Len(t, gotGW.Status.Listeners, 1)
	assert.Equal(t, gatewayv1.SectionName("http"), gotGW.Status.Listeners[0].Name)
	assert.Equal(t, int32(1), gotGW.Status.Listeners[0].AttachedRoutes)
	require.Len(t, gotGW.Status.Listeners[0].SupportedKinds, 1)
	assert.Equal(t, gatewayv1.Kind("HTTPRoute"), gotGW.Status.Listeners[0].SupportedKinds[0].Kind)

	// Route Accepted=True + ResolvedRefs=True under our controller.
	gotHR := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, gotHR))
	require.Len(t, gotHR.Status.Parents, 1)
	assert.Equal(t, gatewaystatus.EdgeControllerName, gotHR.Status.Parents[0].ControllerName)
	racc := meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, racc)
	assert.Equal(t, metav1.ConditionTrue, racc.Status)
	rres := meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionTrue, rres.Status)
}
