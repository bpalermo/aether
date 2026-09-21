package cache

import (
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
)

func epWithPorts(ip string, primary uint32, pp map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{Ip: ip, Port: primary, PortProtocols: pp}
}

const (
	ppHTTP = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	ppTCP  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
)

// TestDeriveTCPPorts covers the map the capture chains are built from
// (proposal 037). Classification comes from the ENDPOINTS, not from the mesh
// Service's app-protocol annotation — the annotation is the registrar's
// projection of a per-pod fact, and #878 is what happens when the two copies
// disagree.
func TestDeriveTCPPorts(t *testing.T) {
	tests := []struct {
		name string
		in   map[string][]*registryv1.ServiceEndpoint
		want map[string][]uint32
	}{
		{
			name: "no TCP ports: nothing derived",
			in: map[string][]*registryv1.ServiceEndpoint{
				"ns/http-only": {epWithPorts("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP})},
			},
			want: map[string][]uint32{},
		},
		{
			name: "the PRIMARY TCP port is excluded: the portless floor already reaches it",
			in: map[string][]*registryv1.ServiceEndpoint{
				"ns/tcp-only": {epWithPorts("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: ppTCP})},
			},
			want: map[string][]uint32{},
		},
		{
			name: "a non-primary TCP port is derived",
			in: map[string][]*registryv1.ServiceEndpoint{
				"ns/mixed": {epWithPorts("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP})},
			},
			want: map[string][]uint32{"ns/mixed": {9000}},
		},
		{
			name: "several, sorted and de-duplicated across endpoints",
			in: map[string][]*registryv1.ServiceEndpoint{
				"ns/multi": {
					epWithPorts("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP, 5432: ppTCP}),
					epWithPorts("10.0.0.2", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 5432: ppTCP}),
				},
			},
			want: map[string][]uint32{"ns/multi": {5432, 9000}},
		},
		{
			name: "an endpoint with no port_protocols (written by an older agent) derives nothing",
			in: map[string][]*registryv1.ServiceEndpoint{
				"ns/legacy": {epWithPorts("10.0.0.1", 9000, nil)},
			},
			want: map[string][]uint32{},
		},
		{
			name: "a service with no endpoints is skipped",
			in:   map[string][]*registryv1.ServiceEndpoint{"ns/empty": {}},
			want: map[string][]uint32{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, deriveTCPPorts(tt.in))
		})
	}
}

// TestDeriveTCPPortsIsStableUnderEndpointChurn is proposal 037 Risk 4.
//
// Regenerating a per-pod capture listener DRAINS that listener's connections,
// and a registry reload happens on every pod ADD/DEL anywhere on the node. So
// the regeneration trigger must compare the DERIVED port set, not the fact
// that a reload occurred — otherwise ordinary churn drops connections.
//
// Two reloads differing only in endpoint IPs must therefore produce an
// identical slice, and equalTCPEntries must call that unchanged.
func TestDeriveTCPPortsIsStableUnderEndpointChurn(t *testing.T) {
	before := map[string][]*registryv1.ServiceEndpoint{
		"ns/mixed": {
			epWithPorts("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP}),
			epWithPorts("10.0.0.2", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP}),
		},
	}
	// Same declared ports, entirely different pods — a rollout.
	after := map[string][]*registryv1.ServiceEndpoint{
		"ns/mixed": {
			epWithPorts("10.0.0.7", 8080, map[uint32]registryv1.PortProtocol{9000: ppTCP, 8080: ppHTTP}),
			epWithPorts("10.0.0.8", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP}),
			epWithPorts("10.0.0.9", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP}),
		},
	}

	assert.Equal(t, deriveTCPPorts(before), deriveTCPPorts(after),
		"endpoint churn must not change the derived port set, or every pod ADD/DEL drains capture listeners")

	entriesBefore := []captureTCPEntry{{serviceName: "ns/mixed", clusterIP: "10.96.0.1", tcpPorts: deriveTCPPorts(before)["ns/mixed"]}}
	entriesAfter := []captureTCPEntry{{serviceName: "ns/mixed", clusterIP: "10.96.0.1", tcpPorts: deriveTCPPorts(after)["ns/mixed"]}}
	assert.True(t, equalTCPEntries(entriesBefore, entriesAfter),
		"the change detector must agree, or the stability above buys nothing")

	// ...and a REAL change must still be detected, or the comparison is
	// decoration: a port set that can never differ would never regenerate.
	entriesNewPort := []captureTCPEntry{{serviceName: "ns/mixed", clusterIP: "10.96.0.1", tcpPorts: []uint32{5432, 9000}}}
	assert.False(t, equalTCPEntries(entriesBefore, entriesNewPort),
		"adding a declared TCP port MUST regenerate the listeners")
}
