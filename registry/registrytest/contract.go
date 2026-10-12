// Package registrytest provides shared assertions every Registry backend must
// satisfy, so the implementations (kubernetes, etcd, registrar) stay consistent.
// The keying format is centralized in common/serviceref and the write
// path qualifies once in registry/cni.go; this contract guards against a backend
// (especially a read-only one that DERIVES keys, like the kubernetes backend)
// silently diverging to a bare or cross-namespace key.
package registrytest

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/registry/endpointmeta"

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
// listings is ListAllEndpoints' output per protocol. Pass EVERY protocol the
// backend serves, with an empty map for one that returned nothing: a protocol
// that is not a key of listings is not checked, and rule 3 can only look for a
// row in the listings it was given.
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
//  3. Completeness: an endpoint that declares a port of class P in
//     port_protocols is in listing P of the same service, for every P that is
//     a key of listings. Rules 1 and 2 hold for a backend that files a mixed
//     pod under its pod-level protocol alone, which leaves the pod's other
//     class with no cluster (#1672); this rule is the one that backend breaks.
//     A row with an empty port_protocols declares no class and is exempt, as
//     in rule 1. A caller that passes only some protocols gets completeness
//     among those only.
//
// Call it from each backend's listing tests (the kubernetes unit tests and the
// etcd integration test).
func RequirePerPortProtocols(t testing.TB, listings map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint) {
	t.Helper()

	seen := map[rowIdentity]*rowSighting{}
	// A fixed order, so a violation is reported the same way on every run.
	for _, protocol := range slices.Sorted(maps.Keys(listings)) {
		services := listings[protocol]
		for _, key := range slices.Sorted(maps.Keys(services)) {
			for _, ep := range services[key] {
				requirePerPortRow(t, seen, protocol, key, ep)
			}
		}
	}
	for _, id := range slices.SortedFunc(maps.Keys(seen), compareRowIdentity) {
		requireListedUnderEveryClass(t, listings, id, seen[id])
	}
}

// rowIdentity is one endpoint of one service; rowSighting is the first listing
// it was seen in, the row it had there, and every listing it was seen in.
type (
	rowIdentity struct{ service, cluster, ip string }
	rowSighting struct {
		protocol registryv1.Service_Protocol
		endpoint *registryv1.ServiceEndpoint
		listedIn map[registryv1.Service_Protocol]struct{}
	}
)

func compareRowIdentity(a, b rowIdentity) int {
	return cmp.Or(
		strings.Compare(a.service, b.service),
		strings.Compare(a.cluster, b.cluster),
		strings.Compare(a.ip, b.ip),
	)
}

// requirePerPortRow applies rules 1 and 2 of RequirePerPortProtocols to one
// row, and records the listing it was seen in for rule 3.
func requirePerPortRow(t testing.TB, seen map[rowIdentity]*rowSighting, protocol registryv1.Service_Protocol, key string, ep *registryv1.ServiceEndpoint) {
	t.Helper()
	if problem := endpointmeta.RowProblem(ep); problem != "" {
		t.Errorf("service %q endpoint %s is listed with a row that does not say the protocol of each of its ports (%s; port=%d ports=%v port_protocols=%v): port_protocols is required, and a backend must neither store nor derive such a row",
			key, ep.GetIp(), problem, ep.GetPort(), ep.GetPorts(), ep.GetPortProtocols())
		// Nothing else can be said of a row that does not say what it serves.
		return
	}
	if !servesClass(ep, protocol) {
		t.Errorf("service %q endpoint %s is listed under %v but advertises no %v port (port_protocols=%v)",
			key, ep.GetIp(), protocol, protocol, ep.GetPortProtocols())
	}
	id := rowIdentity{service: key, cluster: ep.GetClusterName(), ip: ep.GetIp()}
	first, dup := seen[id]
	if !dup {
		seen[id] = &rowSighting{
			protocol: protocol, endpoint: ep,
			listedIn: map[registryv1.Service_Protocol]struct{}{protocol: {}},
		}
		return
	}
	first.listedIn[protocol] = struct{}{}
	if !sameRow(first.endpoint, ep) {
		t.Errorf("service %q endpoint %s differs between the %v and %v listings: "+
			"port %d vs %d, ports %v vs %v, port_protocols %v vs %v",
			key, ep.GetIp(), first.protocol, protocol,
			first.endpoint.GetPort(), ep.GetPort(),
			first.endpoint.GetPorts(), ep.GetPorts(),
			first.endpoint.GetPortProtocols(), ep.GetPortProtocols())
	}
}

// requireListedUnderEveryClass applies rule 3 of RequirePerPortProtocols to one
// endpoint: it is in the listing of every class it declares a port of, among
// the listings the caller passed. The classes are those of the row it was
// first seen with; rule 2 reports a row that differs between listings.
func requireListedUnderEveryClass(t testing.TB, listings map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint, id rowIdentity, sighting *rowSighting) {
	t.Helper()
	declared := map[registryv1.Service_Protocol]struct{}{}
	for _, class := range sighting.endpoint.GetPortProtocols() {
		if class == registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED {
			continue // names no class, so no listing to be in
		}
		// The two enums are numerically identical (endpointmeta pins it).
		declared[registryv1.Service_Protocol(class)] = struct{}{}
	}
	for _, protocol := range slices.Sorted(maps.Keys(declared)) {
		if _, passed := listings[protocol]; !passed {
			continue
		}
		if _, listed := sighting.listedIn[protocol]; listed {
			continue
		}
		t.Errorf("service %q endpoint %s declares a %v port and is missing from the %v listing "+
			"(cluster %q, port_protocols=%v)",
			id.service, id.ip, protocol, protocol, id.cluster, sighting.endpoint.GetPortProtocols())
	}
}

// servesClass reports whether ep advertises a port of the class protocol
// names. A row with no port_protocols advertises none: the field is required,
// and no backend may list such a row (requirePerPortRow says so first).
func servesClass(ep *registryv1.ServiceEndpoint, protocol registryv1.Service_Protocol) bool {
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
