package secret

import (
	"context"
	"fmt"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kubernetesProvider resolves a `kubernetes.io/tls` Secret (by name, in the
// watched namespace) via the Kubernetes API. Rotation is event-driven: the edge
// reconciler watches Secrets and re-resolves on change.
type kubernetesProvider struct {
	client    client.Client
	namespace string
}

// NewKubernetesProvider resolves TLS Secrets from the given namespace.
func NewKubernetesProvider(c client.Client, namespace string) Provider {
	return &kubernetesProvider{client: c, namespace: namespace}
}

func (k *kubernetesProvider) Provider() configv1.SecretProvider {
	return configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES
}

func (k *kubernetesProvider) Scheme() string { return "kubernetes" }

func (k *kubernetesProvider) Resolve(ctx context.Context, ref string) (TLSCert, error) {
	var sec corev1.Secret
	if err := k.client.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: ref}, &sec); err != nil {
		return TLSCert{}, fmt.Errorf("get TLS secret %s/%s: %w", k.namespace, ref, err)
	}
	if sec.Type != corev1.SecretTypeTLS {
		return TLSCert{}, fmt.Errorf("secret %s/%s is %q, want %q", k.namespace, ref, sec.Type, corev1.SecretTypeTLS)
	}
	cert := sec.Data[corev1.TLSCertKey]
	key := sec.Data[corev1.TLSPrivateKeyKey]
	if len(cert) == 0 || len(key) == 0 {
		return TLSCert{}, fmt.Errorf("secret %s/%s missing %s/%s", k.namespace, ref, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	return TLSCert{Cert: cert, Key: key, Version: sec.ResourceVersion}, nil
}
