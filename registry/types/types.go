package types

import "context"

type Registry interface {
	Start(ctx context.Context) error
	RegisterEndpoint(ctx context.Context, endpoint Endpoint) error
	UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error
	ListEndpoints(ctx context.Context, service string, protocol string) ([]Endpoint, error)
}

type Endpoint interface {
	GetClusterName() string
	GetServiceName() string
	GetIp() string
	GetAdditionalMetadata() map[string]string
	GetRegion() string
	GetSubzone() string
	GetPort() uint16
	GetPortProtocol() EndpointProtocol
	GetWeight() uint32
}

type EndpointProtocol string

const (
	EndpointProtocolHTTP EndpointProtocol = "http"
)
