package gamma

import (
	"context"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// PodPorts answers, for a route's status, what the pods of a service declare
// their ports to be. The node agent's implementation is the snapshot cache
// itself (cache.SnapshotCache.PodPortClasses): the class of each port as the
// node's route tables are built from it, derived ONCE, by one function, from
// the registry rows the node holds.
//
// It exists for the Services the mesh did not generate. A generated Service
// carries the answer on its own ports (the registrar derived them from the
// registry). A user's Service says only that a port exists and which pod port
// is behind it; whether the pods serve that port as HTTP is their
// declaration. The data plane answers 500 for a backendRef whose target port
// they declare raw TCP, or do not declare, and a status that said
// ResolvedRefs=True for such a reference was a table and a status built from
// different facts. They are the same facts now, including where pods
// disagree: the table takes the class most pods declare, and so does this.
//
// It costs no registry read. (It was one per protocol and service, and a
// protocol the service has no row under fell back to a listing of the whole
// mesh, from every node, on every reconcile.)
type PodPorts interface {
	// PodPortClasses returns the class of every port of service
	// ("<ns>/<name>") as this node's tables serve it, and whether the node
	// knows the service's ports at all. known is false when it holds no row
	// of the service: then nothing is known here, and nothing is judged.
	PodPortClasses(ctx context.Context, service string) (classes map[uint32]registryv1.PortProtocol, known bool)
}

// withPodPorts returns a user Service's facts judged by what its pods declare:
// portMap is the Service's port map (Service port -> pod port, 0 for a named
// targetPort that was not resolved), classes what PodPorts said.
//
// A Service port whose pod port is HTTP is verified; one whose pod port is raw
// TCP or UDP is a real port that is not HTTP; one whose pod port no pod
// declares is recorded as such. An unresolved name stays unverified.
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
		case class != registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
			f.ports[port] = false
		}
		delete(f.unverified, port)
	}
	return f
}
