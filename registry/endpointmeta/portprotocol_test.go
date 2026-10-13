package endpointmeta

import (
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
)

// TestPortProtocolMatchesServiceProtocol pins the numeric correspondence between
// the two enums that describe the same vocabulary.
//
// They are separate declarations only because service.proto already imports
// endpoint.proto (a Service carries its endpoints), so referencing
// Service.Protocol from ServiceEndpoint would be a circular import. Every
// conversion between them therefore relies on the values agreeing, and nothing
// in the proto system enforces that — adding a value to one and not the other
// compiles cleanly and mistranslates at runtime.
//
// If this ever fails, do not "fix" it by editing a number: unify the enums.
func TestPortProtocolMatchesServiceProtocol(t *testing.T) {
	assert.EqualValues(t, registryv1.Service_PROTOCOL_UNSPECIFIED, registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED)
	assert.EqualValues(t, registryv1.Service_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_HTTP)
	assert.EqualValues(t, registryv1.Service_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_TCP)
	assert.EqualValues(t, registryv1.Service_PROTOCOL_UDP, registryv1.PortProtocol_PORT_PROTOCOL_UDP)

	// And the same number of values, so a new one cannot be added to only one
	// side without this failing.
	assert.Len(t, registryv1.Service_Protocol_name, len(registryv1.PortProtocol_name),
		"the two enums must stay in step; add the value to both or unify them")
}

// TestElectPortClass: the one election of a port's class, the registrar's and
// the node agent's: the class most endpoints declare, ties to HTTP, then raw
// TCP, then UDP, and nothing for no vote.
func TestElectPortClass(t *testing.T) {
	h, tcp, udp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_UDP
	for name, tc := range map[string]struct {
		votes map[registryv1.PortProtocol]int
		want  registryv1.PortProtocol
	}{
		"no vote":                 {nil, registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED},
		"one class":               {map[registryv1.PortProtocol]int{udp: 1}, udp},
		"the most votes":          {map[registryv1.PortProtocol]int{h: 1, tcp: 2}, tcp},
		"the most votes, UDP":     {map[registryv1.PortProtocol]int{h: 1, tcp: 2, udp: 3}, udp},
		"a tie goes to HTTP":      {map[registryv1.PortProtocol]int{h: 2, tcp: 2, udp: 2}, h},
		"a tie goes to TCP":       {map[registryv1.PortProtocol]int{h: 1, tcp: 2, udp: 2}, tcp},
		"a class with zero votes": {map[registryv1.PortProtocol]int{h: 0, udp: 1}, udp},
	} {
		if got := ElectPortClass(tc.votes); got != tc.want {
			t.Errorf("%s: elected %v, want %v", name, got, tc.want)
		}
	}
}
