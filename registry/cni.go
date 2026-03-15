package registry

import (
	"fmt"
	"strconv"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
)

// NewServiceEndpointFromCNIPod creates a ServiceEndpoint from a CNIPod.
// It extracts the service name from the pod's labels and the port and weight from annotations.
// The service protocol is always HTTP. Container and Kubernetes metadata are included
// along with node locality information.
func NewServiceEndpointFromCNIPod(clusterName string, nodeName string, nodeRegion string, nodeZone string, cniPod *cniv1.CNIPod) (string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint, error) {
	protocol := registryv1.Service_HTTP

	serviceName, err := getServiceName(cniPod)
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
			NodeName:  nodeName,
		},
		Locality: &registryv1.ServiceEndpoint_Locality{
			Region: nodeRegion,
			Zone:   nodeZone,
		},
	}

	return serviceName, protocol, endpoint, nil
}

// ExtractCNIPodInformation extracts the service name and IP addresses from a CNIPod.
func ExtractCNIPodInformation(pod *cniv1.CNIPod) (string, []string, error) {
	serviceName, err := getServiceName(pod)
	if err != nil {
		return "", nil, err
	}

	return serviceName, pod.GetIps(), nil
}

// getServiceName extracts the service name from the pod's service account.
func getServiceName(cniPod *cniv1.CNIPod) (string, error) {
	sa := cniPod.GetServiceAccount()
	if sa == "" {
		return "", fmt.Errorf("missing service account")
	}
	return sa, nil
}

// getPortFromAnnotations extracts the endpoint port from pod annotations.
// If the port annotation is not present, it returns the default endpoint port.
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

// getWeightFromAnnotations extracts the endpoint weight from pod annotations.
// If the weight annotation is not present, it returns the default endpoint weight.
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

// getEndpointMetadataFromAnnotations extracts endpoint metadata from pod annotations.
// All annotations with the aether endpoint metadata prefix are included in the result.
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
