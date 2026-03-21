package cloudmap

import (
	"time"

	"github.com/bpalermo/aether/constants"
)

const (
	// DefaultNamespace is the default Cloud Map HTTP namespace name.
	DefaultNamespace = constants.DefaultCloudMapNamespace

	// DefaultNamespaceTTL is the default cache TTL for namespace ID lookups.
	DefaultNamespaceTTL = 30 * time.Second

	// DefaultServiceTTL is the default cache TTL for service ID lookups.
	DefaultServiceTTL = 30 * time.Second

	// DefaultEndpointTTL is the default cache TTL for endpoint discovery results.
	DefaultEndpointTTL = 10 * time.Second
)

// Option configures a CloudMapRegistry.
type Option func(*CloudMapRegistry)

// WithNamespace sets the Cloud Map HTTP namespace name.
func WithNamespace(namespace string) Option {
	return func(r *CloudMapRegistry) {
		r.namespace = namespace
	}
}

// WithNamespaceTTL sets the cache TTL for namespace ID lookups.
func WithNamespaceTTL(ttl time.Duration) Option {
	return func(r *CloudMapRegistry) {
		r.namespaceTTL = ttl
	}
}

// WithServiceTTL sets the cache TTL for service ID lookups.
func WithServiceTTL(ttl time.Duration) Option {
	return func(r *CloudMapRegistry) {
		r.serviceTTL = ttl
	}
}

// WithEndpointTTL sets the cache TTL for endpoint discovery results.
func WithEndpointTTL(ttl time.Duration) Option {
	return func(r *CloudMapRegistry) {
		r.endpointTTL = ttl
	}
}

// WithClient overrides the default Cloud Map SDK client.
// This is primarily useful for testing with fakes.
func WithClient(client Client) Option {
	return func(r *CloudMapRegistry) {
		r.client = client
	}
}
