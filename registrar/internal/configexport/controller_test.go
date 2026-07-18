package configexport

import (
	"context"
	"log/slog"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// fakeExporter records SetConfig/UnsetConfig and serves a seeded ListConfig.
type fakeExporter struct {
	store map[string]*registryv1.ServiceConfigProjection
	unset []string
}

func (f *fakeExporter) SetConfig(_ context.Context, p *registryv1.ServiceConfigProjection) error {
	p.OriginCluster = "cluster-a"
	f.store[p.GetService()] = p
	return nil
}

func (f *fakeExporter) UnsetConfig(_ context.Context, service string) error {
	f.unset = append(f.unset, service)
	delete(f.store, service)
	return nil
}

func (f *fakeExporter) ListConfig(context.Context) ([]*registryv1.ServiceConfigProjection, error) {
	out := make([]*registryv1.ServiceConfigProjection, 0, len(f.store))
	for _, p := range f.store {
		out = append(out, p)
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

func httpRouteToSvc(name, ns, svc string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Service")), Name: gatewayv1.ObjectName(svc)},
			}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Value: ptr("/")}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(svc + "-v1")},
				}}},
			}},
		},
	}
}

func newController(t *testing.T, exp *fakeExporter, objs ...client.Object) *Controller {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, gatewayv1beta1.Install(s))
	require.NoError(t, mcsv1alpha1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Controller{Client: c, Exporter: exp, MeshDomain: "aether.internal", Cluster: "cluster-a", Log: slog.New(slog.DiscardHandler)}
}

// TestReconcile_ExportsOnlyExportedServices verifies the controller writes the GAMMA
// config of route targets that have a ServiceExport, and ignores un-exported ones.
func TestReconcile_ExportsOnlyExportedServices(t *testing.T) {
	exp := &fakeExporter{store: map[string]*registryv1.ServiceConfigProjection{}}
	c := newController(
		t, exp,
		httpRouteToSvc("r-echo", "team-a", "echo"),       // exported
		httpRouteToSvc("r-private", "team-a", "private"), // NOT exported
		&mcsv1alpha1.ServiceExport{ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"}},
	)
	_, err := c.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	require.Contains(t, exp.store, "team-a/echo")
	assert.NotContains(t, exp.store, "team-a/private", "un-exported route target is not exported")
	got := exp.store["team-a/echo"]
	require.Len(t, got.GetRoutes(), 1)
	assert.Equal(t, "echo-v1.team-a.aether.internal", got.GetRoutes()[0].GetBackends()[0].GetCluster())
}

// TestReconcile_UnexportsRemoved verifies a previously-exported service this cluster
// authored is UnsetConfig'd once it is no longer exported.
func TestReconcile_UnexportsRemoved(t *testing.T) {
	exp := &fakeExporter{store: map[string]*registryv1.ServiceConfigProjection{
		"team-a/stale": {Service: "team-a/stale", OriginCluster: "cluster-a", Version: "old"},
		"team-b/peer":  {Service: "team-b/peer", OriginCluster: "cluster-b", Version: "x"}, // peer's — must not touch
	}}
	c := newController(t, exp) // no routes/exports → nothing desired
	_, err := c.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	assert.Equal(t, []string{"team-a/stale"}, exp.unset, "only this cluster's now-undesired export is removed")
	assert.Contains(t, exp.store, "team-b/peer", "a peer cluster's projection is never touched")
}
