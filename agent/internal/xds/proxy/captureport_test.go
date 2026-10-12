package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildCaptureTCPPortFilterChain covers the per-port TCP capture chain
// (proposal 037).
//
// The destination_port match is the whole mechanism: Envoy evaluates it ahead
// of prefix_ranges, and the capture listener's original_dst listener filter
// makes it the port the client actually dialed. A chain
// matching only the ClusterIP would outrank the HCM catch-all's
// application_protocols match and swallow the service's HTTP traffic — which
// is exactly why a service could not be both protocols before.
func TestBuildCaptureTCPPortFilterChain(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:mixed.aether-test.aether.internal",
		ClusterIP:   "10.96.0.60", LegacyTCPPort: 9000,
	}

	fc := buildCaptureTCPPortFilterChain(svc, 9000, "spiffe://example.org/ns/default/sa/client")
	require.NotNil(t, fc)

	assert.Equal(t, "cap_tcp_tcp:mixed.aether-test.aether.internal_9000", fc.GetName())

	m := fc.GetFilterChainMatch()
	require.NotNil(t, m)
	require.Len(t, m.GetPrefixRanges(), 1)
	assert.Equal(t, "10.96.0.60", m.GetPrefixRanges()[0].GetAddressPrefix())
	assert.Equal(t, uint32(32), m.GetPrefixRanges()[0].GetPrefixLen().GetValue())
	require.NotNil(t, m.GetDestinationPort(), "without the port match this chain swallows the VIP's HTTP traffic")
	assert.Equal(t, uint32(9000), m.GetDestinationPort().GetValue())

	// It must target the PORT's own cluster, not the floor cluster: the floor
	// forwards to the pod's PRIMARY port, so routing :9000 through it would
	// deliver that traffic to :8080.
	assert.Equal(t, "tcp:mixed.aether-test.aether.internal:9000",
		TCPPortClusterName(svc.ClusterName, 9000))
}

func TestBuildCaptureTCPPortFilterChain_Rejects(t *testing.T) {
	good := CaptureTCPService{ClusterName: "tcp:svc.ns.aether.internal", ClusterIP: "10.96.0.60", LegacyTCPPort: 9000}
	tests := []struct {
		name string
		svc  CaptureTCPService
		port uint32
	}{
		{name: "no ClusterIP", svc: CaptureTCPService{ClusterName: good.ClusterName}, port: 9000},
		{name: "no ClusterName", svc: CaptureTCPService{ClusterIP: good.ClusterIP}, port: 9000},
		{name: "unparseable ClusterIP", svc: CaptureTCPService{ClusterName: good.ClusterName, ClusterIP: "not-an-ip", LegacyTCPPort: 9000}, port: 9000},
		{name: "port zero", svc: good, port: 0},
		{name: "port out of range", svc: good, port: 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, buildCaptureTCPPortFilterChain(tt.svc, tt.port, "spiffe://example.org/ns/a/sa/b"),
				"a malformed input must yield no chain rather than a chain that matches nothing")
		})
	}
}

// TestGenerateCaptureListener_HTTPPrimaryGetsNoPortlessChain is proposal 037
// design (d).
//
// Every mesh Service with a VIP is now delivered to the capture listener, not
// only the non-HTTP ones — an HTTP-primary service can still serve raw-TCP
// ports, and those need chains. But it must NOT get the PORTLESS /32 floor
// chain: filter-chain match precedence puts destination-IP above
// application-protocol, so that chain would intercept every HTTP request to the
// VIP before the HCM catch-all could see it.
//
// Its TCP ports get destination_port-qualified chains instead, which Envoy
// evaluates ahead of prefix_ranges and which therefore leave HTTP alone. That
// distinction is the whole reason a service can now be both protocols.
func TestGenerateCaptureListener_HTTPPrimaryGetsNoPortlessChain(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}

	svcs := []CaptureTCPService{{
		ClusterName: "tcp:mixed.aether-test.aether.internal",
		ClusterIP:   "10.96.1.30",
		HTTPPorts:   []uint32{8080},
		TCPPorts:    []uint32{9000},
		// LegacyTCPPort zero: the legacy target is HTTP.
	}}

	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test",
		15001, "aether.internal", false, svcs, nil, true, nil)
	require.NoError(t, err)

	var portless, perPort int
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if len(m.GetPrefixRanges()) == 0 {
			continue // the HCM catch-all
		}
		if m.GetDestinationPort() == nil {
			portless++
			continue
		}
		perPort++
	}

	assert.Zero(t, portless,
		"an HTTP-primary service must get NO portless /32 chain — it would swallow the VIP's HTTP traffic")
	assert.Equal(t, 1, perPort,
		"but its raw-TCP port must still get a destination_port-qualified chain")
}

// TestGenerateCaptureListener_TCPPrimaryKeepsPortlessChain: the other side of
// the same gate, as it stands since chart 3.0.0. A TCP-primary service used to
// keep a portless floor chain, on which any port of its VIP reached the floor.
// It has none now: it is reached on its own port and on :18082, and a
// connection to any other port of its VIP matches no chain of the service.
// (The name is kept so the history of the test is one line of `git log`.)
func TestGenerateCaptureListener_TCPPrimaryKeepsPortlessChain(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}

	svcs := []CaptureTCPService{{
		ClusterName:   "tcp:echo-tcp.aether-test.aether.internal",
		ClusterIP:     "10.96.1.40",
		LegacyTCPPort: 9000,
	}}

	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test",
		15001, "aether.internal", false, svcs, nil, true, nil)
	require.NoError(t, err)

	var ports []uint32
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if len(m.GetPrefixRanges()) == 0 {
			continue
		}
		assert.NotNil(t, m.GetDestinationPort(), "chain %s claims the VIP on every port", fc.GetName())
		ports = append(ports, m.GetDestinationPort().GetValue())
	}
	assert.ElementsMatch(t, []uint32{9000, 18082}, ports,
		"a TCP-primary service is reached on its own port and on the TCP mesh port, and on nothing else")
}

// TestAnyPortShimIsCountedSeparately was the gate the removal of the any-port
// chain depended on: the shim had a stat prefix of its own
// (cap_tcp_anyport_<cluster>), so its counter could tell "someone relies on
// the deprecated behaviour" from "someone used a supported spelling".
//
// The chain was removed in chart 3.0.0. What the test holds now is the other
// half of the same sentence: the two spellings that survive are each counted
// under their own chain's name, and no tcp_proxy of the listener counts under
// the any-port prefix, so a dashboard or a harness that still reads it reads
// a series that does not exist rather than one that is silently zero for
// another reason.
func TestAnyPortShimIsCountedSeparately(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	svcs := []CaptureTCPService{{
		ClusterName:   "tcp:echo-tcp.aether-test.aether.internal",
		ClusterIP:     "10.96.1.50",
		LegacyTCPPort: 9000,
	}}

	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test",
		15001, "aether.internal", false, svcs, nil, true, nil)
	require.NoError(t, err)

	prefixes := map[string]uint32{} // stat prefix -> destination_port (0 = portless)
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			tc := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(tc) != nil {
				continue
			}
			prefixes[tc.GetStatPrefix()] = fc.GetFilterChainMatch().GetDestinationPort().GetValue()
		}
	}

	// Exactly the two surviving spellings, each on its own port, each under
	// its own prefix. A third entry would be a chain nobody asked for; the
	// any-port prefix in particular would be the removed chain coming back.
	assert.Equal(t, map[string]uint32{
		"cap_tcp_tcp:echo-tcp.aether-test.aether.internal_18082": 18082,
		"cap_tcp_tcp:echo-tcp.aether-test.aether.internal_9000":  9000,
	}, prefixes)
	assert.NotContains(t, prefixes, "cap_tcp_anyport_tcp:echo-tcp.aether-test.aether.internal")
}
