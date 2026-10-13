package gamma

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/referencegrant"
	"aethermesh.dev/common/serviceref"
	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// portFacts is what a Service object says about its own ports, for a route's
// status (proposal 043: a parentRef and a backendRef name a REAL port of a
// Service, and a reference that does not is said in the route's status, not
// given another meaning).
//
// The data plane decides the same question from the registry (the snapshot
// cache's port facts). The two agree in steady state: a generated Service's
// ports are derived from the registry by the registrar, and a user Service's
// port map is what the cache is given. The status is the visible half.
type portFacts struct {
	// ports: true for a port that carries HTTP or may (see unverified),
	// false for a real port that is known not to: a generated Service's
	// raw-TCP or UDP port.
	ports map[uint32]bool
	// unverified names the ports of a USER Service: the port exists, and
	// that is all a Service says. Whether its target port serves HTTP is the
	// pods' declaration, which the data plane checks per node and the status
	// cannot: a backendRef to one is answered 500 where the pods declare it
	// raw TCP. The status must not claim it checked.
	unverified map[uint32]bool
	// unknown: a generated Service that carries no real port at all, as a
	// registrar of chart 2.6 writes it. Nothing is known of the service's
	// ports from it (the data plane reads the registry), so nothing is said.
	unknown bool
}

// generatedPortName matches the name the registrar gives a REAL port of a
// generated mesh Service, "p<port>-tcp" or "p<port>-udp". Its other ports
// (mesh, mesh-tcp, http, https) are fixed placeholders, not ports the service
// serves: until chart 2.7.0 a route that named 18081 or 80 was read as "the
// port the pods declare".
var generatedPortName = regexp.MustCompile(`^p[0-9]+-(tcp|udp)$`)

// servicePortFacts reads a Service's real ports.
func servicePortFacts(svc *corev1.Service) portFacts {
	f := portFacts{ports: make(map[uint32]bool, len(svc.Spec.Ports))}
	generated := svc.Labels[aetherlabels.LabelMeshService] == "true"
	for _, sp := range svc.Spec.Ports {
		tcp := sp.Protocol == "" || sp.Protocol == corev1.ProtocolTCP
		if !generated {
			// A user Service: every TCP port is a port a route can name. What
			// its targetPort serves is the pods' declaration, which the data
			// plane checks and this cannot.
			if tcp {
				f.ports[uint32(sp.Port)] = true
				if f.unverified == nil {
					f.unverified = map[uint32]bool{}
				}
				f.unverified[uint32(sp.Port)] = true
			}
			continue
		}
		if !generatedPortName.MatchString(sp.Name) {
			continue
		}
		f.ports[uint32(sp.Port)] = tcp && sp.AppProtocol != nil && *sp.AppProtocol == "http"
	}
	f.unknown = generated && len(f.ports) == 0
	return f
}

func (f portFacts) describe() string {
	if len(f.ports) == 0 {
		return "it has no port"
	}
	var parts []string
	for _, p := range slices.Sorted(mapsKeys(f.ports)) {
		if f.ports[p] {
			parts = append(parts, strconv.Itoa(int(p)))
		} else {
			parts = append(parts, fmt.Sprintf("%d (not HTTP)", p))
		}
	}
	return "its ports: " + strings.Join(parts, ", ")
}

func mapsKeys(m map[uint32]bool) func(func(uint32) bool) {
	return func(yield func(uint32) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// parentPortProblem reports why a Service parentRef's port names no HTTP port
// of the Service, "" when it does, when it names no port, or when the Service
// is not known here (then nothing can be said, and nothing is).
func parentPortProblem(facts map[string]portFacts, p gatewayv1.ParentReference, routeNamespace string) string {
	if p.Port == nil {
		return ""
	}
	ns := routeNamespace
	if p.Namespace != nil && string(*p.Namespace) != "" {
		ns = string(*p.Namespace)
	}
	f, known := facts[serviceref.New(ns, string(p.Name)).Key()]
	if !known || f.unknown {
		return ""
	}
	port := uint32(*p.Port)
	http, has := f.ports[port]
	switch {
	case !has:
		return fmt.Sprintf("Service %s/%s has no port %d (%s); the route is attached to nothing", ns, p.Name, port, f.describe())
	case !http:
		return fmt.Sprintf("port %d of Service %s/%s is not an HTTP port (%s); the route is attached to nothing", port, ns, p.Name, f.describe())
	}
	return ""
}

// backendPortProblem returns the first backendRef whose port names no HTTP
// port of its Service, as a ResolvedRefs reason and message; ok is true when
// there is none. Refs that are not projected (another kind, or not permitted)
// are the shape check's business, and a Service that is not known here is not
// judged.
func backendPortProblem(facts map[string]portFacts, routeNamespace string, refs []gatewayv1.BackendObjectReference) (ok bool, reason, message string) {
	for _, ref := range refs {
		if ref.Port == nil || (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			continue
		}
		ns := referencegrant.BackendNamespace(ref.Namespace)
		if ns == "" {
			ns = routeNamespace
		}
		f, known := facts[serviceref.New(ns, string(ref.Name)).Key()]
		if !known || f.unknown {
			continue
		}
		port := uint32(*ref.Port)
		http, has := f.ports[port]
		switch {
		case !has:
			return false, string(gatewayv1.RouteReasonBackendNotFound),
				fmt.Sprintf("backendRef %s/%s: the Service has no port %d (%s); requests for this backendRef are answered 500", ns, ref.Name, port, f.describe())
		case !http:
			return false, string(gatewayv1.RouteReasonUnsupportedProtocol),
				fmt.Sprintf("backendRef %s/%s: port %d is not an HTTP port (%s); requests for this backendRef are answered 500", ns, ref.Name, port, f.describe())
		}
	}
	return true, "", ""
}

// resolveRefs is the ResolvedRefs verdict of a route: the shape and
// ReferenceGrant check every aether route kind shares, then the ports.
func resolveRefs(facts map[string]portFacts, routeNamespace, routeKind string, refs []gatewayv1.BackendObjectReference, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
	if ok, reason, msg := referencegrant.ResolveBackends(routeNamespace, routeKind, refs, grants); !ok {
		return ok, reason, msg
	}
	if ok, reason, msg := backendPortProblem(facts, routeNamespace, refs); !ok {
		return ok, reason, msg
	}
	if unchecked := uncheckedBackendPorts(facts, routeNamespace, refs); len(unchecked) > 0 {
		return true, string(gatewayv1.RouteReasonResolvedRefs),
			"All backend references name a port of their Service. Not checked here: whether the pods serve " + strings.Join(unchecked, ", ") +
				" as HTTP (a port of a Service the mesh did not generate; the pods' declaration decides, and a request for a port they declare raw TCP, or do not declare, is answered 500)"
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"
}

// uncheckedBackendPorts names, sorted, the backendRefs whose port is one the
// status cannot vouch for (portFacts.unverified), as "<ns>/<svc>:<port>".
func uncheckedBackendPorts(facts map[string]portFacts, routeNamespace string, refs []gatewayv1.BackendObjectReference) []string {
	var out []string
	for _, ref := range refs {
		if ref.Port == nil || (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			continue
		}
		ns := referencegrant.BackendNamespace(ref.Namespace)
		if ns == "" {
			ns = routeNamespace
		}
		if f := facts[serviceref.New(ns, string(ref.Name)).Key()]; f.unverified[uint32(*ref.Port)] {
			out = append(out, fmt.Sprintf("%s/%s:%d", ns, ref.Name, *ref.Port))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
