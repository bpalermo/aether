package endpointmeta

import (
	"fmt"
	"sort"
	"strings"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// The PRIMARY port of a service, and the protocol it speaks.
//
// A pod serving ports of more than one protocol is registered once per
// protocol (proposal 037), so it is returned by more than one listing, with
// the same row under each. Being in the HTTP listing therefore says "this pod
// serves an HTTP port", not "this pod's primary port speaks HTTP": a pod
// declaring `protocol: tcp`, port 9000 and `ports: "9000,8080=h1"` is in both
// listings and its primary is raw TCP.
//
// Everything that needs the primary's protocol reads it through this file, so
// that the registrar (which labels the generated mesh Service) and the agent
// (which decides which filter chains a service gets) cannot answer the
// question differently. Reading it off the listing instead is #1672: the
// service was labelled by the first listing it appeared in, the agent built no
// TCP path for it, and its HTTP path was aimed at the raw-TCP port.

// PrimaryProtocol returns the protocol of ep's primary port.
//
// The authoritative field is the endpoint's own port_protocols[port]. An
// endpoint written before proposal 037 carries no such entry and was
// classified whole, by the protocol it was registered under, so it takes
// listed: the protocol of the listing it was read from.
func PrimaryProtocol(ep *registryv1.ServiceEndpoint, listed registryv1.Service_Protocol) registryv1.PortProtocol {
	if pp := ep.GetPortProtocols()[ep.GetPort()]; pp != registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED {
		return pp
	}
	if listed == registryv1.Service_PROTOCOL_UNSPECIFIED {
		listed = registryv1.Service_PROTOCOL_HTTP
	}
	return PortProtocolFromService(listed)
}

// Listing is what one registry listing returned for one service: the protocol
// that was asked for and the endpoints that came back.
type Listing struct {
	Protocol  registryv1.Service_Protocol
	Endpoints []*registryv1.ServiceEndpoint
}

// PrimaryCandidate is one (primary port, protocol) pair and how many of a
// service's endpoints declare it.
type PrimaryCandidate struct {
	Port      uint32
	Protocol  registryv1.PortProtocol
	Endpoints int
}

// ServicePrimary is a service's primary port and its protocol, as decided by
// ServicePrimaryOf.
type ServicePrimary struct {
	// Port and Protocol are the winning candidate's. Zero / UNSPECIFIED when
	// the service has no endpoint (Known reports false).
	Port     uint32
	Protocol registryv1.PortProtocol
	// Candidates is every distinct pair the endpoints declare, the winner
	// first. More than one means the endpoints disagree (Disagree).
	Candidates []PrimaryCandidate
}

// Known reports whether the service had an endpoint to read a primary from.
func (p ServicePrimary) Known() bool { return len(p.Candidates) > 0 }

// Disagree reports whether the service's endpoints declare more than one
// primary. That is expected for the length of a rollout that changes the
// primary port or its protocol, and a misconfiguration when it lasts: two
// workloads sharing a ServiceAccount, and therefore a mesh service, with
// different declarations.
func (p ServicePrimary) Disagree() bool { return len(p.Candidates) > 1 }

// String renders the candidates, winner first, e.g.
// "9000/tcp (2 endpoints), 8080/http (1 endpoint)".
func (p ServicePrimary) String() string {
	if !p.Known() {
		return "unknown"
	}
	parts := make([]string, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		noun := "endpoints"
		if c.Endpoints == 1 {
			noun = "endpoint"
		}
		parts = append(parts, fmt.Sprintf("%d/%s (%d %s)", c.Port, portProtocolName(c.Protocol), c.Endpoints, noun))
	}
	return strings.Join(parts, ", ")
}

func portProtocolName(p registryv1.PortProtocol) string {
	switch p {
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

// ServicePrimaryOf decides a service's primary port and protocol from the
// listings it appears in.
//
// The rule, which holds whatever order the listings or their rows arrive in:
//
//  1. Every endpoint votes once for its own (port, PrimaryProtocol). An
//     endpoint returned by several listings is one endpoint: it is identified
//     by (cluster, address), and its first appearance is the one read.
//  2. The pair with the most endpoints wins.
//  3. On a tie, HTTP before TCP before UDP, then the lower port.
//
// Endpoints of one service are meant to share a primary (proposal 005), so
// rule 1 alone decides almost always. Rules 2 and 3 exist so that a service
// whose endpoints disagree still gets ONE answer, the same one on every
// component and on every refresh, instead of the answer of whichever row a
// listing happened to return first. The caller can see the disagreement
// (Disagree, Candidates) and should report it.
//
// Health is not consulted: an endpoint that is draining or unhealthy still
// says what its service is, and a classification that moved with health would
// rebuild listeners on a probe result.
func ServicePrimaryOf(listings ...Listing) ServicePrimary {
	votes := primaryVotes(listings)
	if len(votes) == 0 {
		return ServicePrimary{}
	}

	candidates := make([]PrimaryCandidate, 0, len(votes))
	for k, n := range votes {
		candidates = append(candidates, PrimaryCandidate{Port: k.port, Protocol: k.proto, Endpoints: n})
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Endpoints != b.Endpoints {
			return a.Endpoints > b.Endpoints
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.Port < b.Port
	})
	return ServicePrimary{Port: candidates[0].Port, Protocol: candidates[0].Protocol, Candidates: candidates}
}

// primaryPair is one (primary port, protocol) an endpoint declares.
type primaryPair struct {
	port  uint32
	proto registryv1.PortProtocol
}

// endpointID identifies an endpoint across listings.
type endpointID struct{ cluster, ip string }

// primaryVotes counts, per declared pair, the distinct endpoints of listings.
func primaryVotes(listings []Listing) map[primaryPair]int {
	// An endpoint can only repeat ACROSS listings, so a service in one listing
	// -- every service that serves one protocol, which is nearly all of them --
	// needs no identity set. This runs for every service on every registry
	// refresh of every agent; keep that case to one small map.
	var seen map[endpointID]struct{}
	if populatedListings(listings) > 1 {
		seen = map[endpointID]struct{}{}
	}

	votes := map[primaryPair]int{}
	for _, l := range listings {
		for _, ep := range l.Endpoints {
			if seen != nil {
				id := endpointID{cluster: ep.GetClusterName(), ip: ep.GetIp()}
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
			}
			votes[primaryPair{port: ep.GetPort(), proto: PrimaryProtocol(ep, l.Protocol)}]++
		}
	}
	return votes
}

// populatedListings counts the listings that hold at least one endpoint.
func populatedListings(listings []Listing) int {
	n := 0
	for _, l := range listings {
		if len(l.Endpoints) > 0 {
			n++
		}
	}
	return n
}
