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
	vhosts []cache.VirtualHost
	certs  map[string]cache.EdgeTLSCert
}

func (f *fakeSink) SetVirtualHosts(vhosts []cache.VirtualHost) { f.vhosts = vhosts }

func (f *fakeSink) SetEdgeTLSSecrets(_ context.Context, certs map[string]cache.EdgeTLSCert) error {
	f.certs = certs
	return nil
}

// prefixRoute builds an HTTPRoute matching a path prefix and forwarding to a
// (service, port) backend.
func prefixRoute(prefix, service string, port uint32) *configv1.HTTPRoute {
	m := &configv1.RouteMatch{}
	m.SetPrefix(prefix)
	b := &configv1.RouteBackend{}
	b.SetService(service)
	b.SetPort(port)
	hr := &configv1.HTTPRoute{}
	hr.SetMatch(m)
	hr.SetBackend(b)
	return hr
}

// virtualHost builds a VirtualHost CR with the given hosts and routes.
func virtualHost(name, ns string, hosts []string, routes ...*configv1.HTTPRoute) *crdv1.VirtualHost {
	vh := &crdv1.VirtualHost{}
	vh.Name = name
	vh.Namespace = ns
	spec := &configv1.VirtualHostSpec{}
	if len(hosts) > 0 {
		spec.SetHosts(hosts)
	}
	if len(routes) > 0 {
		spec.SetRoutes(routes)
	}
	vh.Spec = spec
	return vh
}

func TestReconcileProjectsVirtualHosts(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		virtualHost("api", "aether-edge", []string{"api.example.com"},
			prefixRoute("/users", "svc-1", 0), prefixRoute("/", "svc-web", 0)),
		virtualHost("grpc", "aether-edge", []string{"grpc.example.com"},
			prefixRoute("/", "svc-2", 9090)),
		virtualHost("elsewhere", "other-ns", []string{"x.example.com"},
			prefixRoute("/", "svc-9", 0)), // different namespace, excluded
	).Build()

	sink := &fakeSink{}
	r := &Reconciler{Client: cl, Sink: sink, Namespace: "aether-edge", Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	// Both in-namespace VirtualHosts; the other-namespace one is excluded. Sorted
	// by name: "api", "grpc".
	require.Len(t, sink.vhosts, 2)
	assert.Equal(t, []string{"api.example.com"}, sink.vhosts[0].Hosts)
	require.Len(t, sink.vhosts[0].Routes, 2)
	assert.Equal(t, "/users", sink.vhosts[0].Routes[0].Prefix)
	assert.Equal(t, "svc-1", sink.vhosts[0].Routes[0].Service)
	assert.Equal(t, "svc-web", sink.vhosts[0].Routes[1].Service)

	assert.Equal(t, []string{"grpc.example.com"}, sink.vhosts[1].Hosts)
	assert.Equal(t, uint32(9090), sink.vhosts[1].Routes[0].Port)
}

// TestReconcileSkipsInert verifies virtual hosts that expose nothing are skipped:
// no external host, or no routable route.
func TestReconcileSkipsInert(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))

	noBackend := prefixRoute("/", "", 0) // route with no backend service
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		virtualHost("no-hosts", "aether-edge", nil, prefixRoute("/", "svc-1", 0)),     // routes but no hosts
		virtualHost("no-routes", "aether-edge", []string{"x.example.com"}, noBackend), // hosts but no routable route
	).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: cl, Sink: sink, Namespace: "aether-edge", Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Empty(t, sink.vhosts)
}

// TestReconcileResolvesTLS verifies a virtual host's referenced kubernetes.io/tls
// Secret is resolved to a provider-prefixed SDS name + cert bytes, and an
// unresolvable reference leaves the host without a cert (not an error).
func TestReconcileResolvesTLS(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	withTLS := virtualHost("api", "aether-edge", []string{"api.example.com"}, prefixRoute("/", "svc-1", 0))
	withTLS.Spec.SetTls(&configv1.VirtualHostTLS{})
	withTLS.Spec.GetTls().SetSecretName("api-tls")

	missing := virtualHost("foo", "aether-edge", []string{"foo.example.com"}, prefixRoute("/", "svc-2", 0))
	missing.Spec.SetTls(&configv1.VirtualHostTLS{})
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

	// Sorted by name: "api" (resolvable), "foo" (unresolvable).
	require.Len(t, sink.vhosts, 2)
	assert.Equal(t, "kubernetes/api-tls", sink.vhosts[0].TLSSecret)
	assert.Empty(t, sink.vhosts[1].TLSSecret, "unresolvable cert leaves the host without one")

	require.Contains(t, sink.certs, "kubernetes/api-tls")
	assert.Equal(t, []byte("CERT"), sink.certs["kubernetes/api-tls"].Cert)
	assert.Equal(t, []byte("KEY"), sink.certs["kubernetes/api-tls"].Key)
	assert.NotContains(t, sink.certs, "kubernetes/absent-tls")
}
