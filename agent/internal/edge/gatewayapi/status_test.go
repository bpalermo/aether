package gatewayapi

import (
	"context"
	"log/slog"
	"sort"
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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

type statusFakeSink struct{}

func (statusFakeSink) SetVirtualHosts([]cache.VirtualHost) {}
func (statusFakeSink) SetEdgeTLSSecrets(context.Context, map[string]cache.EdgeTLSCert) error {
	return nil
}
func (statusFakeSink) SetEdgeTCPRoutes([]proxy.EdgeL4TCPRoute) {}
func (statusFakeSink) SetEdgeTLSRoutes([]proxy.EdgeL4TLSRoute) {}
func (statusFakeSink) SetEdgeHTTPRedirect(bool)                {}

func statusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, gatewayv1alpha2.Install(s))
	require.NoError(t, gatewayv1beta1.Install(s))
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
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

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

// TestReconcile_CrossNamespaceBackend_RefNotPermitted: an HTTPRoute in ns A whose
// backendRef targets a Service in ns B with NO ReferenceGrant gets ResolvedRefs=
// False/RefNotPermitted; adding a matching grant flips it to True.
func TestReconcile_CrossNamespaceBackend_RefNotPermitted(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "ns-a", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	nsB := gatewayv1.Namespace("ns-b")
	hr := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns-a", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"},
			}},
			Hostnames: []gatewayv1.Hostname{"api.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-b", Namespace: &nsB, Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns-a", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotHR := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns-a", Name: "r1"}, gotHR))
	require.Len(t, gotHR.Status.Parents, 1)
	rres := meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionFalse, rres.Status, "ungranted cross-ns backend → ResolvedRefs False")
	assert.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), rres.Reason)

	// Add a matching ReferenceGrant in ns-b → ResolvedRefs flips to True.
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns-b"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "ns-a"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
	require.NoError(t, c.Create(context.Background(), grant))

	_, err = r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns-a", Name: "r1"}, gotHR))
	rres = meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionTrue, rres.Status, "granted cross-ns backend → ResolvedRefs True")
}

// TestReconcile_GatewayInNonEdgeNamespace: a Gateway of our class living in a
// namespace OTHER than the edge's own (r.Namespace) is reconciled cluster-wide and
// reaches Accepted=True + Programmed=True, with its attached HTTPRoute (also in a
// foreign namespace) getting Accepted=True under our controller. This is the
// conformance unlock — the suite creates its objects in its own namespaces.
func TestReconcile_GatewayInNonEdgeNamespace(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	// Gateway + Route in "gateway-conformance-infra", NOT the edge namespace ("aether-ingress").
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "conformance-gw", Namespace: "gateway-conformance-infra", Generation: 1},
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
		ObjectMeta: metav1.ObjectMeta{Name: "infra-route", Namespace: "gateway-conformance-infra", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "conformance-gw"},
			}},
			Hostnames: []gatewayv1.Hostname{"infra.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "infra-backend", Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	// Edge's own namespace is aether-ingress; the Gateway lives elsewhere.
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "aether-ingress", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "gateway-conformance-infra", Name: "conformance-gw"}, gotGW))
	acc := meta.FindStatusCondition(gotGW.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
	prog := meta.FindStatusCondition(gotGW.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, prog)
	assert.Equal(t, metav1.ConditionTrue, prog.Status)
	require.Len(t, gotGW.Status.Listeners, 1)
	assert.Equal(t, int32(1), gotGW.Status.Listeners[0].AttachedRoutes)

	gotHR := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "gateway-conformance-infra", Name: "infra-route"}, gotHR))
	require.Len(t, gotHR.Status.Parents, 1)
	racc := meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, racc)
	assert.Equal(t, metav1.ConditionTrue, racc.Status)
}

// TestReconcile_GatewayStatusAddresses: a reconciled class-aether Gateway (in any
// namespace) gets status.addresses = the edge LoadBalancer Service's assigned IP
// (proposal 021 Phase 1, shared edge address), resolved at runtime from the edge
// Service's status.loadBalancer.ingress.
func TestReconcile_GatewayStatusAddresses(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	// Gateway in a foreign namespace — it still shares the one edge address.
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "conformance-gw", Namespace: "gateway-conformance-infra", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	// The edge's own LoadBalancer Service, with MetalLB having assigned an IP.
	edgeSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "aether-edge", Namespace: "aether-ingress"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: "192.168.100.101"}},
		}},
	}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, edgeSvc).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}).
		Build()
	r := &Reconciler{
		Client: c, APIReader: c, Sink: statusFakeSink{},
		Namespace: "aether-ingress", EdgeServiceName: "aether-edge",
		GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default(),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "gateway-conformance-infra", Name: "conformance-gw"}, gotGW))
	require.Len(t, gotGW.Status.Addresses, 1)
	require.NotNil(t, gotGW.Status.Addresses[0].Type)
	assert.Equal(t, gatewayv1.IPAddressType, *gotGW.Status.Addresses[0].Type)
	assert.Equal(t, "192.168.100.101", gotGW.Status.Addresses[0].Value)
}

// TestReconcile_GatewayStatusAddresses_NoLBIP: when the edge LoadBalancer Service
// has no assigned ingress address yet (MetalLB pending), no status.addresses is
// written — the Gateway is left address-less rather than getting an empty/garbage
// address. (The Service watch re-triggers reconciliation once the IP lands.)
func TestReconcile_GatewayStatusAddresses_NoLBIP(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "aether-ingress", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	// Edge Service exists but MetalLB hasn't assigned an IP (empty Ingress).
	edgeSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "aether-edge", Namespace: "aether-ingress"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, edgeSvc).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}).
		Build()
	r := &Reconciler{
		Client: c, APIReader: c, Sink: statusFakeSink{},
		Namespace: "aether-ingress", EdgeServiceName: "aether-edge",
		GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default(),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: "edge"}, gotGW))
	// Conditions are still published; addresses are not (LB IP unset).
	prog := meta.FindStatusCondition(gotGW.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, prog)
	assert.Equal(t, metav1.ConditionTrue, prog.Status)
	assert.Empty(t, gotGW.Status.Addresses, "no LB IP assigned → no status.addresses")
}

// TestReconcile_GatewayClassSupportedFeatures: the GatewayClass status carries the
// advertised supportedFeatures, sorted ascending and including HTTPRoute/GRPCRoute,
// and omitting features aether does not implement.
func TestReconcile_GatewayClassSupportedFeatures(t *testing.T) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc).
		WithStatusSubresource(&gatewayv1.GatewayClass{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "aether-ingress", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotGC := &gatewayv1.GatewayClass{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "aether"}, gotGC))
	require.NotEmpty(t, gotGC.Status.SupportedFeatures)

	names := make([]string, 0, len(gotGC.Status.SupportedFeatures))
	for _, f := range gotGC.Status.SupportedFeatures {
		names = append(names, string(f.Name))
	}
	assert.True(t, sort.StringsAreSorted(names), "supportedFeatures must be ascending by name")
	assert.Contains(t, names, "HTTPRoute")
	assert.Contains(t, names, "GRPCRoute")
	assert.Contains(t, names, "HTTPRouteRequestTimeout")
	// ReferenceGrant is now implemented (cross-namespace backendRef admission +
	// status) and therefore advertised so the suite runs those tests.
	assert.Contains(t, names, "ReferenceGrant")
	// Unsupported features must NOT be advertised (so their suites skip).
	assert.NotContains(t, names, "HTTPRouteRequestMirror")
}
