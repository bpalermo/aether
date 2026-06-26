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

// validTLSCert / validTLSKey are a real, self-signed ECDSA keypair (CN=test). The
// provider validates the keypair with tls.X509KeyPair, so fixtures must hold
// parseable material, not placeholder bytes.
const (
	validTLSCert = `-----BEGIN CERTIFICATE-----
MIIBHzCBxqADAgECAgEBMAoGCCqGSM49BAMCMA8xDTALBgNVBAMTBHRlc3QwHhcN
MjYwNjI2MTU1MzUwWhcNMjcwNjI2MTY1MzUwWjAPMQ0wCwYDVQQDEwR0ZXN0MFkw
EwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEn4alJ26H8ozmsdz28/d8BUxOcmTuHYiT
IdRPGfJR6P6CHstygFvJD1N7mvblUBSGyaJq8jI9p0eIFI+km0F5k6MTMBEwDwYD
VR0RBAgwBoIEdGVzdDAKBggqhkjOPQQDAgNIADBFAiEA8t876pZRDOkFPAoZV0gg
36yLul1OYTpOylOe9NsAyHwCIE7SBys13i6g5re4YmKgv1GBo6wW3VhxZWO6qm/Y
KTqF
-----END CERTIFICATE-----
`
	validTLSKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIIsvYanqaRRY782l1DBVdbGmnaBSmptup19WIFXx2vdjoAoGCCqGSM49
AwEHoUQDQgAEn4alJ26H8ozmsdz28/d8BUxOcmTuHYiTIdRPGfJR6P6CHstygFvJ
D1N7mvblUBSGyaJq8jI9p0eIFI+km0F5kw==
-----END EC PRIVATE KEY-----
`
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
	cl := newFakeClient(tlsSecret("edge", "api-tls", validTLSCert, validTLSKey))
	p := NewKubernetesProvider(cl, "edge")

	assert.Equal(t, configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, p.Provider())
	assert.Equal(t, "kubernetes", p.Scheme())

	got, err := p.Resolve(context.Background(), "api-tls")
	require.NoError(t, err)
	assert.Equal(t, []byte(validTLSCert), got.Cert)
	assert.Equal(t, []byte(validTLSKey), got.Key)
	assert.Equal(t, "7", got.Version)
}

func TestKubernetesProviderResolveNamespacedRef(t *testing.T) {
	// A "<ns>/<name>" ref reads from that namespace, NOT the provider's default —
	// so a Gateway certificateRef resolves from the Gateway's own namespace, not
	// the edge's. (Regression: the edge read all certs from its own namespace.)
	cl := newFakeClient(tlsSecret("other-ns", "their-tls", validTLSCert, validTLSKey))
	p := NewKubernetesProvider(cl, "edge") // default namespace is "edge"

	got, err := p.Resolve(context.Background(), "other-ns/their-tls")
	require.NoError(t, err)
	assert.Equal(t, []byte(validTLSCert), got.Cert)

	// A bare name still resolves in the default namespace (no Secret there → error).
	_, err = p.Resolve(context.Background(), "their-tls")
	require.Error(t, err, "bare name looks in the default namespace, not other-ns")
}

func TestKubernetesProviderResolveErrors(t *testing.T) {
	opaque := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"x": []byte("y")}}
	opaque.Name, opaque.Namespace = "opaque", "edge"

	// Right type, both keys present, but no tls.key (empty) — missing material.
	noKey := tlsSecret("edge", "no-key", validTLSCert, "")

	// Right type, both keys non-empty, but the bytes are NOT a valid keypair —
	// this is the Gateway API conformance "malformed-certificate" case
	// (base64("Hello world") in both tls.crt and tls.key). The provider must
	// reject it via tls.X509KeyPair so the listener reports InvalidCertificateRef.
	malformed := tlsSecret("edge", "malformed", "Hello world\n", "Hello world\n")

	// Right type, non-empty bytes, but tls.crt is a valid cert while tls.key is
	// junk PEM — the keypair can't be assembled (a cert/key mismatch case).
	badKey := tlsSecret("edge", "bad-key", validTLSCert, "-----BEGIN EC PRIVATE KEY-----\nbm90LWEta2V5\n-----END EC PRIVATE KEY-----\n")

	cl := newFakeClient(opaque, noKey, malformed, badKey)
	p := NewKubernetesProvider(cl, "edge")

	_, err := p.Resolve(context.Background(), "absent")
	require.Error(t, err, "missing secret")

	_, err = p.Resolve(context.Background(), "opaque")
	require.Error(t, err, "non-tls secret type")

	_, err = p.Resolve(context.Background(), "no-key")
	require.Error(t, err, "secret missing tls.key")

	_, err = p.Resolve(context.Background(), "malformed")
	require.Error(t, err, "malformed cert/key bytes are not a valid keypair")

	_, err = p.Resolve(context.Background(), "bad-key")
	require.Error(t, err, "unparseable private key is not a valid keypair")
}

func TestRegistryDispatchAndSDSName(t *testing.T) {
	cl := newFakeClient(tlsSecret("edge", "api-tls", validTLSCert, validTLSKey))
	r := NewRegistry(NewKubernetesProvider(cl, "edge"))

	// UNSPECIFIED defaults to kubernetes.
	name, err := r.SDSName(configv1.SecretProvider_SECRET_PROVIDER_UNSPECIFIED, "api-tls")
	require.NoError(t, err)
	assert.Equal(t, "kubernetes/api-tls", name)

	got, err := r.Resolve(context.Background(), configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, "api-tls")
	require.NoError(t, err)
	assert.Equal(t, []byte(validTLSCert), got.Cert)

	// An enum value with no registered impl errors (not panics).
	_, err = r.Resolve(context.Background(), configv1.SecretProvider_SECRET_PROVIDER_AWS_SECRETS_MANAGER, "x")
	require.Error(t, err)
	_, err = r.SDSName(configv1.SecretProvider_SECRET_PROVIDER_AWS_SECRETS_MANAGER, "x")
	require.Error(t, err)
}
