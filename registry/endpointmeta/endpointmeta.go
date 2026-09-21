// Package endpointmeta parses the endpoint.aether.io/* pod annotations into the
// ServiceEndpoint fields every registry backend needs.
//
// It is a leaf package, shared with the backends the same way registry/export
// is, so both the CNI registration path (//registry, which builds an endpoint
// from a cniv1.CNIPod) and the Kubernetes backend (//registry/internal/k8s,
// which builds one from a corev1.Pod) read the same annotations through the
// same code.
//
// That sharing is the point. The two paths previously carried their own copies,
// and the copies drifted by OMISSION rather than by disagreement: the
// Kubernetes backend never grew a reader for endpoint.aether.io/protocol or
// endpoint.aether.io/ports, so a pod declaring either was silently registered
// as if it had not (#878, and the per-port EDS membership of proposal 005 never
// worked on that backend at all). A missing parser is invisible in a way a
// wrong one is not: nothing fails, the field is simply zero.
//
// Every function here takes a plain map[string]string so it is indifferent to
// whether the annotations came from a CNI ADD or the API server.
package endpointmeta

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/constants"
	aetherannotations "aethermesh.dev/common/constants/annotations"
)

// Protocol returns the mesh protocol the pod serves, from
// endpoint.aether.io/protocol. Absent or "http" is HTTP; "tcp" registers a
// non-HTTP TCP-over-mTLS service.
//
// An unrecognised value is an error rather than a default, so a typo can never
// silently register a service under the wrong protocol — which, on a backend
// that keys by protocol, means registering it where nothing will look for it.
func Protocol(annotations map[string]string) (registryv1.Service_Protocol, error) {
	switch annotations[aetherannotations.AnnotationEndpointProtocol] {
	case "", aetherannotations.ProtocolHTTP:
		return registryv1.Service_PROTOCOL_HTTP, nil
	case aetherannotations.ProtocolTCP:
		return registryv1.Service_PROTOCOL_TCP, nil
	default:
		return registryv1.Service_PROTOCOL_UNSPECIFIED, fmt.Errorf("invalid protocol annotation %q (want %q or %q)",
			annotations[aetherannotations.AnnotationEndpointProtocol], aetherannotations.ProtocolHTTP, aetherannotations.ProtocolTCP)
	}
}

// Port returns the endpoint's primary port from endpoint.aether.io/port,
// defaulting to constants.DefaultEndpointPort.
func Port(annotations map[string]string) (uint16, error) {
	s, ok := annotations[aetherannotations.AnnotationEndpointPort]
	if !ok {
		return constants.DefaultEndpointPort, nil
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port annotation %q: %w", s, err)
	}
	return uint16(port), nil
}

// Ports returns the full served-port set from endpoint.aether.io/ports
// (comma-separated), sorted and de-duplicated. defaultPort is always a member,
// so the result is never empty and a pod with no annotation yields exactly
// {defaultPort}.
//
// An entry may carry an optional "=proto" suffix (e.g. "9090=h2"). It is
// stripped here: that suffix selects the agent-local loopback codec (h1 vs
// h2c) and the registry carries only the numeric set, which is what per-port
// EDS membership is keyed on (proposal 005).
func Ports(annotations map[string]string, defaultPort uint16) ([]uint32, error) {
	set := map[uint32]struct{}{uint32(defaultPort): {}}
	if raw, ok := annotations[aetherannotations.AnnotationEndpointPorts]; ok && raw != "" {
		for _, part := range strings.Split(raw, ",") {
			t := strings.TrimSpace(part)
			if t == "" {
				continue
			}
			if i := strings.IndexByte(t, '='); i >= 0 {
				t = strings.TrimSpace(t[:i])
			}
			p, err := strconv.ParseUint(t, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid ports annotation entry %q", t)
			}
			set[uint32(p)] = struct{}{}
		}
	}
	ports := make([]uint32, 0, len(set))
	for p := range set {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports, nil
}

// Weight returns the endpoint's load-balancing weight from
// endpoint.aether.io/weight, defaulting to constants.DefaultEndpointWeight.
func Weight(annotations map[string]string) (uint32, error) {
	s, ok := annotations[aetherannotations.AnnotationEndpointWeight]
	if !ok {
		return constants.DefaultEndpointWeight, nil
	}
	weight, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid weight annotation %q: %w", s, err)
	}
	return uint32(weight), nil
}

// Metadata returns the endpoint metadata carried by annotations under the
// endpoint-metadata prefix, with the prefix removed from each key.
func Metadata(annotations map[string]string) map[string]string {
	metadata := map[string]string{}
	prefix := aetherannotations.AnnotationAetherEndpointMetadataPrefix
	for key, value := range annotations {
		if after, ok := strings.CutPrefix(key, prefix); ok && after != "" {
			metadata[after] = value
		}
	}
	return metadata
}

// The health-check mode is deliberately NOT here, even though both backends
// parse the same annotation, because the two readings genuinely differ on the
// UNSET case and the difference is load-bearing:
//
//   - //registry (the CNI registration path) defaults to EDS. Delegated
//     liveness is the default there: the node-local agent vets each endpoint
//     once and publishes its health over EDS, so new endpoints enter every
//     client pre-warmed.
//   - //registry/internal/k8s defaults to UNSPECIFIED, which consumers treat
//     as active. That backend derives endpoints from the API server rather
//     than receiving agent registrations, and the delegated active-HC path
//     applies only to the write-based backends, so it must not claim EDS.
//
// Unifying them would silently flip one backend's default. Keep them apart.

// PortProtocols returns the L4 class of every port the pod advertises
// (proposal 037).
//
// The grammar is the existing endpoint.aether.io/ports suffix, extended:
//
//	endpoint.aether.io/ports:    "8080,9090=h2,9000=tcp,5432=tcp"
//	endpoint.aether.io/protocol: "http"   # the DEFAULT for unsuffixed ports
//
// A port with no suffix takes the pod-level endpoint.aether.io/protocol value,
// which is why that annotation is KEPT rather than deprecated: it becomes the
// default for the port set, and every manifest written before this proposal
// means exactly what it meant before. A pod with `protocol: tcp` and
// `ports: "9000,8080=h1"` has a TCP primary and an HTTP secondary.
//
// Suffix vocabulary, and what each one says:
//
//	h1, http1, (none)  HTTP -- the loopback hop speaks HTTP/1.1
//	h2, http2          HTTP -- the loopback hop speaks h2c
//	tcp                TCP  -- raw mTLS passthrough through the capture floor
//
// h1 and h2 differ only in the agent-local loopback codec, which the registry
// does not carry (AppPortProtocols reads it from the same annotation on the
// agent side). Both are PORT_PROTOCOL_HTTP here.
//
// `grpc` is deliberately NOT accepted. gRPC is h2 on the wire and the mesh does
// nothing gRPC-specific at L4, so accepting it would advertise a distinction
// the data plane does not make. Add it when something consumes it.
//
// An unrecognised suffix is an ERROR, not a default. The alternative — treating
// it as HTTP, which is what the agent-side AppPortProtocols does today with an
// unknown suffix — is how a typo becomes a port silently served over the wrong
// protocol, which is the failure this whole proposal exists to remove.
func PortProtocols(annotations map[string]string) (map[uint32]registryv1.PortProtocol, error) {
	defaultProto, err := Protocol(annotations)
	if err != nil {
		return nil, err
	}
	fallback := PortProtocolFromService(defaultProto)

	primary, err := Port(annotations)
	if err != nil {
		return nil, err
	}

	out := map[uint32]registryv1.PortProtocol{uint32(primary): fallback}

	raw := annotations[aetherannotations.AnnotationEndpointPorts]
	if raw == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		t := strings.TrimSpace(part)
		if t == "" {
			continue
		}
		portStr, suffix := t, ""
		if i := strings.IndexByte(t, '='); i >= 0 {
			portStr = strings.TrimSpace(t[:i])
			suffix = strings.ToLower(strings.TrimSpace(t[i+1:]))
		}
		p, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid ports annotation entry %q", t)
		}
		proto, err := portProtocolFromSuffix(suffix, fallback)
		if err != nil {
			return nil, fmt.Errorf("port %d: %w", p, err)
		}
		out[uint32(p)] = proto
	}
	return out, nil
}

// portProtocolFromSuffix maps one "=proto" suffix to its L4 class. An empty
// suffix takes fallback (the pod-level protocol).
func portProtocolFromSuffix(suffix string, fallback registryv1.PortProtocol) (registryv1.PortProtocol, error) {
	switch suffix {
	case "":
		return fallback, nil
	case "h1", "http1", "http/1.1":
		return registryv1.PortProtocol_PORT_PROTOCOL_HTTP, nil
	case "h2", "http2":
		return registryv1.PortProtocol_PORT_PROTOCOL_HTTP, nil
	case aetherannotations.ProtocolTCP:
		return registryv1.PortProtocol_PORT_PROTOCOL_TCP, nil
	default:
		return registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED,
			fmt.Errorf("unknown port protocol suffix %q (want h1, h2 or tcp)", suffix)
	}
}

// PortProtocolFromService converts a service-level protocol to the per-port
// enum. The two vocabularies are numerically identical and
// TestPortProtocolMatchesServiceProtocol pins that; this function exists so the
// conversion is named and searchable rather than an unexplained cast.
func PortProtocolFromService(p registryv1.Service_Protocol) registryv1.PortProtocol {
	return registryv1.PortProtocol(p)
}

// ServiceProtocolFromPort is the inverse of PortProtocolFromService.
func ServiceProtocolFromPort(p registryv1.PortProtocol) registryv1.Service_Protocol {
	return registryv1.Service_Protocol(p)
}

// ProtocolsServed returns the distinct L4 classes a pod serves across all its
// advertised ports, which is the set of registry keys it must be registered
// under (proposal 037: a pod serving both an HTTP and a raw-TCP port is
// registered once per protocol, with both registrations carrying the same full
// port_protocols map).
func ProtocolsServed(portProtocols map[uint32]registryv1.PortProtocol) []registryv1.Service_Protocol {
	seen := map[registryv1.Service_Protocol]struct{}{}
	for _, pp := range portProtocols {
		seen[ServiceProtocolFromPort(pp)] = struct{}{}
	}
	out := make([]registryv1.Service_Protocol, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
