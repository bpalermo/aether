package edge

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type fakeSink struct {
	routes []cache.EdgeRoute
}

func (f *fakeSink) SetEdgeRoutes(routes []cache.EdgeRoute) { f.routes = routes }

func edgeRoute(name, ns, service string, port uint32, hosts ...string) *crdv1.EdgeRoute {
	er := &crdv1.EdgeRoute{}
	er.Name = name
	er.Namespace = ns
	er.Spec = &configv1.EdgeRouteSpec{}
	er.Spec.SetService(service)
	er.Spec.SetPort(port)
	if len(hosts) > 0 {
		er.Spec.SetHosts(hosts)
	}
	return er
}

func TestReconcileProjectsRoutes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		edgeRoute("api", "aether-edge", "svc-1", 0, "api.example.com"),
		edgeRoute("grpc", "aether-edge", "svc-2", 9090, "grpc.example.com"),
		edgeRoute("elsewhere", "other-ns", "svc-9", 0, "x.example.com"), // different namespace, excluded
	).Build()

	sink := &fakeSink{}
	r := &Reconciler{
		Client:    cl,
		Sink:      sink,
		Namespace: "aether-edge",
		Log:       slog.New(slog.DiscardHandler),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	// Both in-namespace EdgeRoutes; the other-namespace one is excluded.
	require.Len(t, sink.routes, 2)
	got := map[string]cache.EdgeRoute{}
	for _, rt := range sink.routes {
		got[rt.Service] = rt
	}
	assert.Equal(t, []string{"api.example.com"}, got["svc-1"].Hosts)
	assert.Equal(t, uint32(9090), got["svc-2"].Port)
	assert.Equal(t, []string{"grpc.example.com"}, got["svc-2"].Hosts)
	assert.NotContains(t, got, "svc-9")
}

// TestReconcileSkipsInertRoutes verifies routes that expose nothing are skipped:
// no service, or no external host (the edge never routes a service at its mesh
// FQDN).
func TestReconcileSkipsInertRoutes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))

	noService := &crdv1.EdgeRoute{}
	noService.Name = "no-service"
	noService.Namespace = "aether-edge"
	noService.Spec = &configv1.EdgeRouteSpec{}
	noService.Spec.SetHosts([]string{"x.example.com"})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		noService,
		edgeRoute("no-hosts", "aether-edge", "svc-1", 0), // service but no hosts
	).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: cl, Sink: sink, Namespace: "aether-edge", Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Empty(t, sink.routes)
}
