package proxy

import "strings"

// The cluster families a snapshot publishes that are told apart by a name
// prefix. The name builders and the predicates below read the same constants,
// so the two cannot drift.
const (
	// tcpClusterPrefix: a service's TCP-floor clusters (TCPClusterName,
	// TCPPortClusterName), mTLS with ALPN "aether-tcp".
	tcpClusterPrefix = "tcp:"
	// udpClusterPrefix: a service's UDP-floor cluster (UDPClusterName),
	// plaintext.
	udpClusterPrefix = "udp:"
	// waypointIngressClusterPrefix: the STATIC cluster of a service's local
	// pods behind the east/west tunnel (WaypointIngressClusterName), which
	// forwards the source's own mTLS bytes raw.
	waypointIngressClusterPrefix = "ew_ingress_"
	// edgeK8sClusterPrefix: the edge's cleartext cluster for a non-mesh
	// HTTPRoute backend (EdgeK8sClusterName).
	edgeK8sClusterPrefix = "edge_k8s_"
)

// ClusterNameCarriesNoPin reports whether a cluster of this name is, by its
// name alone, of a family that never carries a server-identity SAN pin of its
// own, so that it is never one of the cluster entries the agent's pin gauges
// count:
//
//   - per-pod clusters (application delivery, health probe, inbound
//     readiness): loopback or node-local, IsPerPodClusterName;
//   - a QUIC twin: pinned exactly when its h2 base entry is, and counted
//     through that entry;
//   - the UDP floor, the east/west waypoint ingress cluster, the edge's
//     cleartext k8s-Service cluster, the ORIGINAL_DST passthrough and the
//     blackhole: no upstream transport socket at all.
//
// It is the single list. A cluster family added to this package is either one
// IsMeshEntryClusterName recognises or is added here;
// TestEveryClusterConstructorIsOfAClassifiedFamily fails until it is one or
// the other.
func ClusterNameCarriesNoPin(name string) bool {
	return IsPerPodClusterName(name) ||
		IsQUICClusterName(name) ||
		strings.HasPrefix(name, udpClusterPrefix) ||
		strings.HasPrefix(name, waypointIngressClusterPrefix) ||
		strings.HasPrefix(name, edgeK8sClusterPrefix) ||
		name == PassthroughClusterName ||
		name == BlackholeClusterName
}

// IsMeshEntryClusterName reports whether name is one a mesh cluster entry is
// published under: a service's HTTP cluster, per-port cluster or port alias
// ("<svc>.<ns>.<meshDomain>[:<port>]"), or its TCP-floor clusters (the same
// behind "tcp:"). Those are the clusters meant to be mTLS with a
// server-identity pin, on the node agent and on the edge alike.
//
// A name of a no-pin family is never one, whatever it ends in: the waypoint
// ingress cluster is named by a service FQDN behind its prefix.
func IsMeshEntryClusterName(name, meshDomain string) bool {
	if ClusterNameCarriesNoPin(name) {
		return false
	}
	_, ok := ServiceFromClusterName(strings.TrimPrefix(name, tcpClusterPrefix), meshDomain)
	return ok
}
