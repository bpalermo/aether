package registry

import (
	"fmt"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry/types"
)

type CNIEndpoint struct {
	clusterName  string
	serviceName  string
	region       string
	subzone      string
	ip           string
	port         uint16
	portProtocol types.EndpointProtocol
	metadata     map[string]string
	weight       uint32
}

func NewRegistryPodEndpoint(clusterName string, pod *registryv1.RegistryPod) (*CNIEndpoint, error) {
	serviceName, err := getServiceNameFromLabels(pod.Labels)
	if err != nil {
		return nil, fmt.Errorf("missing service label")
	}

	port, err := getPortFromAnnotations(pod.Annotations)
	if err != nil {
		return nil, err
	}

	weight, err := getWeightFromAnnotations(pod.Annotations)
	if err != nil {
		return nil, err
	}

	return &CNIEndpoint{
		clusterName:  clusterName,
		serviceName:  serviceName,
		ip:           pod.CniPod.Ips[0],
		port:         port,
		portProtocol: types.EndpointProtocolHTTP,
		metadata:     getEndpointMetadataFromAnnotations(pod.Annotations),
		weight:       weight,
	}, nil
}

func (ke *CNIEndpoint) GetClusterName() string {
	return ke.clusterName
}

func (ke *CNIEndpoint) GetServiceName() string {
	return ke.serviceName
}

func (ke *CNIEndpoint) GetIp() string {
	return ke.ip
}

func (ke *CNIEndpoint) GetAdditionalMetadata() map[string]string {
	return ke.metadata
}

func (ke *CNIEndpoint) GetRegion() string {
	return ke.region
}

func (ke *CNIEndpoint) GetSubzone() string {
	return ke.subzone
}

func (ke *CNIEndpoint) GetPort() uint16 {
	return ke.port
}

func (ke *CNIEndpoint) GetPortProtocol() types.EndpointProtocol {
	return ke.portProtocol
}

func (ke *CNIEndpoint) GetWeight() uint32 {
	return ke.weight
}
