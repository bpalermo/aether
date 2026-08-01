// Package secret resolves edge downstream-TLS certificates from pluggable
// backends (the SecretProvider enum). The kubernetes provider is the only
// implementation today; AWS Secrets Manager / Vault plug in behind the same
// Provider interface without touching the listener/SDS path. See
// docs/proposals/003_edge-proxy.md.
package secret

import (
	"context"
	"fmt"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
)

// TLSCert is resolved certificate material. Version is a change token (e.g. the
// Secret resourceVersion) so callers can skip re-pushing unchanged certs.
type TLSCert struct {
	Cert    []byte
	Key     []byte
	Version string
}

// Provider resolves a provider-specific reference to certificate material.
type Provider interface {
	// Provider is the SecretProvider enum value this implementation serves.
	Provider() configv1.SecretProvider
	// Scheme is the stable prefix used in the Envoy SDS secret name
	// (<scheme>/<ref>), so certs from different providers coexist.
	Scheme() string
	// Resolve fetches the current cert material for the provider-specific ref.
	Resolve(ctx context.Context, ref string) (TLSCert, error)
}

// Registry dispatches by SecretProvider enum to a registered Provider.
type Registry struct {
	byProvider map[configv1.SecretProvider]Provider
}

// NewRegistry registers the given providers, keyed by their enum value.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byProvider: make(map[configv1.SecretProvider]Provider, len(providers))}
	for _, p := range providers {
		r.byProvider[p.Provider()] = p
	}
	return r
}

// normalize maps the unspecified default to the kubernetes provider.
func normalize(p configv1.SecretProvider) configv1.SecretProvider {
	if p == configv1.SecretProvider_SECRET_PROVIDER_UNSPECIFIED {
		return configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES
	}
	return p
}

func (r *Registry) provider(p configv1.SecretProvider) (Provider, error) {
	prov, ok := r.byProvider[normalize(p)]
	if !ok {
		return nil, fmt.Errorf("no secret provider registered for %s", normalize(p))
	}
	return prov, nil
}

// Resolve fetches the cert material for (provider, ref).
func (r *Registry) Resolve(ctx context.Context, p configv1.SecretProvider, ref string) (TLSCert, error) {
	prov, err := r.provider(p)
	if err != nil {
		return TLSCert{}, err
	}
	return prov.Resolve(ctx, ref)
}

// SDSName returns the Envoy SDS secret name for (provider, ref):
// <provider scheme>/<ref>. The listener and the cache key on this name.
func (r *Registry) SDSName(p configv1.SecretProvider, ref string) (string, error) {
	prov, err := r.provider(p)
	if err != nil {
		return "", err
	}
	return prov.Scheme() + "/" + ref, nil
}
