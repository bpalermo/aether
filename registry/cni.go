package registry

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
)

// NewServiceEndpointFromCNIPod creates a ServiceEndpoint from a CNIPod.
// It extracts the service name from the pod's labels and the port and weight from annotations.
// The service protocol comes from the endpoint.aether.io/protocol annotation
// (default HTTP; "tcp" registers a non-HTTP TCP-over-mTLS service). Container
// and Kubernetes metadata are included along with node locality information.
func NewServiceEndpointFromCNIPod(clusterName string, nodeName string, nodeRegion string, nodeZone string, cniPod *cniv1.CNIPod) (string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint, error) {
	protocol, err := getProtocolFromAnnotations(cniPod.GetAnnotations())
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

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

	ports, err := getPortsFromAnnotations(cniPod.GetAnnotations(), port)
	if err != nil {
		return "", registryv1.Service_PROTOCOL_UNSPECIFIED, nil, err
	}

	endpoint := &registryv1.ServiceEndpoint{
		Ip:              cniPod.GetIps()[0],
		ClusterName:     clusterName,
		Port:            uint32(port),
		Ports:           ports,
		Weight:          weight,
		Metadata:        getEndpointMetadataFromAnnotations(cniPod.GetAnnotations()),
		HealthCheckMode: HealthCheckModeFromAnnotations(cniPod.GetAnnotations()),
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

// getProtocolFromAnnotations maps the endpoint.aether.io/protocol annotation to
// the service protocol. Unset or "http" yields PROTOCOL_HTTP (default); "tcp"
// yields PROTOCOL_TCP (a non-HTTP TCP-over-mTLS service, reached through the
// transparent-capture TCP floor). Any other value is rejected so a typo never
// silently registers a service under the wrong (or unspecified) protocol.
func getProtocolFromAnnotations(annotations map[string]string) (registryv1.Service_Protocol, error) {
	switch annotations[constants.AnnotationEndpointProtocol] {
	case "", constants.ProtocolHTTP:
		return registryv1.Service_PROTOCOL_HTTP, nil
	case constants.ProtocolTCP:
		return registryv1.Service_PROTOCOL_TCP, nil
	default:
		return registryv1.Service_PROTOCOL_UNSPECIFIED, fmt.Errorf("invalid protocol annotation %q (want %q or %q)",
			annotations[constants.AnnotationEndpointProtocol], constants.ProtocolHTTP, constants.ProtocolTCP)
	}
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

// getPortsFromAnnotations parses the full served-port set from the
// endpoint.aether.io/ports annotation (comma-separated). The default port is
// always included. Returns the sorted, de-duplicated set; when the annotation
// is absent the set is just {defaultPort}.
func getPortsFromAnnotations(annotations map[string]string, defaultPort uint16) ([]uint32, error) {
	set := map[uint32]struct{}{uint32(defaultPort): {}}
	if raw, ok := annotations[constants.AnnotationEndpointPorts]; ok && raw != "" {
		for _, part := range strings.Split(raw, ",") {
			t := strings.TrimSpace(part)
			if t == "" {
				continue
			}
			// Strip an optional "=proto" suffix (e.g. "9090=h2"): the protocol
			// is an agent-local inbound concern; the registry carries only the
			// numeric port set (per-port EDS membership).
			if i := strings.IndexByte(t, '='); i >= 0 {
				t = strings.TrimSpace(t[:i])
			}
			p, err := strconv.ParseUint(t, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid ports annotation entry %q", t)
			}
			set[uint32(p)] = struct{}{}
		}
	}
	ports := make([]uint32, 0, len(set))
	for p := range set {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports, nil
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

// HealthCheckModeFromAnnotations maps the endpoint.aether.io/health-check-mode
// annotation to the ServiceEndpoint health-check mode. "active" yields ACTIVE;
// "eds" or unset yields EDS — delegated liveness is the default: the node-local
// agent vets each endpoint once and publishes its health over EDS, so new
// endpoints enter every client pre-warmed (no per-client first-HC round) and a
// per-pod annotation opts back into per-client active checking.
func HealthCheckModeFromAnnotations(annotations map[string]string) registryv1.ServiceEndpoint_HealthCheckMode {
	switch annotations[constants.AnnotationEndpointHealthCheckMode] {
	case constants.HealthCheckModeActive:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_ACTIVE
	default:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS
	}
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
