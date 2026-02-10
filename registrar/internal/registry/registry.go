package registry

import (
	"context"
)

type Registry interface {
	Start(ctx context.Context) error
	RegisterEndpoint(ctx context.Context, endpoint Endpoint) error
	UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error
	ListEndpoints(ctx context.Context, service string, protocol string) ([]Endpoint, error)
}
