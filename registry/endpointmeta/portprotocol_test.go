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

	// And the same number of values, so a new one cannot be added to only one
	// side without this failing.
	assert.Len(t, registryv1.Service_Protocol_name, len(registryv1.PortProtocol_name),
		"the two enums must stay in step; add the value to both or unify them")
}
