package gamma

import (
	"context"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/registry"
	"aethermesh.dev/registry/endpointmeta"
)

// PodPorts answers, for a route's status, what the pods of a service declare
// their ports to be: the same registry rows the data plane decides by.
//
// It exists for the Services the mesh did not generate. A generated Service
// carries the answer on its own ports (the registrar derived them from the
// registry). A user's Service says only that a port exists and which pod port
// is behind it; whether the pods serve that port as HTTP is their declaration.
// The data plane reads it from the registry and answers 500 for a backendRef
// whose target port they declare raw TCP, or do not declare. A status that
// said ResolvedRefs=True for such a reference ("not checked here") was a table
// and a status built from different facts.
type PodPorts interface {
	// PodPortClasses returns the class of every port the registered pods of
	// service ("<ns>/<name>") declare, and whether any row of it is held at
	// all. A port the rows disagree about is returned as UNSPECIFIED: it is
	// declared, and nothing more can be said of it here. known is false when
	// the registry holds no row of the service or could not be read: then
	// nothing is known, and nothing is judged.
	PodPortClasses(ctx context.Context, service string) (classes map[uint32]registryv1.PortProtocol, known bool)
}

// RegistryPodPorts is PodPorts over the agent's registry. The read is the
// registry's own per-service one, which is answered for a service this node
// does not watch as well, so that every node's agent says the same of a
// route: the status is one object, written by all of them.
func RegistryPodPorts(reg registry.Registry) PodPorts { return registryPodPorts{reg: reg} }

type registryPodPorts struct{ reg registry.Registry }

func (p registryPodPorts) PodPortClasses(ctx context.Context, service string) (map[uint32]registryv1.PortProtocol, bool) {
	classes := map[uint32]registryv1.PortProtocol{}
	rows := 0
	for _, protocol := range registry.ServedProtocols {
		eps, err := p.reg.ListEndpoints(ctx, service, protocol)
		if err != nil {
			return nil, false
		}
		for _, ep := range eps {
			if endpointmeta.RowProblem(ep) != "" {
				continue // not served by anything (port_protocols is required)
			}
			rows++
			addPodPorts(classes, ep)
		}
	}
	return classes, rows > 0
}

// addPodPorts adds what one row declares to classes. A port two rows give
// different classes is left with none.
func addPodPorts(classes map[uint32]registryv1.PortProtocol, ep *registryv1.ServiceEndpoint) {
	for port, class := range ep.GetPortProtocols() {
		if mesh.ReservedPort(port) {
			continue // a port of no service
		}
		if prev, seen := classes[port]; seen && prev != class {
			class = registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED
		}
		classes[port] = class
	}
}

// withPodPorts returns a user Service's facts judged by what its pods declare:
// portMap is the Service's port map (Service port -> pod port, 0 for a named
// targetPort that was not resolved), classes what PodPorts said.
//
// A Service port whose pod port is HTTP is verified; one whose pod port is raw
// TCP or UDP is a real port that is not HTTP; one whose pod port no pod
// declares is recorded as such. Anything else (an unresolved name, a port the
// pods disagree about) stays unverified.
func (f portFacts) withPodPorts(portMap map[uint32]uint32, classes map[uint32]registryv1.PortProtocol) portFacts {
	for port := range f.unverified {
		pod := portMap[port]
		if pod == 0 {
			continue
		}
		class, declared := classes[pod]
		switch {
		case !declared:
			if f.undeclared == nil {
				f.undeclared = map[uint32]uint32{}
			}
			f.undeclared[port] = pod
		case class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
		case class == registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED:
			continue
		default:
			f.ports[port] = false
		}
		delete(f.unverified, port)
	}
	return f
}
