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
// of prefix_ranges, and use_original_dst (already set on the capture listener)
// makes it the port the client actually dialed before the REDIRECT. A chain
// matching only the ClusterIP would outrank the HCM catch-all's
// application_protocols match and swallow the service's HTTP traffic — which
// is exactly why a service could not be both protocols before.
func TestBuildCaptureTCPPortFilterChain(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:mixed.aether-test.aether.internal",
		ClusterIP:   "10.96.0.60", PrimaryIsTCP: true,
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
	good := CaptureTCPService{ClusterName: "tcp:svc.ns.aether.internal", ClusterIP: "10.96.0.60", PrimaryIsTCP: true}
	tests := []struct {
		name string
		svc  CaptureTCPService
		port uint32
	}{
		{name: "no ClusterIP", svc: CaptureTCPService{ClusterName: good.ClusterName}, port: 9000},
		{name: "no ClusterName", svc: CaptureTCPService{ClusterIP: good.ClusterIP}, port: 9000},
		{name: "unparseable ClusterIP", svc: CaptureTCPService{ClusterName: good.ClusterName, ClusterIP: "not-an-ip", PrimaryIsTCP: true}, port: 9000},
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
		ClusterName:  "tcp:mixed.aether-test.aether.internal",
		ClusterIP:    "10.96.1.30",
		TCPPorts:     []uint32{9000},
		PrimaryIsTCP: false, // HTTP primary
	}}

	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test",
		15001, "aether.internal", false, svcs, true, nil)
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
// the same gate. A TCP-primary service keeps the portless floor, which is what
// makes any port to its VIP reach the floor — today's behaviour, unchanged.
func TestGenerateCaptureListener_TCPPrimaryKeepsPortlessChain(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}

	svcs := []CaptureTCPService{{
		ClusterName:  "tcp:echo-tcp.aether-test.aether.internal",
		ClusterIP:    "10.96.1.40",
		PrimaryIsTCP: true,
	}}

	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test",
		15001, "aether.internal", false, svcs, true, nil)
	require.NoError(t, err)

	var portless int
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if len(m.GetPrefixRanges()) > 0 && m.GetDestinationPort() == nil {
			portless++
		}
	}
	assert.Equal(t, 1, portless, "a TCP-primary service keeps its portless floor chain")
}

// TestAnyPortShimIsCountedSeparately is the gate Phase 4 depends on.
//
// Phase 4 proposes deleting the portless floor chain — the only step in
// proposal 037 that changes what an existing client observes — and it is gated
// on evidence that no client observes it: the shim's own counter reading zero
// across a full release on talos.
//
// That evidence is only obtainable if the shim has a stat prefix of its own. If
// it shared one with the supported spellings (:18082 and the primary port), the
// counter could never distinguish "someone relies on the deprecated behaviour"
// from "someone used a supported spelling", and the removal would be a guess.
func TestAnyPortShimIsCountedSeparately(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	svcs := []CaptureTCPService{{
		ClusterName:  "tcp:echo-tcp.aether-test.aether.internal",
		ClusterIP:    "10.96.1.50",
		PrimaryIsTCP: true,
		PrimaryPort:  9000,
	}}

	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test",
		15001, "aether.internal", false, svcs, true, nil)
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

	shim := "cap_tcp_anyport_tcp:echo-tcp.aether-test.aether.internal"
	require.Contains(t, prefixes, shim, "the shim must have its own stat prefix, or Phase 4 has no evidence to act on")
	assert.Zero(t, prefixes[shim], "the shim is PORTLESS: it catches what no destination_port chain claimed")

	// The two spellings that survive Phase 4 are counted apart from it.
	assert.Equal(t, uint32(18082), prefixes["cap_tcp_tcp:echo-tcp.aether-test.aether.internal_18082"])
	assert.Equal(t, uint32(9000), prefixes["cap_tcp_tcp:echo-tcp.aether-test.aether.internal_9000"],
		"the service's own primary port keeps working after the shim is removed")
}
