package registrytest

import (
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry/endpointmeta"
	"google.golang.org/protobuf/proto"
)

// Typed returns ep as a row a registry can hold: when ep carries no
// port_protocols it returns a copy whose port and ports are typed with the
// class of protocol; a row that carries any is returned as it is.
//
// It is a fixture's shorthand, not a rule of the product. port_protocols is
// required (endpointmeta.RowProblem): the product refuses a row without it,
// and no longer reads a class off the listing a row came from. Most tests
// care about one service on one protocol and build their rows with a port and
// nothing else; this is where such a fixture says what it meant, in the fake
// registry that serves it, so that the hundreds of them do not each have to.
// A test ABOUT untyped rows hands them to the product directly.
func Typed(ep *registryv1.ServiceEndpoint, protocol registryv1.Service_Protocol) *registryv1.ServiceEndpoint {
	if ep == nil || len(ep.GetPortProtocols()) > 0 {
		return ep
	}
	if protocol == registryv1.Service_PROTOCOL_UNSPECIFIED {
		protocol = registryv1.Service_PROTOCOL_HTTP
	}
	class := endpointmeta.PortProtocolFromService(protocol)
	out := proto.Clone(ep).(*registryv1.ServiceEndpoint)
	out.PortProtocols = map[uint32]registryv1.PortProtocol{}
	if port := ep.GetPort(); port != 0 {
		out.PortProtocols[port] = class
	}
	for _, port := range ep.GetPorts() {
		out.PortProtocols[port] = class
	}
	if len(out.PortProtocols) == 0 {
		// A fixture that is only an address (a test of the snapshot, the
		// broadcaster, the write-behind queue: nothing about ports). It
		// stands for a pod that serves one port; which one does not matter
		// to such a test.
		out.PortProtocols[fixturePort] = class
	}
	return out
}

// fixturePort is the port Typed gives a fixture row that names none.
const fixturePort = 8080

// TypedRows is Typed over a list of rows. The list is copied only when a row
// had to be.
func TypedRows(rows []*registryv1.ServiceEndpoint, protocol registryv1.Service_Protocol) []*registryv1.ServiceEndpoint {
	var out []*registryv1.ServiceEndpoint
	for i, ep := range rows {
		typed := Typed(ep, protocol)
		if typed == ep {
			continue
		}
		if out == nil {
			out = append([]*registryv1.ServiceEndpoint(nil), rows...)
		}
		out[i] = typed
	}
	if out == nil {
		return rows
	}
	return out
}

// TypedListing is Typed over a listing of one protocol. The map is copied only
// when a row had to be.
func TypedListing(listing map[string][]*registryv1.ServiceEndpoint, protocol registryv1.Service_Protocol) map[string][]*registryv1.ServiceEndpoint {
	var out map[string][]*registryv1.ServiceEndpoint
	for service, rows := range listing {
		typed := TypedRows(rows, protocol)
		if len(typed) == 0 || len(rows) == 0 || &typed[0] == &rows[0] {
			continue
		}
		if out == nil {
			out = make(map[string][]*registryv1.ServiceEndpoint, len(listing))
			for k, v := range listing {
				out[k] = v
			}
		}
		out[service] = typed
	}
	if out == nil {
		return listing
	}
	return out
}
