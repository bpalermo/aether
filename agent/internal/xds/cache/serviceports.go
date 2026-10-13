package cache

import (
	"fmt"
	"slices"
	"strings"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
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
}

// known reports whether anything was derived for the service.
func (p servicePorts) known() bool {
	return p.legacyPort != 0 || len(p.http)+len(p.tcp)+len(p.udp) > 0
}

// servesHTTP reports whether the service declares at least one HTTP port.
func (p servicePorts) servesHTTP() bool { return len(p.http) > 0 }

// hasHTTP reports whether port is one of the service's HTTP ports.
func (p servicePorts) hasHTTP(port uint32) bool { return slices.Contains(p.http, port) }

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
func (p servicePorts) legacyTCPPort() uint32 {
	if p.legacyClass == registryv1.PortProtocol_PORT_PROTOCOL_TCP {
		return p.legacyPort
	}
	return 0
}

// equal reports whether two derivations are the same. Every slice is sorted at
// derivation, so endpoint churn that leaves the declarations alone compares
// equal; that comparison is what keeps a pod ADD or DEL from rebuilding the
// capture listeners (proposal 037 Risk 4, proposal 043 Risk 8).
func (p servicePorts) equal(o servicePorts) bool {
	return p.legacyPort == o.legacyPort && p.legacyClass == o.legacyClass &&
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

// endpointPortClasses returns the class of every port one endpoint serves.
//
// The authoritative field is the endpoint's own port_protocols. A row written
// before proposal 037 carries none and was classified whole, by the protocol
// it was registered under, so its ports (or its one port) take listed, the
// protocol of the listing it was read from; so does a port its port_protocols
// does not name.
func endpointPortClasses(ep *registryv1.ServiceEndpoint, listed registryv1.Service_Protocol) map[uint32]registryv1.PortProtocol {
	if listed == registryv1.Service_PROTOCOL_UNSPECIFIED {
		listed = registryv1.Service_PROTOCOL_HTTP
	}
	fallback := endpointmeta.PortProtocolFromService(listed)
	declared := ep.GetPortProtocols()
	out := make(map[uint32]registryv1.PortProtocol, len(declared)+1)
	add := func(port uint32) {
		if port == 0 || port > 65535 {
			return
		}
		if class := declared[port]; class != registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED {
			out[port] = class
			return
		}
		out[port] = fallback
	}
	for port := range declared {
		add(port)
	}
	for _, port := range ep.GetPorts() {
		add(port)
	}
	add(ep.GetPort())
	return out
}

// endpointIdentity identifies an endpoint across listings: a pod that serves
// ports of two classes is returned by two listings, with the same row.
type endpointIdentity struct{ cluster, ip string }

// distinctEndpoints calls visit once per distinct endpoint of listings, with
// the listing it was first read from.
func distinctEndpoints(listings []listedEndpoints, visit func(ep *registryv1.ServiceEndpoint, listed registryv1.Service_Protocol)) {
	// An endpoint can only repeat ACROSS listings, so a service in one listing
	// -- nearly every service -- needs no identity set. This runs for every
	// service on every registry refresh of every agent.
	populated := 0
	for _, l := range listings {
		if len(l.endpoints) > 0 {
			populated++
		}
	}
	var seen map[endpointIdentity]struct{}
	if populated > 1 {
		seen = map[endpointIdentity]struct{}{}
	}
	for _, l := range listings {
		for _, ep := range l.endpoints {
			if seen != nil {
				id := endpointIdentity{cluster: ep.GetClusterName(), ip: ep.GetIp()}
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
			}
			visit(ep, l.protocol)
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
	type pair struct {
		port  uint32
		class registryv1.PortProtocol
	}
	votes := map[pair]int{}
	distinctEndpoints(listings, func(ep *registryv1.ServiceEndpoint, listed registryv1.Service_Protocol) {
		p := ep.GetPort()
		if p == 0 || p > 65535 {
			return
		}
		votes[pair{port: p, class: endpointPortClasses(ep, listed)[p]}]++
	})
	var best pair
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
	distinctEndpoints(listings, func(ep *registryv1.ServiceEndpoint, listed registryv1.Service_Protocol) {
		for port, class := range endpointPortClasses(ep, listed) {
			if votes[port] == nil {
				votes[port] = map[registryv1.PortProtocol]int{}
			}
			votes[port][class]++
		}
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
	slices.Sort(out.conflicts)
	out.legacyPort, out.legacyClass = legacyTarget(listings...)
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
