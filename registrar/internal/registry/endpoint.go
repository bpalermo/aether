package registry

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
	defaultEndpointPort   uint16 = 8080
	defaultEndpointWeight uint32 = 1024

	EndpointProtocolHTTP EndpointProtocol = "http"
)
