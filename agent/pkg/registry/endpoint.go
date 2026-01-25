package registry

type Endpoint interface {
	GetClusterName() string
	GetIp() string
	GetAdditionalMetadata() map[string]string
	GetRegion() string
	GetSubzone() string
}

type EndpointProtocol string

const (
	EndpointProtocolHTTP EndpointProtocol = "http"
)
