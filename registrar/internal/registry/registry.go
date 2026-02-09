package registry

import (
	"context"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

type Registry interface {
	Start(ctx context.Context) error
	RegisterEndpoint(ctx context.Context, pod *registryv1.RegistryPod) error
	UnregisterEndpoints(ctx context.Context, pod *registryv1.RegistryPod) error
	ListEndpoints(ctx context.Context, service string, protocol string) ([]Endpoint, error)
}
