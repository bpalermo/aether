package cache

import (
	"context"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// PodPortClasses returns the class of every port of a service as this node's
// route tables serve it, and whether the node knows the service's ports from
// registry rows at all.
//
// It is the answer the tables themselves are built from (servicePorts: one
// class per port number, the one most pods declare when they disagree), handed
// out for a route's status, so that the status and the table cannot be built
// from different facts: a backendRef the table answers 500 for, because the
// port's class here is not HTTP or the port is not one of these, is one the
// status says False of (agent/internal/gamma, PodPorts).
func (c *SnapshotCache) PodPortClasses(_ context.Context, service string) (map[uint32]registryv1.PortProtocol, bool) {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	p := c.servicePorts[service]
	if !p.known() {
		return nil, false
	}
	out := make(map[uint32]registryv1.PortProtocol, len(p.http)+len(p.tcp)+len(p.udp))
	for _, port := range p.http {
		out[port] = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	}
	for _, port := range p.tcp {
		out[port] = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	}
	for _, port := range p.udp {
		out[port] = registryv1.PortProtocol_PORT_PROTOCOL_UDP
	}
	return out, true
}
