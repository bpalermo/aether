package gamma

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
)

func ptr[T any](v T) *T { return &v }

type fakeSink struct{ routes map[string][]proxy.GammaRoute }

func (f *fakeSink) SetServiceRoutes(r map[string][]proxy.GammaRoute) { f.routes = r }

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	return s
}

func httpRoute(name, ns, svcParent, backend string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Service")), Name: gatewayv1.ObjectName(svcParent)},
			}},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(backend), Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}
}

// A Service-parented HTTPRoute with a resolvable backend Service gets
// Accepted=True + ResolvedRefs=True written to its RouteParentStatus.
func TestReconcile_WritesAcceptedResolved(t *testing.T) {
	hr := httpRoute("r1", "ns", "svc-1", "svc-1")
	backend := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, backend).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	assert.Equal(t, gatewaystatus.MeshControllerName, got.Status.Parents[0].ControllerName)
	acc := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
	res := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, res)
	assert.Equal(t, metav1.ConditionTrue, res.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), res.Reason)
}

// aether resolves backends by NAME via the registry, so a valid Service-kind
// backendRef gets ResolvedRefs=True even without a matching k8s Service object
// (a genuinely-absent backend surfaces at runtime as no endpoints / 503).
func TestReconcile_BackendRefsResolvedByName(t *testing.T) {
	hr := httpRoute("r1", "ns", "svc-1", "no-k8s-svc")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	res := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, res)
	assert.Equal(t, metav1.ConditionTrue, res.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), res.Reason)
	acc := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
}

// backendsResolve validates the backendRef shape (aether resolves by name via the
// registry): a valid core Service-kind ref is resolved; a non-Service ref is
// InvalidKind; an empty name is BackendNotFound.
func TestBackendsResolve_Shapes(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	svcKind := gatewayv1.Kind("Service")
	otherKind := gatewayv1.Kind("Foo")

	ok, _, _ := r.backendsResolve(context.Background(), "ns", []gatewayv1.BackendObjectReference{
		{Name: "svc-1", Kind: &svcKind},
	})
	assert.True(t, ok, "valid Service-kind ref resolves")

	ok, reason, _ := r.backendsResolve(context.Background(), "ns", []gatewayv1.BackendObjectReference{
		{Name: "x", Kind: &otherKind},
	})
	assert.False(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonInvalidKind), reason)

	ok, reason, _ = r.backendsResolve(context.Background(), "ns", []gatewayv1.BackendObjectReference{
		{Name: ""},
	})
	assert.False(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), reason)
}

func TestServiceParents(t *testing.T) {
	hr := &gatewayv1.HTTPRoute{Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
		ParentRefs: []gatewayv1.ParentReference{
			{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-1"},
			{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"}, // ignored
			{Name: "no-kind"}, // no kind → ignored
		},
	}}}
	assert.Equal(t, []string{"svc-1"}, serviceParents(hr.Spec.ParentRefs))
}

// TestBuildGammaRoute: matches → GammaMatch, weighted backendRefs → GammaBackend
// with resolved cluster names + bare service, request timeout parsed.
func TestBuildGammaRoute(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path:    &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/admin")},
			Headers: []gatewayv1.HTTPHeaderMatch{{Name: "x-env", Value: "beta"}},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1-v1", Port: ptr(gatewayv1.PortNumber(8080))}, Weight: ptr(int32(90))}},
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1-v2"}, Weight: ptr(int32(10))}},
		},
		Timeouts: &gatewayv1.HTTPRouteTimeouts{Request: ptr(gatewayv1.Duration("5s"))},
	}
	gr := r.buildGammaRoute(rule)

	require.Len(t, gr.Matches, 1)
	assert.Equal(t, "/admin", gr.Matches[0].Exact)
	require.Len(t, gr.Matches[0].Headers, 1)
	assert.Equal(t, proxy.GammaHeaderMatch{Name: "x-env", Value: "beta"}, gr.Matches[0].Headers[0])

	require.Len(t, gr.Backends, 2)
	assert.Equal(t, proxy.GammaBackend{Service: "svc-1-v1", Cluster: proxy.ServiceClusterName("svc-1-v1", "mesh"), Weight: 90}, gr.Backends[0])
	assert.Equal(t, uint32(10), gr.Backends[1].Weight)

	require.NotNil(t, gr.Timeout)
	assert.Equal(t, int64(5), gr.Timeout.GetSeconds())
}

// TestBuildGammaRouteFromGRPC: gRPC method match -> path, weighted backendRefs.
func TestBuildGammaRouteFromGRPC(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	w := ptr(int32(7))
	rule := gatewayv1.GRPCRouteRule{
		Matches: []gatewayv1.GRPCRouteMatch{
			{Method: &gatewayv1.GRPCMethodMatch{Service: ptr("foo.Bar"), Method: ptr("Baz")}},
			{Method: &gatewayv1.GRPCMethodMatch{Service: ptr("foo.Bar")}},
		},
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-2"}, Weight: w}},
		},
	}
	gr := r.buildGammaRouteFromGRPC(rule)
	require.Len(t, gr.Matches, 2)
	assert.Equal(t, "/foo.Bar/Baz", gr.Matches[0].Exact, "service+method -> exact path")
	assert.Equal(t, "/foo.Bar/", gr.Matches[1].Prefix, "service-only -> prefix path")
	require.Len(t, gr.Backends, 1)
	assert.Equal(t, "svc-2", gr.Backends[0].Service)
	assert.Equal(t, "svc-2.aether.internal", gr.Backends[0].Cluster)
	assert.Equal(t, uint32(7), gr.Backends[0].Weight)
}
