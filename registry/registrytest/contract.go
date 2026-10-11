// Package registrytest provides shared assertions every Registry backend must
// satisfy, so the implementations (kubernetes, etcd, registrar) stay consistent.
// The keying format is centralized in common/serviceref and the write
// path qualifies once in registry/cni.go; this contract guards against a backend
// (especially a read-only one that DERIVES keys, like the kubernetes backend)
// silently diverging to a bare or cross-namespace key.
package registrytest

import (
	"maps"
	"slices"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/serviceref"
)

// RequireNamespaceQualifiedKeys asserts the contract that ListAllEndpoints output
// must satisfy regardless of backend:
//
//  1. every service key is a namespace-qualified "<ns>/<sa>" (serviceref.ParseKey
//     accepts it) — a bare ServiceAccount key is rejected by every downstream
//     consumer (the VIP generator, the agent dependency set), so it is a bug;
//  2. every endpoint listed under a key whose KubernetesMetadata carries a
//     namespace agrees with the key's namespace — catches the collision where
//     same-named ServiceAccounts in different namespaces merge under one key.
//
// Call it from each backend's ListAllEndpoints test (the kubernetes unit test and
// the etcd integration test) so the keying can't drift.
func RequireNamespaceQualifiedKeys(t testing.TB, services map[string][]*registryv1.ServiceEndpoint) {
	t.Helper()
	for key, eps := range services {
		ref, ok := serviceref.ParseKey(key)
		if !ok {
			t.Errorf("service key %q is not namespace-qualified <ns>/<sa> (a backend must key like serviceref/cni.go)", key)
			continue
		}
		for _, ep := range eps {
			epNS := ep.GetKubernetesMetadata().GetNamespace()
			if epNS != "" && epNS != ref.Namespace {
				t.Errorf("service key %q has an endpoint in namespace %q — same-named ServiceAccounts must not merge across namespaces", key, epNS)
			}
		}
	}
}

// RequirePerPortProtocols asserts the per-port contract of the protocol
// listings, which every backend must satisfy (proposals 037 and 043). It
// replaces the older "a service is HTTP xor TCP" check: a service has ports, a
// port has a class, and a pod that serves ports of two classes is in both
// listings on every backend.
//
// listings is ListAllEndpoints' output per protocol. Pass every protocol the
// test listed; a protocol left out is simply not checked.
//
//  1. Every endpoint returned under protocol P advertises at least one port of
//     class P in port_protocols. A listing is "who serves a port of P", so a
//     row with no P port is a row some reader would put behind the wrong kind
//     of cluster. A row with an EMPTY port_protocols is exempt: it was written
//     before the field existed, and its meaning is "every port is the protocol
//     of the key I was read from".
//  2. The same endpoint (service key, cluster, IP) returned under two protocols
//     carries the same row: the same port, ports and port_protocols. A reader
//     takes a port's class from the row, so two listings that disagree about
//     one pod give two readers two different services.
//
// Call it from each backend's listing tests (the kubernetes unit tests and the
// etcd integration test).
func RequirePerPortProtocols(t testing.TB, listings map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint) {
	t.Helper()

	seen := map[rowIdentity]rowSighting{}
	// A fixed order, so a violation is reported the same way on every run.
	for _, protocol := range slices.Sorted(maps.Keys(listings)) {
		services := listings[protocol]
		for _, key := range slices.Sorted(maps.Keys(services)) {
			for _, ep := range services[key] {
				requirePerPortRow(t, seen, protocol, key, ep)
			}
		}
	}
}

// rowIdentity is one endpoint of one service; rowSighting is the first listing
// it was seen in, and the row it had there.
type (
	rowIdentity struct{ service, cluster, ip string }
	rowSighting struct {
		protocol registryv1.Service_Protocol
		endpoint *registryv1.ServiceEndpoint
	}
)

// requirePerPortRow applies both rules of RequirePerPortProtocols to one row.
func requirePerPortRow(t testing.TB, seen map[rowIdentity]rowSighting, protocol registryv1.Service_Protocol, key string, ep *registryv1.ServiceEndpoint) {
	t.Helper()
	if !servesClass(ep, protocol) {
		t.Errorf("service %q endpoint %s is listed under %v but advertises no %v port (port_protocols=%v)",
			key, ep.GetIp(), protocol, protocol, ep.GetPortProtocols())
	}
	id := rowIdentity{service: key, cluster: ep.GetClusterName(), ip: ep.GetIp()}
	first, dup := seen[id]
	if !dup {
		seen[id] = rowSighting{protocol: protocol, endpoint: ep}
		return
	}
	if !sameRow(first.endpoint, ep) {
		t.Errorf("service %q endpoint %s differs between the %v and %v listings: "+
			"port %d vs %d, ports %v vs %v, port_protocols %v vs %v",
			key, ep.GetIp(), first.protocol, protocol,
			first.endpoint.GetPort(), ep.GetPort(),
			first.endpoint.GetPorts(), ep.GetPorts(),
			first.endpoint.GetPortProtocols(), ep.GetPortProtocols())
	}
}

// servesClass reports whether ep advertises a port of the class protocol
// names. An empty port_protocols is a row from before the field existed and
// serves whatever listing it is in.
func servesClass(ep *registryv1.ServiceEndpoint, protocol registryv1.Service_Protocol) bool {
	if len(ep.GetPortProtocols()) == 0 {
		return true
	}
	for _, class := range ep.GetPortProtocols() {
		// The two enums are numerically identical (endpointmeta pins it).
		if registryv1.Service_Protocol(class) == protocol {
			return true
		}
	}
	return false
}

// sameRow compares what a reader derives a service's ports from. The served
// set is compared as a set: its order carries no meaning.
func sameRow(a, b *registryv1.ServiceEndpoint) bool {
	if a.GetPort() != b.GetPort() || !maps.Equal(a.GetPortProtocols(), b.GetPortProtocols()) {
		return false
	}
	pa, pb := slices.Clone(a.GetPorts()), slices.Clone(b.GetPorts())
	slices.Sort(pa)
	slices.Sort(pb)
	return slices.Equal(pa, pb)
}
