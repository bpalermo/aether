package proxy

import (
	"testing"

	meshconst "aethermesh.dev/common/constants/mesh"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainsByPort indexes filter chains by their destination_port match (0 = none).
func chainsByPort(chains []*listenerv3.FilterChain) map[uint32][]*listenerv3.FilterChain {
	out := map[uint32][]*listenerv3.FilterChain{}
	for _, c := range chains {
		var p uint32
		if dp := c.GetFilterChainMatch().GetDestinationPort(); dp != nil {
			p = dp.GetValue()
		}
		out[p] = append(out[p], c)
	}
	return out
}

// hasSNIChainAt reports whether some chain matching exactly `port` carries a
// server_names match. That is the question #911 turns on: it is not enough for
// an SNI chain to EXIST, it has to exist at the port the client dials.
func hasSNIChainAt(chains []*listenerv3.FilterChain, port uint32) bool {
	for _, c := range chainsByPort(chains)[port] {
		if len(c.GetFilterChainMatch().GetServerNames()) > 0 {
			return true
		}
	}
	return false
}

// TestTLSRouteChainsSurvivePortQualifiedFloor is the regression test for #911.
//
// Envoy resolves destination_port FIRST and server_names third, so an SNI chain
// matching only prefix_ranges+server_names loses outright to a port-qualified
// floor chain — it never reaches the tier where its SNI would win. Proposal 037
// introduced those qualified chains, and from that commit a TLSRoute on a
// TCP-primary service was Accepted, produced correct-looking config, and was
// inert on both mesh spellings. Nothing failed; that is why it went unnoticed.
//
// The assertion is deliberately on the MATCH CRITERIA rather than on validity:
// the generated config was always valid, it was only wrongly ordered, so
// envoy --mode validate cannot catch this class on its own.
func TestTLSRouteChainsSurvivePortQualifiedFloor(t *testing.T) {
	const (
		clusterIP = "10.96.0.77"
		primary   = 9000
		sni       = "alpha.example.com"
	)

	svc := CaptureTCPService{
		ClusterName:  "tls-svc.aether-test.aether.internal",
		ClusterIP:    clusterIP,
		PrimaryIsTCP: true,
		PrimaryPort:  primary,
		TLSRouteRules: []L4ServiceRoute{{
			SNIHostnames: []string{sni},
			Backends:     []L4Backend{{Service: "aether-test/tls-svc", Cluster: "tcp:tls-svc.aether-test.aether.internal", Weight: 1}},
		}},
	}

	tls := BuildCaptureTLSRouteFilterChains(svc, svc.TLSRouteRules, "spiffe://aether.internal/ns/t/sa/c")
	require.NotEmpty(t, tls, "fixture produced no TLS chains; the test would be vacuous")

	got := qualifyTLSChainsByClaimedPorts(svc, tls)

	// The two sanctioned raw-TCP spellings for a TCP-primary service. Before the
	// fix BOTH of these were claimed by cap_tcp_* floor chains and the SNI chain
	// was unreachable on them.
	for _, port := range []uint32{meshconst.ProxyTCPOutboundPort, primary} {
		assert.True(t, hasSNIChainAt(got, port),
			"no server_names chain at :%d — a port-qualified floor chain outranks the SNI chain "+
				"on Envoy's first tier, so the TLSRoute is inert on that spelling (#911)", port)
	}

	// The unqualified chain must survive: it is what serves every port nothing
	// claims, and it was the ONLY working spelling before this fix.
	assert.True(t, hasSNIChainAt(got, 0),
		"the portless SNI chain was dropped; ports that no floor chain claims would stop routing")

	// Chain names must stay unique or Envoy rejects the listener outright.
	seen := map[string]bool{}
	for _, c := range got {
		require.False(t, seen[c.GetName()], "duplicate filter chain name %q", c.GetName())
		seen[c.GetName()] = true
	}
}

// TestClaimedTCPPortsCoversBothProducers pins the port set against the two
// places that emit destination_port, so adding a third without updating this
// silently reopens #911 on that port.
func TestClaimedTCPPortsCoversBothProducers(t *testing.T) {
	t.Run("tcp-primary: mesh port and primary are claimed", func(t *testing.T) {
		got := claimedTCPPorts(CaptureTCPService{PrimaryIsTCP: true, PrimaryPort: 9000})
		assert.ElementsMatch(t, []uint32{meshconst.ProxyTCPOutboundPort, 9000}, got)
	})

	t.Run("http-primary: its raw-TCP ports are claimed, the floor ports are not", func(t *testing.T) {
		// design (d) delivers every mesh Service here, so an HTTP-primary
		// service also emits destination_port chains — for its TCP ports only.
		got := claimedTCPPorts(CaptureTCPService{PrimaryIsTCP: false, PrimaryPort: 8080, TCPPorts: []uint32{9000}})
		assert.ElementsMatch(t, []uint32{9000}, got)
	})

	t.Run("no duplicate when a declared TCP port collides with the mesh port", func(t *testing.T) {
		got := claimedTCPPorts(CaptureTCPService{
			PrimaryIsTCP: true,
			PrimaryPort:  meshconst.ProxyTCPOutboundPort,
			TCPPorts:     []uint32{meshconst.ProxyTCPOutboundPort},
		})
		assert.Equal(t, []uint32{meshconst.ProxyTCPOutboundPort}, got,
			"a duplicated port yields two chains with the same name and Envoy rejects the listener")
	})

	t.Run("zero ports are never claimed", func(t *testing.T) {
		got := claimedTCPPorts(CaptureTCPService{PrimaryIsTCP: true, PrimaryPort: 0})
		assert.Equal(t, []uint32{meshconst.ProxyTCPOutboundPort}, got)
		assert.NotContains(t, got, uint32(0), "port 0 would collide with the portless spelling")
	})
}
