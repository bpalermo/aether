package registry

import (
	"fmt"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/serviceref"
	"aethermesh.dev/registry/endpointmeta"
)

// NewServiceEndpointFromCNIPod creates a ServiceEndpoint from a CNIPod.
// It extracts the service name from the pod's labels and the port and weight from annotations.
// The service protocol comes from the endpoint.aether.io/protocol annotation
// (default HTTP; "tcp" registers a non-HTTP TCP-over-mTLS service). Container
// and Kubernetes metadata are included along with node locality information.
func NewServiceEndpointFromCNIPod(clusterName string, nodeName string, nodeRegion string, nodeZone string, nodeIP string, cniPod *cniv1.CNIPod) (string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint, error) {
	protocol, err := endpointmeta.Protocol(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	serviceName, err := getServiceName(cniPod)
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	port, err := endpointmeta.Port(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	weight, err := endpointmeta.Weight(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	ports, err := endpointmeta.Ports(cniPod.GetAnnotations(), port)
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	// Per-port L4 class (proposal 037). Carried on the endpoint rather than in
	// the key: the key protocol stays what it always was, and a reader that
	// predates this field treats its absence as "every port is the protocol of
	// my key", which is exactly the pre-037 meaning.
	portProtocols, err := endpointmeta.PortProtocols(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	endpoint := &registryv1.ServiceEndpoint{
		Ip:              cniPod.GetIps()[0],
		ClusterName:     clusterName,
		Port:            uint32(port),
		Ports:           ports,
		PortProtocols:   portProtocols,
		Weight:          weight,
		Metadata:        endpointmeta.Metadata(cniPod.GetAnnotations()),
		HealthCheckMode: HealthCheckModeFromAnnotations(cniPod.GetAnnotations()),
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      cniPod.GetContainerId(),
			NetworkNamespace: cniPod.GetNetworkNamespace(),
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: cniPod.GetNamespace(),
			PodName:   cniPod.GetName(),
			NodeName:  nodeName,
			NodeIp:    nodeIP,
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

// getServiceName returns the namespace-qualified registry key for the pod's mesh
// service: "<namespace>/<serviceAccount>" (proposal 020 Part 1). The identity
// unit is still the ServiceAccount, but the key is now namespace-qualified so two
// workloads sharing a ServiceAccount name across namespaces (e.g. "default") no
// longer collapse into one service. serviceref is the single source of truth for
// the key format.
func getServiceName(cniPod *cniv1.CNIPod) (string, error) {
	sa := cniPod.GetServiceAccount()
	if sa == "" {
		return "", fmt.Errorf("missing service account")
	}
	ns := cniPod.GetNamespace()
	if ns == "" {
		return "", fmt.Errorf("missing namespace")
	}
	return serviceref.New(ns, sa).Key(), nil
}

// HealthCheckModeFromAnnotations maps the endpoint.aether.io/health-check-mode
// annotation to the ServiceEndpoint health-check mode. "active" yields ACTIVE;
// "eds" or unset yields EDS — delegated liveness is the default: the node-local
// agent vets each endpoint once and publishes its health over EDS, so new
// endpoints enter every client pre-warmed (no per-client first-HC round) and a
// per-pod annotation opts back into per-client active checking.
func HealthCheckModeFromAnnotations(annotations map[string]string) registryv1.ServiceEndpoint_HealthCheckMode {
	switch annotations[aetherannotations.AnnotationEndpointHealthCheckMode] {
	case aetherannotations.HealthCheckModeActive:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_ACTIVE
	default:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS
	}
}
