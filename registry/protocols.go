package registry

import (
	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// ServedProtocols is every registry protocol a service can be registered under,
// in a fixed order, excluding PROTOCOL_UNSPECIFIED.
//
// WHY THIS EXISTS. The registry key is (service, protocol), so anything that
// enumerates services has to enumerate protocols to see all of them. Before
// this, five call sites each carried their own literal two-element slice:
//
//	registrar/internal/server/sync.go       the snapshot the agents watch
//	agent/internal/cni/server/ghostsweep.go the missed-CNI-DEL reconciler
//	registrar/internal/services/generator.go the mesh VIP Services
//	agent/internal/xds/cache/cluster.go      the xDS cluster/EDS build (x2)
//
// A protocol missing from one of those lists does not fail — it disappears. A
// service under an unlisted protocol is never synced to agents, never gets a
// mesh VIP or DNS record, never becomes a cluster, and is never ghost-swept, so
// a missed deregistration leaks into etcd permanently. Every one of those is
// silent, and none of them is a test failure unless a test happens to cover
// that protocol.
//
// Adding PROTOCOL_UDP (#931) meant finding all five by hand. This makes a
// fourth value a one-line change instead, and TestServedProtocolsCoversEnum
// fails if a value is added to the proto and not here — which is the part a
// reviewer would otherwise have to catch by eye.
//
// The order is HTTP, TCP, UDP and is load-bearing for the mesh Service
// generator, whose no-clobber convergence needs a deterministic winner if one
// name ever appeared under two protocols. Append new values; do not reorder.
var ServedProtocols = []registryv1.Service_Protocol{
	registryv1.Service_PROTOCOL_HTTP,
	registryv1.Service_PROTOCOL_TCP,
	registryv1.Service_PROTOCOL_UDP,
}
