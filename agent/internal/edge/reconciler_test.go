package edge

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type fakeSink struct {
	routes []cache.EdgeRoute
	certs  map[string]cache.EdgeTLSCert
}

func (f *fakeSink) SetEdgeRoutes(routes []cache.EdgeRoute) { f.routes = routes }

func (f *fakeSink) SetEdgeTLSSecrets(_ context.Context, certs map[string]cache.EdgeTLSCert) error {
	f.certs = certs
	return nil
}

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

// TestReconcileResolvesTLS verifies a route's referenced kubernetes.io/tls Secret
// is resolved to a provider-prefixed SDS name + cert bytes, and an unresolvable
// reference leaves the route without a cert (not an error).
func TestReconcileResolvesTLS(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	withTLS := edgeRoute("api", "aether-edge", "svc-1", 0, "api.example.com")
	withTLS.Spec.SetTls(&configv1.EdgeRouteTLS{})
	withTLS.Spec.GetTls().SetSecretName("api-tls")

	missing := edgeRoute("foo", "aether-edge", "svc-2", 0, "foo.example.com")
	missing.Spec.SetTls(&configv1.EdgeRouteTLS{})
	missing.Spec.GetTls().SetSecretName("absent-tls")

	tlsSecret := &corev1.Secret{
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte("CERT"), corev1.TLSPrivateKeyKey: []byte("KEY")},
	}
	tlsSecret.Name = "api-tls"
	tlsSecret.Namespace = "aether-edge"

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(withTLS, missing, tlsSecret).Build()
	sink := &fakeSink{}
	r := &Reconciler{
		Client:    cl,
		Sink:      sink,
		Namespace: "aether-edge",
		Secrets:   secret.NewRegistry(secret.NewKubernetesProvider(cl, "aether-edge")),
		Log:       slog.New(slog.DiscardHandler),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := map[string]cache.EdgeRoute{}
	for _, rt := range sink.routes {
		got[rt.Service] = rt
	}
	assert.Equal(t, "kubernetes/api-tls", got["svc-1"].TLSSecret)
	assert.Empty(t, got["svc-2"].TLSSecret, "unresolvable cert leaves the route without one")

	require.Contains(t, sink.certs, "kubernetes/api-tls")
	assert.Equal(t, []byte("CERT"), sink.certs["kubernetes/api-tls"].Cert)
	assert.Equal(t, []byte("KEY"), sink.certs["kubernetes/api-tls"].Key)
	assert.NotContains(t, sink.certs, "kubernetes/absent-tls")
}
