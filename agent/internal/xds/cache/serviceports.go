package cache

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/registry/endpointmeta"
)

// servicePorts is what the registry endpoints say about ONE service's ports
// (proposal 043, "The model"): which port numbers it serves and what speaks on
// each. A service has ports; a port has a class. Nothing here, and nothing
// built from it, carries a protocol for the service as a whole.
//
// The zero value is "nothing known": a service this node holds no endpoint of.
type servicePorts struct {
	// http, tcp and udp are the service's ports by class, each sorted. A port
	// number is in exactly one of them (classOfPort decides when the service's
	// endpoints disagree).
	http []uint32
	tcp  []uint32
	udp  []uint32
	// legacyPort and legacyClass are the service's LEGACY TARGET: the port its
	// pods declare in endpoint.aether.io/port, and that port's class. It is
	// what the three spellings that name no real port (":18081", ":18082" and
	// the bare name) reach until they are removed, and nothing else: see
	// legacyTarget. Zero when unknown.
	legacyPort  uint32
	legacyClass registryv1.PortProtocol
	// conflicts names, sorted, the ports whose class the service's endpoints
	// disagree about, as "<port>: <n> http, <m> tcp". Reported, not acted on.
	conflicts []string
	// reservedOnly: the service's endpoints declare ports, and every one of
	// them is a port reserved for the mesh (reservedMeshPort). The node then
	// KNOWS the service, and knows it serves nothing: its names are refused
	// like those of any service with no HTTP port, instead of being asked
	// for on demand like a name nothing is known of.
	reservedOnly bool
}

// known reports whether anything was derived for the service.
func (p servicePorts) known() bool {
	return p.reservedOnly || p.legacyPort != 0 || len(p.http)+len(p.tcp)+len(p.udp) > 0
}

// servesHTTP reports whether the service declares at least one HTTP port.
func (p servicePorts) servesHTTP() bool { return len(p.http) > 0 }

// hasHTTP reports whether port is one of the service's HTTP ports.
func (p servicePorts) hasHTTP(port uint32) bool { return slices.Contains(p.http, port) }

// declaresNonHTTP reports whether the service declares port as raw TCP or
// UDP: it is known to be something else than HTTP, which is more than not
// being known as HTTP (a port no listed row declares at all).
func (p servicePorts) declaresNonHTTP(port uint32) bool {
	return slices.Contains(p.tcp, port) || slices.Contains(p.udp, port)
}

// classOf returns the class of one of the service's ports, UNSPECIFIED for a
// port the service does not declare.
func (p servicePorts) classOf(port uint32) registryv1.PortProtocol {
	switch {
	case slices.Contains(p.http, port):
		return registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	case slices.Contains(p.tcp, port):
		return registryv1.PortProtocol_PORT_PROTOCOL_TCP
	case slices.Contains(p.udp, port):
		return registryv1.PortProtocol_PORT_PROTOCOL_UDP
	default:
		return registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED
	}
}

// legacyHTTPPort returns the legacy target when it is an HTTP port, else 0.
func (p servicePorts) legacyHTTPPort() uint32 {
	if p.legacyClass == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
		return p.legacyPort
	}
	return 0
}

// legacyTCPPort returns the legacy target when it is a raw-TCP port, else 0.
//
// The port must also be SERVED as raw TCP here (its listing holds the pod):
// the chains built for this number forward to the floor cluster, which exists
// only then.
func (p servicePorts) legacyTCPPort() uint32 {
	if p.legacyClass == registryv1.PortProtocol_PORT_PROTOCOL_TCP && slices.Contains(p.tcp, p.legacyPort) {
		return p.legacyPort
	}
	return 0
}

// equal reports whether two derivations are the same. Every slice is sorted at
// derivation, so endpoint churn that leaves the declarations alone compares
// equal; that comparison is what keeps a pod ADD or DEL from rebuilding the
// capture listeners (proposal 037 Risk 4, proposal 043 Risk 8).
func (p servicePorts) equal(o servicePorts) bool {
	return p.legacyPort == o.legacyPort && p.legacyClass == o.legacyClass && p.reservedOnly == o.reservedOnly &&
		slices.Equal(p.http, o.http) && slices.Equal(p.tcp, o.tcp) && slices.Equal(p.udp, o.udp)
}

// describe lists the service's ports and classes for a refusal body, e.g.
// "8080 (http), 9000 (tcp)".
func (p servicePorts) describe() string {
	type pc struct {
		port  uint32
		class string
	}
	all := make([]pc, 0, len(p.http)+len(p.tcp)+len(p.udp))
	for _, port := range p.http {
		all = append(all, pc{port, "http"})
	}
	for _, port := range p.tcp {
		all = append(all, pc{port, "tcp"})
	}
	for _, port := range p.udp {
		all = append(all, pc{port, "udp"})
	}
	if len(all) == 0 {
		return "none"
	}
	slices.SortFunc(all, func(a, b pc) int { return int(a.port) - int(b.port) })
	parts := make([]string, 0, len(all))
	for _, e := range all {
		parts = append(parts, fmt.Sprintf("%d (%s)", e.port, e.class))
	}
	return strings.Join(parts, ", ")
}

// listedEndpoints is what one registry listing returned for one service: the
// protocol that was asked for and the rows that came back.
type listedEndpoints struct {
	protocol  registryv1.Service_Protocol
	endpoints []*registryv1.ServiceEndpoint
}

// reservedMeshPort reports whether port is one of the mesh's own two ports,
// which no service can declare as an application port: the HTTP proxy port
// (18081) and the L4 port (18082). On a captured dial both numbers select the
// mesh's own chains, and "<name>:18081" in an authority names the proxy, so a
// pod's port of either number could never be told from the mesh's. The
// controller's pod webhook refuses such a pod at admission; a pod that is
// running anyway (admitted before, or with the webhook off) is not served on
// that port, and the agent says so (reservedPortNotes).
func reservedMeshPort(port uint32) bool { return meshconst.ReservedPort(port) }

// reservedPortNotes names, sorted, the reserved ports the endpoints of the
// listings declare, for the operator: "18081: reserved for the mesh, not
// served (2 endpoints declare it)".
func reservedPortNotes(listings []listedEndpoints) []string {
	declaring := map[uint32]map[string]struct{}{}
	note := func(ep *registryv1.ServiceEndpoint, port uint32) {
		if !reservedMeshPort(port) {
			return
		}
		if declaring[port] == nil {
			declaring[port] = map[string]struct{}{}
		}
		declaring[port][ep.GetClusterName()+"/"+ep.GetIp()] = struct{}{}
	}
	for _, l := range listings {
		for _, ep := range l.endpoints {
			note(ep, ep.GetPort())
			for _, p := range ep.GetPorts() {
				note(ep, p)
			}
			for p := range ep.GetPortProtocols() {
				note(ep, p)
			}
		}
	}
	out := make([]string, 0, len(declaring))
	for _, port := range slices.Sorted(maps.Keys(declaring)) {
		out = append(out, fmt.Sprintf("%d: reserved for the mesh, not served (%d endpoints declare it)", port, len(declaring[port])))
	}
	return out
}

// endpointPortClasses returns the class of every port one endpoint serves:
// what its port_protocols says, and nothing else.
//
// port_protocols is required (endpointmeta.RowProblem). A row without it used
// to be classified whole, by the protocol of the listing it was read from, and
// so was a port the map did not name. That fallback is gone: a class that is
// not stated is not guessed, and a port with no class is not a port of the
// endpoint. Rows that carry none are dropped before anything is derived
// (typedRegistry); this is the same rule for a row that reaches here anyway.
//
// listed is the listing the row was read from. It decides nothing any more; it
// stays in the signature because every caller has it and a reader of the call
// should see that the listing was deliberately not consulted.
func endpointPortClasses(ep *registryv1.ServiceEndpoint, _ registryv1.Service_Protocol) map[uint32]registryv1.PortProtocol {
	declared := ep.GetPortProtocols()
	out := make(map[uint32]registryv1.PortProtocol, len(declared))
	for port, class := range declared {
		if port == 0 || port > 65535 || reservedMeshPort(port) || class == registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED {
			// A reserved port is a port of no service (reservedMeshPort);
			// a port with no class is not served.
			continue
		}
		out[port] = class
	}
	return out
}

// servedPorts calls visit for every (endpoint, port, class) the listings
// SERVE: a port of class P counts when the endpoint is in the listing of P.
//
// A pod that serves ports of two classes is returned by two listings, with
// the same row, and each listing vouches for the ports of its own class. That
// is also what the cluster entries are built from (one pass per listing), so a
// port that is served here has a cluster, and a port that has no cluster is
// not served: a registrar of chart 2.6 on the kubernetes backend lists a pod
// under its pod-level protocol alone, and behind it the other class's ports of
// a mixed pod name nothing, where they used to get a chain or a route with no
// cluster behind it. No endpoint is counted twice for one port: its class has
// one listing.
func servedPorts(listings []listedEndpoints, visit func(ep *registryv1.ServiceEndpoint, port uint32, class registryv1.PortProtocol)) {
	for _, l := range listings {
		listed := l.protocol
		if listed == registryv1.Service_PROTOCOL_UNSPECIFIED {
			listed = registryv1.Service_PROTOCOL_HTTP
		}
		want := endpointmeta.PortProtocolFromService(listed)
		for _, ep := range l.endpoints {
			for port, class := range endpointPortClasses(ep, listed) {
				if class == want {
					visit(ep, port, class)
				}
			}
		}
	}
}

// classRank orders the classes for a tie: HTTP, then raw TCP, then UDP.
func classRank(c registryv1.PortProtocol) int {
	switch c {
	case registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
		return 0
	case registryv1.PortProtocol_PORT_PROTOCOL_TCP:
		return 1
	case registryv1.PortProtocol_PORT_PROTOCOL_UDP:
		return 2
	default:
		return 3
	}
}

func className(c registryv1.PortProtocol) string {
	switch c {
	case registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
		return "http"
	case registryv1.PortProtocol_PORT_PROTOCOL_TCP:
		return "tcp"
	case registryv1.PortProtocol_PORT_PROTOCOL_UDP:
		return "udp"
	default:
		return "unspecified"
	}
}

// classOfPort decides the class of one port number from how many endpoints
// declare each class for it: the class most endpoints declare, ties to HTTP
// then raw TCP then UDP (proposal 043, fact 4, "A rollout that changes a
// port's protocol"). The answer does not depend on the order of the rows.
//
// The capture listener can build one chain for a port number of a service, so
// one class has to win. A client speaks one protocol, so no rule makes a
// change of class hitless; the documented way to retype a port is a new port
// number. conflict describes a disagreement, and is empty when there is none.
func classOfPort(port uint32, votes map[registryv1.PortProtocol]int) (class registryv1.PortProtocol, conflict string) {
	best := registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED
	for c, n := range votes {
		if best == registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED ||
			n > votes[best] || (n == votes[best] && classRank(c) < classRank(best)) {
			best = c
		}
	}
	if len(votes) > 1 {
		classes := make([]registryv1.PortProtocol, 0, len(votes))
		for c := range votes {
			classes = append(classes, c)
		}
		slices.SortFunc(classes, func(a, b registryv1.PortProtocol) int { return classRank(a) - classRank(b) })
		parts := make([]string, 0, len(classes))
		for _, c := range classes {
			parts = append(parts, fmt.Sprintf("%d %s", votes[c], className(c)))
		}
		conflict = fmt.Sprintf("%d: %s", port, strings.Join(parts, ", "))
	}
	return best, conflict
}

// declaredPort is one endpoint's declared port and its class.
type declaredPort struct {
	port  uint32
	class registryv1.PortProtocol
}

// declaredPortVotes counts, per (declared port, class), the endpoints that
// declare it (legacyTarget).
//
// One vote per endpoint, from the row's OWN declaration of its declared port,
// whichever listing returned the row. What the declared port is does not
// depend on which listings have caught up: a mixed pod whose HTTP row is
// listed before its raw-TCP one is still a pod whose declared port is raw
// TCP, and must not be taken for a service with no declared port (which gets
// a default HTTP cluster). Whether the port is SERVED here is servedPorts'
// question, asked by legacyTCPPort.
//
// Two rows of one (cluster, address) can coexist for a moment (an address
// reused before the old row is gone). The endpoint votes once, and for the
// same candidate whatever order the rows came in: the one that wins the
// tie-break (class, then the lower port).
func declaredPortVotes(listings []listedEndpoints) map[declaredPort]int {
	better := func(a, b declaredPort) bool {
		if classRank(a.class) != classRank(b.class) {
			return classRank(a.class) < classRank(b.class)
		}
		return a.port < b.port
	}
	byEndpoint := map[string]declaredPort{}
	for _, l := range listings {
		for _, ep := range l.endpoints {
			port := ep.GetPort()
			class, ok := endpointPortClasses(ep, l.protocol)[port]
			if !ok {
				continue
			}
			// The same address in two clusters is two endpoints.
			id := ep.GetClusterName() + "/" + ep.GetIp()
			cand := declaredPort{port: port, class: class}
			if prev, dup := byEndpoint[id]; !dup || better(cand, prev) {
				byEndpoint[id] = cand
			}
		}
	}
	votes := map[declaredPort]int{}
	for _, cand := range byEndpoint {
		votes[cand]++
	}
	return votes
}

// legacyTarget decides a service's LEGACY TARGET from the listings it appears
// in: the port its pods declare in endpoint.aether.io/port, with that port's
// class (proposal 043, "The legacy spellings during Phases 1 and 2").
//
// It exists for the three client spellings that name no real port, and for
// what serves them: ":18081" and the bare name reach the legacy target if it
// is an HTTP port, ":18082" reaches it if it is raw TCP (or UDP, on the UDP
// listener), and otherwise the spelling names nothing. It is NOT the service's
// protocol and it does not rank the service's ports; every real port is
// addressed by its own number and typed by its own declaration. The function
// and its callers are deleted with the spellings (proposal 043, Phase 3).
//
// Pods of one service normally declare the same port. When they do not
// (decision 4): every endpoint votes once for its own (port, class); the pair
// with the most endpoints wins; on a tie, HTTP before raw TCP before UDP, then
// the lower port. The answer does not depend on the order of the listings or
// of their rows, where taking the first row did. Health is not consulted: a
// draining endpoint still says what its service declares, and an answer that
// moved with health would rebuild listeners on a probe result.
func legacyTarget(listings ...listedEndpoints) (port uint32, class registryv1.PortProtocol) {
	votes := declaredPortVotes(listings)
	var best declaredPort
	for k, n := range votes {
		switch {
		case best.port == 0, n > votes[best]:
			best = k
		case n < votes[best]:
		case classRank(k.class) != classRank(best.class):
			if classRank(k.class) < classRank(best.class) {
				best = k
			}
		case k.port < best.port:
			best = k
		}
	}
	return best.port, best.class
}

// deriveServicePorts derives one service's ports and classes, and its legacy
// target, from every listing the service appears in.
func deriveServicePorts(listings ...listedEndpoints) servicePorts {
	votes := map[uint32]map[registryv1.PortProtocol]int{}
	servedPorts(listings, func(_ *registryv1.ServiceEndpoint, port uint32, class registryv1.PortProtocol) {
		if votes[port] == nil {
			votes[port] = map[registryv1.PortProtocol]int{}
		}
		votes[port][class]++
	})
	var out servicePorts
	for port, v := range votes {
		class, conflict := classOfPort(port, v)
		switch class {
		case registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
			out.http = append(out.http, port)
		case registryv1.PortProtocol_PORT_PROTOCOL_TCP:
			out.tcp = append(out.tcp, port)
		case registryv1.PortProtocol_PORT_PROTOCOL_UDP:
			out.udp = append(out.udp, port)
		}
		if conflict != "" {
			out.conflicts = append(out.conflicts, conflict)
		}
	}
	slices.Sort(out.http)
	slices.Sort(out.tcp)
	slices.Sort(out.udp)
	reserved := reservedPortNotes(listings)
	out.conflicts = append(out.conflicts, reserved...)
	slices.Sort(out.conflicts)
	out.legacyPort, out.legacyClass = legacyTarget(listings...)
	out.reservedOnly = len(reserved) > 0 && out.legacyPort == 0 && len(out.http)+len(out.tcp)+len(out.udp) == 0
	return out
}

// deriveAllServicePorts derives every listed service's ports from the three
// registry listings of one refresh.
func deriveAllServicePorts(httpEndpoints, tcpEndpoints, udpEndpoints map[string][]*registryv1.ServiceEndpoint) map[string]servicePorts {
	out := make(map[string]servicePorts, len(httpEndpoints)+len(tcpEndpoints)+len(udpEndpoints))
	derive := func(svc string) {
		if _, done := out[svc]; done {
			return
		}
		ports := deriveServicePorts(
			listedEndpoints{protocol: registryv1.Service_PROTOCOL_HTTP, endpoints: httpEndpoints[svc]},
			listedEndpoints{protocol: registryv1.Service_PROTOCOL_TCP, endpoints: tcpEndpoints[svc]},
			listedEndpoints{protocol: registryv1.Service_PROTOCOL_UDP, endpoints: udpEndpoints[svc]},
		)
		if ports.known() {
			out[svc] = ports
		}
	}
	for svc := range httpEndpoints {
		derive(svc)
	}
	for svc := range tcpEndpoints {
		derive(svc)
	}
	for svc := range udpEndpoints {
		derive(svc)
	}
	return out
}
