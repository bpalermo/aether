package registry

import (
	"context"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

type Registry interface {
	Start(ctx context.Context) error
	RegisterEndpoint(ctx context.Context, serviceName string, protocol string, endpoint *registryv1.ServiceEndpoint) error
	UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error
	UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error
	ListEndpoints(ctx context.Context, service string, protocol string) ([]*registryv1.ServiceEndpoint, error)
	ListAllEndpoints(ctx context.Context, protocol string) (map[string][]*registryv1.ServiceEndpoint, error)
}
