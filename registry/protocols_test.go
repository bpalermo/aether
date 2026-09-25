package registry

import (
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServedProtocolsCoversEnum is the whole point of the list existing: a
// protocol added to the proto and not to ServedProtocols must fail here rather
// than vanish silently at five call sites.
//
// It reads Service_Protocol_name (generated) rather than a second hand-written
// list, so it cannot drift the way the thing it guards drifted.
func TestServedProtocolsCoversEnum(t *testing.T) {
	served := map[registryv1.Service_Protocol]bool{}
	for _, p := range ServedProtocols {
		assert.False(t, served[p], "duplicate protocol %v in ServedProtocols", p)
		served[p] = true
	}

	for value, name := range registryv1.Service_Protocol_name {
		p := registryv1.Service_Protocol(value)
		if p == registryv1.Service_PROTOCOL_UNSPECIFIED {
			assert.False(t, served[p], "UNSPECIFIED is not a registrable protocol")
			continue
		}
		assert.True(t, served[p],
			"%s is in the proto but not in ServedProtocols: services registered under it "+
				"would be invisible to the agent watch, the mesh VIP generator, the xDS "+
				"cluster build and the ghost sweep, with no error anywhere", name)
	}

	// Cardinality both ways, so a value REMOVED from the proto also fails here
	// rather than leaving a stale entry that reads as a supported protocol.
	require.Len(t, ServedProtocols, len(registryv1.Service_Protocol_name)-1,
		"ServedProtocols must hold every enum value except UNSPECIFIED")
}

// TestServedProtocolsOrderIsStable pins the order the mesh Service generator's
// no-clobber convergence depends on: if one service name ever appeared under two
// protocols, the first one iterated wins, and that winner must not change
// because someone sorted the list.
func TestServedProtocolsOrderIsStable(t *testing.T) {
	assert.Equal(t, []registryv1.Service_Protocol{
		registryv1.Service_PROTOCOL_HTTP,
		registryv1.Service_PROTOCOL_TCP,
		registryv1.Service_PROTOCOL_UDP,
	}, ServedProtocols)
}
