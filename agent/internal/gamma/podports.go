package gamma

import (
	"context"
	"maps"

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
	// of the service: then nothing is known here, and this node judges
	// nothing of it. What another node that does hold it judged is in the
	// route's status (judged.go), and is what this node then goes by.
	PodPortClasses(ctx context.Context, service string) (classes map[uint32]registryv1.PortProtocol, known bool)
}

// verdict is this node's own judgement of a pod port of the service: what its
// tables serve the port as. ok is false when the node holds no row of the
// service: then it cannot judge, and only then may it take another node's
// verdict (judgedFacts).
func (f portFacts) verdict(pod uint32) (v string, ok bool) {
	if !f.held {
		return "", false
	}
	class, declared := f.classes[pod]
	switch {
	case !declared:
		return verdictUndeclared, true
	case class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
		return verdictHTTP, true
	case class == registryv1.PortProtocol_PORT_PROTOCOL_UDP:
		return verdictUDP, true
	}
	return verdictTCP, true
}

// open returns the pod port behind Service port `port` when the port is one
// only the pods can speak for: a port of a user's Service whose targetPort is
// a known number (pods holds those and no other), or a port of a service that
// has no Service object (its ports are the pods' own) that has no verdict yet.
func (f portFacts) open(port uint32) (pod uint32, ok bool) {
	if f.absent {
		_, done := f.ports[port]
		return port, !done
	}
	pod = f.pods[port]
	return pod, pod != 0
}

// apply returns f with Service port `port` judged: its pod port `pod` is v.
// An HTTP port is verified; a raw TCP or UDP one is a real port that is not
// HTTP; one no pod declares is recorded as such. f's maps are written: the
// caller owns them (clone).
func (f portFacts) apply(port, pod uint32, v string) portFacts {
	switch v {
	case verdictHTTP:
		f.ports[port] = true
	case verdictUndeclared:
		f.ports[port] = true
		f.undeclared[port] = pod
	default:
		f.ports[port] = false
	}
	delete(f.unverified, port)
	return f
}

// clone returns f with maps of its own, none nil.
func (f portFacts) clone() portFacts {
	c := f
	c.ports = make(map[uint32]bool, len(f.ports)+1)
	maps.Copy(c.ports, f.ports)
	c.unverified = make(map[uint32]bool, len(f.unverified))
	maps.Copy(c.unverified, f.unverified)
	c.undeclared = make(map[uint32]uint32, len(f.undeclared)+1)
	maps.Copy(c.undeclared, f.undeclared)
	return c
}
