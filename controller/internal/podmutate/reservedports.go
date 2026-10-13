package podmutate

import (
	"fmt"
	"strings"

	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/registry/endpointmeta"
	corev1 "k8s.io/api/core/v1"
)

// reservedPortDenial returns why a mesh-managed pod's port declaration cannot
// be served, or "" when it can: the pod declares, in endpoint.aether.io/port
// or endpoint.aether.io/ports, one of the mesh's own two ports
// (mesh.ReservedPort), as an application port of any protocol.
//
// The pod is DENIED at admission, where its author is looking. Without this it
// would run and register, the registrar would not write that port onto the
// service's Service, and the node agents would not serve it: the port would be
// unreachable through the mesh with a WARN in two logs and nothing on the pod.
//
// It is about the ANNOTATIONS, which are what the mesh routes by. A container
// may listen on these numbers; the mesh never dials a container port that is
// not declared.
func reservedPortDenial(pod *corev1.Pod) string {
	reserved := endpointmeta.ReservedPorts(pod.Annotations)
	if len(reserved) == 0 {
		return ""
	}
	names := make([]string, 0, len(reserved))
	for _, p := range reserved {
		names = append(names, fmt.Sprint(p))
	}
	return fmt.Sprintf("the pod declares port %s as an application port (%s=%q, %s=%q); "+
		"%d and %d are the mesh's own ports (its HTTP proxy port and its L4 port) and cannot be a port of a service: "+
		"serve and declare another port number",
		strings.Join(names, " and "),
		annotations.AnnotationEndpointPort, pod.Annotations[annotations.AnnotationEndpointPort],
		annotations.AnnotationEndpointPorts, pod.Annotations[annotations.AnnotationEndpointPorts],
		mesh.ProxyOutboundPort, mesh.ProxyL4OutboundPort)
}
