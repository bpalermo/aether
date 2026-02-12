package registry

import (
	"fmt"
	"strconv"

	"github.com/bpalermo/aether/constants"
	"github.com/bpalermo/aether/registry/types"
)

type KubernetesEndpoint struct {
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

var _ types.Endpoint = (*KubernetesEndpoint)(nil)

func NewKubernetesEndpoint(clusterName string, annotations map[string]string, labels map[string]string, ip string, nodeLabels map[string]string) (*KubernetesEndpoint, error) {
	serviceName, err := getServiceNameFromLabels(labels)
	if err != nil {
		return nil, fmt.Errorf("missing service label")
	}

	port, err := getPortFromAnnotations(annotations)
	if err != nil {
		return nil, err
	}

	weight, err := getWeightFromAnnotations(annotations)
	if err != nil {
		return nil, err
	}

	return &KubernetesEndpoint{
		clusterName:  clusterName,
		serviceName:  serviceName,
		region:       nodeLabels[constants.LabelKubernetesTopologyRegion],
		subzone:      nodeLabels[constants.LabelKubernetesTopologyZone],
		ip:           ip,
		metadata:     getEndpointMetadataFromAnnotations(annotations),
		port:         port,
		portProtocol: types.EndpointProtocolHTTP,
		weight:       weight,
	}, nil
}

func (ke *KubernetesEndpoint) GetClusterName() string {
	return ke.clusterName
}

func (ke *KubernetesEndpoint) GetServiceName() string {
	return ke.serviceName
}

func (ke *KubernetesEndpoint) GetIp() string {
	return ke.ip
}

func (ke *KubernetesEndpoint) GetAdditionalMetadata() map[string]string {
	return ke.metadata
}

func (ke *KubernetesEndpoint) GetRegion() string {
	return ke.region
}

func (ke *KubernetesEndpoint) GetSubzone() string {
	return ke.subzone
}

func (ke *KubernetesEndpoint) GetPort() uint16 {
	return ke.port
}

func (ke *KubernetesEndpoint) GetPortProtocol() types.EndpointProtocol {
	return ke.portProtocol
}

func (ke *KubernetesEndpoint) GetWeight() uint32 {
	return ke.weight
}

func getServiceNameFromLabels(labels map[string]string) (string, error) {
	serviceName, ok := labels[constants.LabelAetherService]
	if !ok {
		return "", fmt.Errorf("missing service label")
	}
	return serviceName, nil
}

func getPortFromAnnotations(annotations map[string]string) (uint16, error) {
	s, ok := annotations[constants.AnnotationEndpointPort]
	if !ok {
		return constants.DefaultEndpointPort, nil
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port annotation")
	}

	return uint16(port), nil
}

func getWeightFromAnnotations(annotations map[string]string) (uint32, error) {
	s, ok := annotations[constants.AnnotationEndpointWeight]
	if !ok {
		return constants.DefaultEndpointWeight, nil
	}

	weight, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid weight annotation")
	}

	return uint32(weight), nil
}

func getEndpointMetadataFromAnnotations(annotations map[string]string) map[string]string {
	metadata := map[string]string{}
	prefix := constants.AnnotationAetherEndpointMetadataPrefix
	for key, value := range annotations {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			metadataKey := key[len(prefix):]
			metadata[metadataKey] = value
		}
	}
	return metadata
}
