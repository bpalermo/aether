package secret

import (
	"context"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func tlsSecret(ns, name, cert, key string) *corev1.Secret {
	s := &corev1.Secret{
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte(cert), corev1.TLSPrivateKeyKey: []byte(key)},
	}
	s.Name = name
	s.Namespace = ns
	s.ResourceVersion = "7"
	return s
}

func newFakeClient(objs ...*corev1.Secret) client.WithWatch {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b.Build()
}

func TestKubernetesProviderResolve(t *testing.T) {
	cl := newFakeClient(tlsSecret("edge", "api-tls", "CERT", "KEY"))
	p := NewKubernetesProvider(cl, "edge")

	assert.Equal(t, configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, p.Provider())
	assert.Equal(t, "kubernetes", p.Scheme())

	got, err := p.Resolve(context.Background(), "api-tls")
	require.NoError(t, err)
	assert.Equal(t, []byte("CERT"), got.Cert)
	assert.Equal(t, []byte("KEY"), got.Key)
	assert.Equal(t, "7", got.Version)
}

func TestKubernetesProviderResolveNamespacedRef(t *testing.T) {
	// A "<ns>/<name>" ref reads from that namespace, NOT the provider's default —
	// so a Gateway certificateRef resolves from the Gateway's own namespace, not
	// the edge's. (Regression: the edge read all certs from its own namespace.)
	cl := newFakeClient(tlsSecret("other-ns", "their-tls", "OTHERCERT", "OTHERKEY"))
	p := NewKubernetesProvider(cl, "edge") // default namespace is "edge"

	got, err := p.Resolve(context.Background(), "other-ns/their-tls")
	require.NoError(t, err)
	assert.Equal(t, []byte("OTHERCERT"), got.Cert)

	// A bare name still resolves in the default namespace (no Secret there → error).
	_, err = p.Resolve(context.Background(), "their-tls")
	require.Error(t, err, "bare name looks in the default namespace, not other-ns")
}

func TestKubernetesProviderResolveErrors(t *testing.T) {
	opaque := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"x": []byte("y")}}
	opaque.Name, opaque.Namespace = "opaque", "edge"
	cl := newFakeClient(opaque)
	p := NewKubernetesProvider(cl, "edge")

	_, err := p.Resolve(context.Background(), "absent")
	require.Error(t, err, "missing secret")

	_, err = p.Resolve(context.Background(), "opaque")
	require.Error(t, err, "non-tls secret type")
}

func TestRegistryDispatchAndSDSName(t *testing.T) {
	cl := newFakeClient(tlsSecret("edge", "api-tls", "C", "K"))
	r := NewRegistry(NewKubernetesProvider(cl, "edge"))

	// UNSPECIFIED defaults to kubernetes.
	name, err := r.SDSName(configv1.SecretProvider_SECRET_PROVIDER_UNSPECIFIED, "api-tls")
	require.NoError(t, err)
	assert.Equal(t, "kubernetes/api-tls", name)

	got, err := r.Resolve(context.Background(), configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, "api-tls")
	require.NoError(t, err)
	assert.Equal(t, []byte("C"), got.Cert)

	// An enum value with no registered impl errors (not panics).
	_, err = r.Resolve(context.Background(), configv1.SecretProvider_SECRET_PROVIDER_AWS_SECRETS_MANAGER, "x")
	require.Error(t, err)
	_, err = r.SDSName(configv1.SecretProvider_SECRET_PROVIDER_AWS_SECRETS_MANAGER, "x")
	require.Error(t, err)
}
