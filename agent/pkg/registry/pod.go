package registry

import registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"

func ProcessPodAnnotations(cluster string, annotations map[string]string, pod *registryv1.RegistryPod) error {

	pod.ServiceName = getServiceFromAnnotations(annotations)
	pod.ClusterName = cluster
	pod.PodLocality = &registryv1.RegistryPod_Locality{
		Region: getEndpointRegionOrDefault(annotations),
		Zone:   getEndpointZoneOrDefault(annotations),
	}

	pod.PortProtocol = registryv1.RegistryPod_HTTP
	pod.ServicePort = &registryv1.RegistryPod_ServicePort{
		PortSpecifier: &registryv1.RegistryPod_ServicePort_PortNumber{
			PortNumber: getEndpointPortOrDefault(annotations),
		},
	}
	pod.EndpointWeight = getEndpointWeightOrDefault(annotations)

	pod.AdditionalMetadata = getEndpointMetadata(annotations)

	return nil
}
