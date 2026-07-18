package gatewayapi

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"testing"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

type statusFakeSink struct{}

func (statusFakeSink) SetVirtualHosts([]cache.VirtualHost) {}
func (statusFakeSink) SetEdgeTLSSecrets(context.Context, map[string]cache.EdgeTLSCert) error {
	return nil
}
func (statusFakeSink) SetEdgeTCPRoutes([]proxy.EdgeL4TCPRoute)  {}
func (statusFakeSink) SetEdgeTLSRoutes([]proxy.EdgeL4TLSRoute)  {}
func (statusFakeSink) SetEdgeHTTPRedirect(bool)                 {}
func (statusFakeSink) SetEdgeGateways([]cache.EdgeGatewayEntry) {}
func (statusFakeSink) HasRegistryService(string) bool           { return false }

func statusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
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

// resolvedRefsFixture builds a class-aether GatewayClass + an HTTP Gateway "edge" in
// "ns" + an HTTPRoute "r1" in "ns" whose single backendRef is `ref`. Used by the
// ResolvedRefs existence/kind tests.
func resolvedRefsFixture(ref gatewayv1.BackendObjectReference) (*gatewayv1.GatewayClass, *gatewayv1.Gateway, *gatewayv1.HTTPRoute) {
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
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
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}}},
			}},
		},
	}
	return gc, gw, hr
}

// routeResolvedRefs returns the edge controller's ResolvedRefs condition on r1.
func routeResolvedRefs(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	gotHR := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, gotHR))
	require.Len(t, gotHR.Status.Parents, 1)
	return meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
}

// statusFakeSinkWithRegistry is a RouteSink that reports specific service names as
// registry/mesh services. Used to test registry-aware backend-existence behavior.
type statusFakeSinkWithRegistry struct {
	registryServices map[string]bool
}

func (s statusFakeSinkWithRegistry) SetVirtualHosts([]cache.VirtualHost) {}
func (s statusFakeSinkWithRegistry) SetEdgeTLSSecrets(context.Context, map[string]cache.EdgeTLSCert) error {
	return nil
}
func (s statusFakeSinkWithRegistry) SetEdgeTCPRoutes([]proxy.EdgeL4TCPRoute)  {}
func (s statusFakeSinkWithRegistry) SetEdgeTLSRoutes([]proxy.EdgeL4TLSRoute)  {}
func (s statusFakeSinkWithRegistry) SetEdgeHTTPRedirect(bool)                 {}
func (s statusFakeSinkWithRegistry) SetEdgeGateways([]cache.EdgeGatewayEntry) {}
func (s statusFakeSinkWithRegistry) HasRegistryService(name string) bool {
	return s.registryServices[name]
}

// TestReconcile_BackendNotFound: an HTTPRoute whose core-Service backendRef names a
// Service that does not exist gets ResolvedRefs=False/BackendNotFound.
func TestReconcile_BackendNotFound(t *testing.T) {
	gc, gw, hr := resolvedRefsFixture(gatewayv1.BackendObjectReference{Name: "missing-svc", Port: ptr(gatewayv1.PortNumber(8080))})

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	rres := routeResolvedRefs(t, c)
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionFalse, rres.Status, "missing Service backend → ResolvedRefs False")
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), rres.Reason)
}

// TestReconcile_BackendExists: once the referenced Service is created, ResolvedRefs
// flips to True (the Service watch re-triggers reconciliation in production).
func TestReconcile_BackendExists(t *testing.T) {
	gc, gw, hr := resolvedRefsFixture(gatewayv1.BackendObjectReference{Name: "svc-1", Port: ptr(gatewayv1.PortNumber(8080))})
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "ns"}}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr, svc).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	rres := routeResolvedRefs(t, c)
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionTrue, rres.Status, "existing Service backend → ResolvedRefs True")
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), rres.Reason)
}

// TestReconcile_RegistryOnlyBackend_ResolvedRefs: an HTTPRoute whose backendRef is a
// mesh/registry service (HasRegistryService=true) but has no k8s Service in the route
// namespace gets ResolvedRefs=True. This is the regression guard for #367: before the
// fix, such a backend returned BackendNotFound because the namespaced Service.Get
// failed. The registry-aware check must report it as resolved.
func TestReconcile_RegistryOnlyBackend_ResolvedRefs(t *testing.T) {
	gc, gw, hr := resolvedRefsFixture(gatewayv1.BackendObjectReference{Name: "echo", Port: ptr(gatewayv1.PortNumber(8080))})

	// Client has NO Services — "echo" is registry-only (no k8s Service in "ns").
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	// Sink reports "echo" as a mesh/registry service.
	sink := statusFakeSinkWithRegistry{registryServices: map[string]bool{"ns/echo": true}}
	r := &Reconciler{Client: c, APIReader: c, Sink: sink, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	rres := routeResolvedRefs(t, c)
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionTrue, rres.Status,
		"registry-only backend must report ResolvedRefs=True (not BackendNotFound)")
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), rres.Reason)
}

// TestReconcile_BackendInvalidKind: a backendRef whose kind is not the core Service
// kind gets ResolvedRefs=False/InvalidKind, and InvalidKind takes precedence over
// BackendNotFound (the existence check never runs for a bad kind).
func TestReconcile_BackendInvalidKind(t *testing.T) {
	notService := gatewayv1.Kind("Frobnicator")
	gc, gw, hr := resolvedRefsFixture(gatewayv1.BackendObjectReference{
		Kind: &notService, Name: "missing-svc", Port: ptr(gatewayv1.PortNumber(8080)),
	})

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	rres := routeResolvedRefs(t, c)
	require.NotNil(t, rres)
	assert.Equal(t, metav1.ConditionFalse, rres.Status, "non-core-Service kind → ResolvedRefs False")
	assert.Equal(t, string(gatewayv1.RouteReasonInvalidKind), rres.Reason, "InvalidKind wins over BackendNotFound")
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
	// The cross-ns backend Service exists in ns-b; the gating condition under test is
	// the ReferenceGrant, not existence (RefNotPermitted must win over BackendNotFound).
	svcB := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-b", Namespace: "ns-b"}}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr, svcB).
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
	// The backend Service exists so the route resolves cleanly (Accepted is the
	// assertion under test, not ResolvedRefs).
	infraSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "infra-backend", Namespace: "gateway-conformance-infra"}}

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gc, gw, hr, infraSvc).
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
// advertised supportedFeatures, sorted ascending and including HTTPRoute plus the
// implemented redirect/rewrite filter features, and omitting features aether does
// not implement (mirror) or does not serve on the north-south Gateway (GRPCRoute).
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
	assert.Contains(t, names, "HTTPRouteRequestTimeout")
	// ReferenceGrant is now implemented (cross-namespace backendRef admission +
	// status) and therefore advertised so the suite runs those tests.
	assert.Contains(t, names, "ReferenceGrant")
	// The RequestRedirect filter (port/scheme/path redirect) and URLRewrite filter
	// (host/path rewrite) are implemented on edge + GAMMA HTTPRoute, so the suite
	// should RUN (not skip) those tests.
	assert.Contains(t, names, "HTTPRoutePortRedirect")
	assert.Contains(t, names, "HTTPRouteSchemeRedirect")
	assert.Contains(t, names, "HTTPRoutePathRedirect")
	assert.Contains(t, names, "HTTPRouteHostRewrite")
	assert.Contains(t, names, "HTTPRoutePathRewrite")
	// GRPCRoute is served only east-west via GAMMA (parentRef=Service), not on the
	// north-south Gateway, so it must NOT be advertised on the GatewayClass — else
	// the GATEWAY-GRPC suite runs gRPC traffic the edge Gateway cannot serve.
	assert.NotContains(t, names, "GRPCRoute")
	// Unsupported features must NOT be advertised (so their suites skip): request
	// mirroring is not implemented, and the 303/307/308 redirect status codes are not
	// (only 301/302).
	assert.NotContains(t, names, "HTTPRouteRequestMirror")
	assert.NotContains(t, names, "HTTPRoute303RedirectStatusCode")
}

// TestWriteGatewayStatus_RetriesOnConflict: both edge replicas reconcile the same
// cluster-wide Gateways active-active, so their status writes collide with 409
// Conflict. writeGatewayStatus must re-Get + re-merge + retry so the address still
// lands (the previous code logged the conflict and dropped the write, leaving the
// Gateway address-less past the conformance GatewayMustHaveAddress window — the
// HTTPRouteRedirectPortAndScheme timeout).
func TestWriteGatewayStatus_RetriesOnConflict(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}

	var statusUpdates int
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(gw).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusUpdates++
				if statusUpdates == 1 {
					// Simulate the other replica winning the first write.
					return apierrors.NewConflict(
						schema.GroupResource{Group: gatewayv1.GroupName, Resource: "gateways"},
						obj.GetName(), errors.New("the object has been modified"),
					)
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()

	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", Log: slog.Default()}

	inputs := []listenerStatusInput{{
		name:           "http",
		supportedKinds: []gatewayv1.RouteGroupKind{{Kind: "HTTPRoute"}},
		attached:       0,
		accepted:       gatewaystatus.Condition{Type: string(gatewayv1.ListenerConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.ListenerReasonAccepted)},
		programmed:     gatewaystatus.Condition{Type: string(gatewayv1.ListenerConditionProgrammed), Status: metav1.ConditionTrue, Reason: string(gatewayv1.ListenerReasonProgrammed)},
		resolved:       gatewaystatus.Condition{Type: string(gatewayv1.ListenerConditionResolvedRefs), Status: metav1.ConditionTrue, Reason: string(gatewayv1.ListenerReasonResolvedRefs)},
	}}
	acceptedTop := gatewaystatus.Condition{Type: string(gatewayv1.GatewayConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.GatewayReasonAccepted)}
	programmedTop := gatewaystatus.Condition{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: string(gatewayv1.GatewayReasonProgrammed)}
	addrs := []gatewayv1.GatewayStatusAddress{{Type: ptr(gatewayv1.IPAddressType), Value: "192.168.100.50"}}

	key := types.NamespacedName{Namespace: "ns", Name: "edge"}
	require.NoError(t, r.writeGatewayStatus(context.Background(), key, inputs, acceptedTop, programmedTop, addrs))

	// The first write conflicted; the retry must have written (≥2 attempts) and the
	// address must have landed despite the conflict.
	assert.GreaterOrEqual(t, statusUpdates, 2, "expected a retry after the 409 conflict")
	got := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), key, got))
	require.Len(t, got.Status.Addresses, 1)
	assert.Equal(t, "192.168.100.50", got.Status.Addresses[0].Value)
	prog := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, prog)
	assert.Equal(t, metav1.ConditionTrue, prog.Status)
}
