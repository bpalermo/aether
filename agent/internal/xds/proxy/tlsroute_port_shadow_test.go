package proxy

import (
	"testing"

	meshconst "aethermesh.dev/common/constants/mesh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTLSRouteChainsSurvivePortQualifiedFloor is the regression test for #911.
//
// With filter_chain_match, Envoy resolved destination_port FIRST and
// server_names third, so an SNI chain matching only prefix_ranges+server_names
// lost outright to a port-qualified floor chain: it never reached the tier
// where its SNI would win. From proposal 037 a TLSRoute on a TCP-primary
// service was Accepted, produced correct-looking config, and was inert on
// both mesh spellings. Nothing failed; that is why it went unnoticed.
//
// The listener selects chains with a matching tree now, which tests the
// server name under each raw-TCP port of the address. The assertion stays on
// SELECTION, not on validity: the broken config was always valid.
func TestTLSRouteChainsSurvivePortQualifiedFloor(t *testing.T) {
	const (
		clusterIP = "10.96.0.77"
		primary   = 9000
		sni       = "alpha.example.com"
	)
	svc := CaptureTCPService{
		ClusterName:   "tls-svc.aether-test.aether.internal",
		ClusterIP:     clusterIP,
		LegacyTCPPort: primary,
		TLSRouteRules: []L4ServiceRoute{{
			SNIHostnames: []string{sni},
			Backends:     []L4Backend{{Service: "aether-test/tls-svc", Cluster: "tcp:tls-svc.aether-test.aether.internal", Weight: 1}},
		}},
	}
	l := perPortListener(t, []CaptureTCPService{svc}, true)
	const tlsChain = "cap_tls_tls-svc.aether-test.aether.internal_0"
	require.NotNil(t, chainByName(l.GetFilterChains(), tlsChain), "fixture produced no TLS chain; the test would be vacuous")

	// The two sanctioned raw-TCP spellings of a TCP-primary service, and a
	// port nothing claims.
	for _, port := range []uint32{meshconst.ProxyL4OutboundPort, primary, 7777} {
		assert.Equal(t, tlsChain, sel(t, l, tlsConn(clusterIP, port, sni)),
			"TLS with the route's name to :%d must reach the TLSRoute (#911)", port)
	}
	// And the floor still serves the raw-TCP ports for anything else.
	assert.Equal(t, "cap_tcp_tls-svc.aether-test.aether.internal_9000", sel(t, l, rawConn(clusterIP, primary)))
	assert.Equal(t, "cap_tcp_tls-svc.aether-test.aether.internal_18082", sel(t, l, tlsConn(clusterIP, meshconst.ProxyL4OutboundPort, "other.example.com")))
}

// TestClaimedTCPPortsCoversBothProducers pins the raw-TCP ports a service is
// reached on against the two places that build a port's chain (the declared
// raw-TCP ports, and the legacy target's own port with :18082).
func TestClaimedTCPPortsCoversBothProducers(t *testing.T) {
	reach := func(svc CaptureTCPService) []uint32 {
		svc.ClusterName, svc.ClusterIP = "tcp:c.t.aether.internal", "10.96.0.5"
		return rawReach(t, perPortListener(t, []CaptureTCPService{svc}, true), svc.ClusterIP, 9100)
	}
	t.Run("tcp-primary: mesh port and primary are claimed", func(t *testing.T) {
		assert.Equal(t, []uint32{9000, meshconst.ProxyL4OutboundPort}, reach(CaptureTCPService{LegacyTCPPort: 9000}))
	})
	t.Run("http-primary: its raw-TCP ports are claimed, the floor ports are not", func(t *testing.T) {
		assert.Equal(t, []uint32{9000}, reach(CaptureTCPService{HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}}))
	})
	t.Run("no duplicate when a declared TCP port collides with the mesh port", func(t *testing.T) {
		svc := CaptureTCPService{ClusterName: "tcp:c.t.aether.internal", ClusterIP: "10.96.0.5", LegacyTCPPort: meshconst.ProxyL4OutboundPort, TCPPorts: []uint32{meshconst.ProxyL4OutboundPort}}
		l := perPortListener(t, []CaptureTCPService{svc}, true) // the gate refuses a name twice
		assert.Equal(t, []uint32{meshconst.ProxyL4OutboundPort}, rawReach(t, l, svc.ClusterIP))
	})
	t.Run("zero ports are never claimed", func(t *testing.T) {
		// A zero LegacyTCPPort means "the legacy target is not raw TCP"
		// (proposal 043), so it claims neither itself nor the TCP mesh port;
		// a zero among the declared ports is dropped.
		assert.Equal(t, []uint32{9000}, reach(CaptureTCPService{LegacyTCPPort: 0, TCPPorts: []uint32{0, 9000}}))
		assert.Empty(t, reach(CaptureTCPService{}), "a service with no raw-TCP port claims nothing")
	})
}
