package secret

import (
	"context"
	"fmt"
	"strings"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kubernetesProvider resolves a `kubernetes.io/tls` Secret via the Kubernetes API.
// The ref is either a bare Secret name (resolved in the provider's default
// namespace) or a namespaced "<namespace>/<name>" — the Gateway listener path uses
// the latter so a certificateRef is read from ITS namespace (the Gateway's own, or
// certificateRef.namespace), not the edge's namespace. Rotation is event-driven:
// the edge reconciler watches Secrets and re-resolves on change.
type kubernetesProvider struct {
	client    client.Client
	namespace string
}

// NewKubernetesProvider resolves TLS Secrets, defaulting bare-name refs to the
// given namespace.
func NewKubernetesProvider(c client.Client, namespace string) Provider {
	return &kubernetesProvider{client: c, namespace: namespace}
}

func (k *kubernetesProvider) Provider() configv1.SecretProvider {
	return configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES
}

func (k *kubernetesProvider) Scheme() string { return "kubernetes" }

// splitRef splits a "<namespace>/<name>" ref into its parts, defaulting the
// namespace to the provider's when the ref is a bare name.
func (k *kubernetesProvider) splitRef(ref string) (namespace, name string) {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return k.namespace, ref
}

func (k *kubernetesProvider) Resolve(ctx context.Context, ref string) (TLSCert, error) {
	ns, name := k.splitRef(ref)
	var sec corev1.Secret
	if err := k.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		return TLSCert{}, fmt.Errorf("get TLS secret %s/%s: %w", ns, name, err)
	}
	if sec.Type != corev1.SecretTypeTLS {
		return TLSCert{}, fmt.Errorf("secret %s/%s is %q, want %q", ns, name, sec.Type, corev1.SecretTypeTLS)
	}
	cert := sec.Data[corev1.TLSCertKey]
	key := sec.Data[corev1.TLSPrivateKeyKey]
	if len(cert) == 0 || len(key) == 0 {
		return TLSCert{}, fmt.Errorf("secret %s/%s missing %s/%s", ns, name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	return TLSCert{Cert: cert, Key: key, Version: sec.ResourceVersion}, nil
}
