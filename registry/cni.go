package registry

import (
	"fmt"
	"strconv"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
)

func NewServiceEndpointFromCNIPod(clusterName string, nodeRegion string, nodeZone string, cniPod *cniv1.CNIPod) (string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint, error) {
	protocol := registryv1.Service_HTTP

	serviceName, err := getServiceNameFromLabels(cniPod.GetLabels())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	port, err := getPortFromAnnotations(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	weight, err := getWeightFromAnnotations(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	endpoint := &registryv1.ServiceEndpoint{
		Ip:          cniPod.GetIps()[0],
		ClusterName: clusterName,
		Port:        uint32(port),
		Weight:      weight,
		Metadata:    getEndpointMetadataFromAnnotations(cniPod.GetAnnotations()),
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      cniPod.GetContainerId(),
			NetworkNamespace: cniPod.GetNetworkNamespace(),
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: cniPod.GetNamespace(),
			PodName:   cniPod.GetName(),
		},
		Locality: &registryv1.ServiceEndpoint_Locality{
			Region: nodeRegion,
			Zone:   nodeZone,
		},
	}

	return serviceName, protocol, endpoint, nil
}

func ExtractCNIPodInformation(pod *cniv1.CNIPod) (string, []string, error) {
	serviceName, err := getServiceNameFromLabels(pod.GetLabels())
	if err != nil {
		return "", nil, err
	}

	return serviceName, pod.GetIps(), nil
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
