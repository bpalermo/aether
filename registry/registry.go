// Package registry provides interfaces for service endpoint registration and discovery.
// It manages the lifecycle of service endpoints and allows querying available services
// and their endpoints. Implementations can use different backends (e.g., DynamoDB).
package registry

import (
	"context"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// Registry manages service endpoint registration, deregistration, and discovery.
// It maintains a list of service endpoints and supports querying by service name and protocol.
type Registry interface {
	// Start initializes the registry. This should be called before using other methods.
	Start(ctx context.Context) error
	// RegisterEndpoint registers a single endpoint for a service with a specific protocol.
	RegisterEndpoint(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint) error
	// UnregisterEndpoint unregisters an endpoint for a service by its IP address.
	UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error
	// UnregisterEndpoints unregisters multiple endpoints for a service by their IP addresses.
	UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error
	// ListEndpoints returns all endpoints for a service with a specific protocol.
	ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error)
	// ListAllEndpoints returns all endpoints for all services of a specific protocol,
	// organized in a map keyed by service name.
	ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
}
