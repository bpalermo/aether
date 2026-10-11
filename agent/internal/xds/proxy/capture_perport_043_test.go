package proxy

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Proposal 043, "The chains of a capture listener": the port a client DIALLED
// selects the chain. These tests hold the listener's shape against the silent
// wrong answers the layout exists to remove: HTTP delivered to another port's
// table, HTTP on a port number some other service declares raw TCP leaving
// through the passthrough, and a per-address chain swallowing a service's own
// HTTP.

const perPortSource = "spiffe://aether.internal/ns/default/sa/client"

func perPortPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{Name: "p1", Namespace: "default", NetworkNamespace: "/var/run/netns/p1"}
}

func perPortListener(t *testing.T, svcs []CaptureTCPService, httpPorts []uint32, withPassthrough bool) *listenerv3.Listener {
	t.Helper()
	l, err := GenerateCaptureListener(perPortPod(), perPortSource, meshconst.ProxyCapturePort, "aether.internal", false, svcs, httpPorts, withPassthrough, nil)
	require.NoError(t, err)
	return l
}

// matchKey renders every criterion of a filter-chain match that Envoy tells two
// chains apart by. Two chains with the same key are a listener Envoy rejects
// whole ("multiple filter chains with the same matching rules").
func matchKey(fc *listenerv3.FilterChain) string {
	m := fc.GetFilterChainMatch()
	prefixes := make([]string, 0, len(m.GetPrefixRanges()))
	for _, pr := range m.GetPrefixRanges() {
		prefixes = append(prefixes, fmt.Sprintf("%s/%d", pr.GetAddressPrefix(), pr.GetPrefixLen().GetValue()))
	}
	sort.Strings(prefixes)
	sni := slices.Clone(m.GetServerNames())
	sort.Strings(sni)
	alpn := slices.Clone(m.GetApplicationProtocols())
	sort.Strings(alpn)
	port := "any"
	if m.GetDestinationPort() != nil {
		port = fmt.Sprint(m.GetDestinationPort().GetValue())
	}
	return fmt.Sprintf("port=%s prefix=%v sni=%v transport=%q alpn=%v", port, prefixes, sni, m.GetTransportProtocol(), alpn)
}

// duplicateMatches returns the match keys more than one chain carries, with
// the names of the chains that share them, and the names used more than once.
func duplicateMatches(chains []*listenerv3.FilterChain) (dups []string) {
	byKey := map[string][]string{}
	byName := map[string]int{}
	for _, fc := range chains {
		byKey[matchKey(fc)] = append(byKey[matchKey(fc)], fc.GetName())
		byName[fc.GetName()]++
	}
	for k, names := range byKey {
		if len(names) > 1 {
			dups = append(dups, fmt.Sprintf("match {%s} shared by %v", k, names))
		}
	}
	for n, c := range byName {
		if c > 1 {
			dups = append(dups, fmt.Sprintf("name %q used %d times", n, c))
		}
	}
	sort.Strings(dups)
	return dups
}

func hcmRouteName(t *testing.T, fc *listenerv3.FilterChain) string {
	t.Helper()
	last := fc.GetFilters()[len(fc.GetFilters())-1]
	var hcm http_connection_managerv3.HttpConnectionManager
	require.NoError(t, last.GetTypedConfig().UnmarshalTo(&hcm), "chain %s does not end in an HCM", fc.GetName())
	return hcm.GetRds().GetRouteConfigName()
}

func chainTCPProxy(fc *listenerv3.FilterChain) *tcp_proxyv3.TcpProxy {
	for _, f := range fc.GetFilters() {
		tc := &tcp_proxyv3.TcpProxy{}
		if f.GetTypedConfig() != nil && f.GetTypedConfig().UnmarshalTo(tc) == nil {
			return tc
		}
	}
	return nil
}

// TestCaptureHTTPChains_OnePerPortNumber: one cap_http_<P> chain per HTTP port
// number, matched on destination port P, cleartext and sniffed HTTP, serving
// the route table of the SAME name; and cap_http_other with no destination
// port. A chain whose table name differed from its own would send port P's
// requests through another port's table.
func TestCaptureHTTPChains_OnePerPortNumber(t *testing.T) {
	ports := []uint32{80, 8080, 9090, 18081}
	l := perPortListener(t, nil, ports, true)

	require.Len(t, l.GetFilterChains(), len(ports)+1, "one chain per HTTP port number, plus cap_http_other")
	for _, p := range ports {
		name := fmt.Sprintf("cap_http_%d", p)
		fc := chainByName(l.GetFilterChains(), name)
		require.NotNil(t, fc, name)
		m := fc.GetFilterChainMatch()
		require.NotNil(t, m.GetDestinationPort(), "%s must match its destination port", name)
		assert.Equal(t, p, m.GetDestinationPort().GetValue(), name)
		assert.Equal(t, "raw_buffer", m.GetTransportProtocol(), name)
		assert.Equal(t, []string{"http/1.0", "http/1.1", "h2c"}, m.GetApplicationProtocols(), name)
		assert.Empty(t, m.GetPrefixRanges(), "%s is per port NUMBER, not per service address", name)
		assert.Equal(t, name, hcmRouteName(t, fc), "the chain of port %d must serve the table of port %d", p, p)
	}
	other := chainByName(l.GetFilterChains(), CaptureHTTPOtherRouteName)
	require.NotNil(t, other)
	assert.Nil(t, other.GetFilterChainMatch().GetDestinationPort(), "cap_http_other names no destination port")
	assert.Equal(t, "raw_buffer", other.GetFilterChainMatch().GetTransportProtocol())
	assert.Equal(t, []string{"http/1.0", "http/1.1", "h2c"}, other.GetFilterChainMatch().GetApplicationProtocols())
	assert.Equal(t, CaptureHTTPOtherRouteName, hcmRouteName(t, other))
	assert.Empty(t, duplicateMatches(l.GetFilterChains()))

	// Negative control: a port that is NOT in the set has no chain.
	assert.Nil(t, chainByName(l.GetFilterChains(), "cap_http_7777"))
	// And every route table named is named by exactly one chain.
	routes := map[string]int{}
	for _, fc := range l.GetFilterChains() {
		routes[hcmRouteName(t, fc)]++
	}
	for name, n := range routes {
		assert.Equal(t, 1, n, "route table %s", name)
	}
}

// TestCaptureHTTPChains_PortSetIsNormalised: zeros, values above 65535, the TCP
// mesh port and duplicates never become chains. A cap_http_18082 chain would
// tie with the scoped-mode blackhole chain and be shadowed by every raw-TCP
// legacy chain; a duplicate is a listener Envoy rejects whole.
func TestCaptureHTTPChains_PortSetIsNormalised(t *testing.T) {
	assert.Equal(t, []uint32{80, 8080}, normalizeHTTPPorts([]uint32{8080, 0, 80, 18082, 8080, 70000, 80}))
	assert.Empty(t, normalizeHTTPPorts(nil))

	for _, withPassthrough := range []bool{true, false} {
		l := perPortListener(t, nil, []uint32{8080, 0, 80, meshconst.ProxyL4OutboundPort, 8080, 70000}, withPassthrough)
		var names []string
		for _, fc := range l.GetFilterChains() {
			if strings.HasPrefix(fc.GetName(), CaptureHTTPRoutePrefix) {
				names = append(names, fc.GetName())
			}
		}
		assert.Equal(t, []string{"cap_http_80", "cap_http_8080", CaptureHTTPOtherRouteName}, names, "withPassthrough=%v", withPassthrough)
		assert.Empty(t, duplicateMatches(l.GetFilterChains()), "withPassthrough=%v", withPassthrough)
	}
}

// TestCaptureHTTPChains_ScopedMode: without the passthrough (the scoped shape
// only unit tests build) the per-port chains match on the destination port
// alone and cap_http_other is the catch-all, with no match at all.
func TestCaptureHTTPChains_ScopedMode(t *testing.T) {
	l := perPortListener(t, nil, []uint32{8080}, false)
	assert.Nil(t, l.GetDefaultFilterChain())

	p := chainByName(l.GetFilterChains(), "cap_http_8080")
	require.NotNil(t, p)
	assert.Equal(t, uint32(8080), p.GetFilterChainMatch().GetDestinationPort().GetValue())
	assert.Empty(t, p.GetFilterChainMatch().GetTransportProtocol())
	assert.Empty(t, p.GetFilterChainMatch().GetApplicationProtocols())

	other := chainByName(l.GetFilterChains(), CaptureHTTPOtherRouteName)
	require.NotNil(t, other)
	assert.Nil(t, other.GetFilterChainMatch(), "the scoped catch-all has no match criteria")
	require.NotNil(t, chainByName(l.GetFilterChains(), "cap_tcp_blackhole"))
	assert.Empty(t, duplicateMatches(l.GetFilterChains()))
}

// TestCaptureChains_NoDuplicateMatch runs the duplicate check over the shapes
// most likely to produce two chains with one match.
func TestCaptureChains_NoDuplicateMatch(t *testing.T) {
	tls := []L4ServiceRoute{{
		SNIHostnames: []string{"a.example.com"},
		Backends:     []L4Backend{{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1}},
	}}
	shapes := map[string]struct {
		svcs  []CaptureTCPService
		ports []uint32
	}{
		"legacy target IS the TCP mesh port": {
			svcs:  []CaptureTCPService{{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: meshconst.ProxyL4OutboundPort, AnyPort: true, TCPPorts: []uint32{meshconst.ProxyL4OutboundPort}}},
			ports: []uint32{80, 18081},
		},
		"legacy port repeated in TCPPorts": {
			svcs:  []CaptureTCPService{{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, AnyPort: true, TCPPorts: []uint32{9000, 9001}}},
			ports: []uint32{80, 18081},
		},
		"one number, HTTP for one service and raw TCP for another": {
			svcs: []CaptureTCPService{
				{ClusterName: "tcp:mixed.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
				{ClusterName: "tcp:nine.t.aether.internal", ClusterIP: "10.96.0.2", HTTPPorts: []uint32{9000}, TCPPorts: []uint32{8080}},
			},
			ports: []uint32{80, 8080, 9000, 18081},
		},
		"any-port service whose port is another service's HTTP number": {
			svcs: []CaptureTCPService{
				{ClusterName: "tcp:raw.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: 8080, AnyPort: true},
				{ClusterName: "tcp:web.t.aether.internal", ClusterIP: "10.96.0.2", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
			},
			ports: []uint32{80, 8080, 18081},
		},
		"TLSRoute on a mixed service and on a pure-TCP one": {
			svcs: []CaptureTCPService{
				{ClusterName: "tcp:mixed.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}, TLSRouteRules: tls},
				{ClusterName: "tcp:raw.t.aether.internal", ClusterIP: "10.96.0.2", LegacyTCPPort: 9000, AnyPort: true, TLSRouteRules: tls},
			},
			ports: []uint32{0, 80, 8080, 8080, 18081, meshconst.ProxyL4OutboundPort},
		},
		"two services, the same address by mistake, different ports": {
			svcs: []CaptureTCPService{
				{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
				{ClusterName: "tcp:b.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9001}},
			},
			ports: []uint32{80, 8080, 18081},
		},
	}
	for name, shape := range shapes {
		for _, withPassthrough := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/passthrough=%v", name, withPassthrough), func(t *testing.T) {
				l := perPortListener(t, shape.svcs, shape.ports, withPassthrough)
				assert.Empty(t, duplicateMatches(l.GetFilterChains()))
			})
		}
	}

	// Negative control: the helper does find a duplicate when there is one.
	l := perPortListener(t, nil, []uint32{8080}, true)
	twice := append(slices.Clone(l.GetFilterChains()), l.GetFilterChains()[0])
	assert.Len(t, duplicateMatches(twice), 2, "one shared match and one repeated name")
	sameMatch := []*listenerv3.FilterChain{
		{Name: "x", FilterChainMatch: l.GetFilterChains()[0].GetFilterChainMatch()},
		{Name: "y", FilterChainMatch: l.GetFilterChains()[0].GetFilterChainMatch()},
	}
	assert.Len(t, duplicateMatches(sameMatch), 1)
}

// TestCaptureChains_RawTCPAndHTTPShareAPortNumber is the measured finding the
// per-port layout exists for (proposal 043, layout L2): service A declares 9000
// as raw TCP, service B serves HTTP on 9000. Envoy matches the destination
// port first and does not backtrack, so with only a port-less HTTP chain, B's
// HTTP on 9000 left through the passthrough. Both chains must name 9000: A's
// with A's address, the HTTP one with none.
func TestCaptureChains_RawTCPAndHTTPShareAPortNumber(t *testing.T) {
	svcs := []CaptureTCPService{
		{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
	}
	l := perPortListener(t, svcs, []uint32{80, 8080, 9000, 18081}, true)

	raw := chainByName(l.GetFilterChains(), "cap_tcp_tcp:a.t.aether.internal_9000")
	require.NotNil(t, raw)
	assert.Equal(t, uint32(9000), raw.GetFilterChainMatch().GetDestinationPort().GetValue())
	require.Len(t, raw.GetFilterChainMatch().GetPrefixRanges(), 1)
	assert.Equal(t, "10.96.0.1", raw.GetFilterChainMatch().GetPrefixRanges()[0].GetAddressPrefix())
	assert.Equal(t, uint32(32), raw.GetFilterChainMatch().GetPrefixRanges()[0].GetPrefixLen().GetValue())
	require.NotNil(t, chainTCPProxy(raw))
	assert.Equal(t, "tcp:a.t.aether.internal:9000", chainTCPProxy(raw).GetCluster(),
		"the raw-TCP port's own cluster, never the floor (which forwards to another port)")

	http := chainByName(l.GetFilterChains(), "cap_http_9000")
	require.NotNil(t, http, "HTTP on port number 9000 must have a chain although A declares 9000 raw TCP")
	assert.Equal(t, uint32(9000), http.GetFilterChainMatch().GetDestinationPort().GetValue())
	assert.Empty(t, http.GetFilterChainMatch().GetPrefixRanges())

	// A has no chain that claims its ADDRESS on its HTTP port: its own HTTP
	// must reach cap_http_8080.
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if len(m.GetPrefixRanges()) == 0 {
			continue
		}
		if m.GetDestinationPort() == nil {
			t.Errorf("chain %s claims A's address on every port; it would swallow A's HTTP", fc.GetName())
			continue
		}
		assert.NotEqual(t, uint32(8080), m.GetDestinationPort().GetValue(), "chain %s claims A's address on its HTTP port", fc.GetName())
	}

	// Negative control: without the declaration there is no raw-TCP chain.
	bare := perPortListener(t, []CaptureTCPService{{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}}}, []uint32{9000}, true)
	assert.Nil(t, chainByName(bare.GetFilterChains(), "cap_tcp_tcp:a.t.aether.internal_9000"))
}

// TestCaptureChains_LegacyTCPTarget: a raw-TCP legacy target gets a chain on
// its own port and on the TCP mesh port, both to the floor cluster; a service
// with an HTTP legacy target gets neither (raw bytes to :18082 are never
// forwarded to an HTTP port).
func TestCaptureChains_LegacyTCPTarget(t *testing.T) {
	const cluster = "tcp:first.t.aether.internal"
	l := perPortListener(t, []CaptureTCPService{
		{ClusterName: cluster, ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, LegacyTCPPort: 9000, TCPPorts: []uint32{9000}},
		{ClusterName: "tcp:mixed.t.aether.internal", ClusterIP: "10.96.0.2", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
	}, []uint32{80, 8080, 18081}, true)

	for _, p := range []uint32{9000, meshconst.ProxyL4OutboundPort} {
		fc := chainByName(l.GetFilterChains(), fmt.Sprintf("cap_tcp_%s_%d", cluster, p))
		require.NotNil(t, fc, "port %d", p)
		assert.Equal(t, p, fc.GetFilterChainMatch().GetDestinationPort().GetValue())
		assert.Equal(t, "10.96.0.1", fc.GetFilterChainMatch().GetPrefixRanges()[0].GetAddressPrefix())
		assert.Equal(t, cluster, chainTCPProxy(fc).GetCluster(), "the legacy target is reached through the floor cluster")
		assert.Equal(t, fc.GetName(), chainTCPProxy(fc).GetStatPrefix())
	}
	// The HTTP-legacy service: its own per-port chain, to its own cluster, and
	// nothing on the TCP mesh port.
	assert.NotNil(t, chainByName(l.GetFilterChains(), "cap_tcp_tcp:mixed.t.aether.internal_9000"))
	assert.Nil(t, chainByName(l.GetFilterChains(), "cap_tcp_tcp:mixed.t.aether.internal_18082"),
		"raw TCP to :18082 of a service whose legacy target is HTTP names nothing")
	assert.Empty(t, duplicateMatches(l.GetFilterChains()))
}

// TestCaptureChains_NoAnyPortChainForAServiceWithHTTP: the any-port chain
// matches the service's address on every port nothing else names. On a
// service with an HTTP port it would outrank the HTTP chains for the address
// (the address is decided before the protocol) and forward HTTP to the
// raw-TCP port as bytes. AnyPort false must mean NO chain without a
// destination port on that address.
func TestCaptureChains_NoAnyPortChainForAServiceWithHTTP(t *testing.T) {
	const cluster = "tcp:first.t.aether.internal"
	portless := func(l *listenerv3.Listener) (n int) {
		for _, fc := range l.GetFilterChains() {
			m := fc.GetFilterChainMatch()
			if len(m.GetPrefixRanges()) > 0 && m.GetDestinationPort() == nil {
				n++
			}
		}
		return n
	}
	without := perPortListener(t, []CaptureTCPService{{ClusterName: cluster, ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, LegacyTCPPort: 9000}}, []uint32{80, 8080, 18081}, true)
	assert.Zero(t, portless(without))
	for _, fc := range without.GetFilterChains() {
		assert.NotContains(t, fc.GetName(), "anyport")
		if tc := chainTCPProxy(fc); tc != nil {
			assert.NotContains(t, tc.GetStatPrefix(), "anyport")
		}
		// and no chain claims the address on an HTTP port number
		if m := fc.GetFilterChainMatch(); len(m.GetPrefixRanges()) > 0 {
			assert.NotContains(t, []uint32{80, 8080, 18081}, m.GetDestinationPort().GetValue(), "chain %s", fc.GetName())
		}
	}
	// Negative control: with AnyPort the portless chain is there.
	with := perPortListener(t, []CaptureTCPService{{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, AnyPort: true}}, []uint32{80, 8080, 18081}, true)
	assert.Equal(t, 1, portless(with))
}

// TestCaptureChains_AnyPortTwinsOnHTTPPortNumbers: the listener names every
// HTTP port number of the mesh, and a connection to a named port is matched
// only among the chains naming it. A pure-TCP service's any-port chain
// therefore needs a twin on each of those numbers, or a client that dials
// :8080 of it stops being served the day another service declares 8080 as
// HTTP. The twins share the any-port stat prefix (the counter its removal is
// gated on must see every port) and stay off the ports the service claims.
func TestCaptureChains_AnyPortTwinsOnHTTPPortNumbers(t *testing.T) {
	const cluster = "tcp:raw.t.aether.internal"
	const prefix = "cap_tcp_anyport_" + cluster
	httpPorts := []uint32{80, 8080, 9000, 9100, 18081}
	l := perPortListener(t, []CaptureTCPService{
		{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, AnyPort: true, TCPPorts: []uint32{9100}},
	}, httpPorts, true)

	twinPorts := map[uint32]string{}
	for _, fc := range l.GetFilterChains() {
		tc := chainTCPProxy(fc)
		if tc == nil || tc.GetStatPrefix() != prefix {
			continue
		}
		require.Len(t, fc.GetFilterChainMatch().GetPrefixRanges(), 1, fc.GetName())
		assert.Equal(t, "10.96.0.1", fc.GetFilterChainMatch().GetPrefixRanges()[0].GetAddressPrefix())
		assert.Equal(t, cluster, tc.GetCluster(), "every any-port spelling reaches the legacy target's floor cluster")
		twinPorts[fc.GetFilterChainMatch().GetDestinationPort().GetValue()] = fc.GetName()
	}
	assert.Equal(t, map[uint32]string{
		0:     prefix,
		80:    prefix + "_p80",
		8080:  prefix + "_p8080",
		18081: prefix + "_p18081",
	}, twinPorts, "a twin on every HTTP port number the service does not claim; none on 9000 (legacy), 9100 (its own raw-TCP port) or 18082")
	// The ports it claims are served by their own chains, under their own names.
	assert.Equal(t, cluster, chainTCPProxy(chainByName(l.GetFilterChains(), "cap_tcp_"+cluster+"_9000")).GetCluster())
	assert.Equal(t, cluster+":9100", chainTCPProxy(chainByName(l.GetFilterChains(), "cap_tcp_"+cluster+"_9100")).GetCluster())
	assert.Empty(t, duplicateMatches(l.GetFilterChains()))

	// Negative control: no HTTP port numbers, no twins.
	bare := perPortListener(t, []CaptureTCPService{{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, AnyPort: true}}, nil, true)
	n := 0
	for _, fc := range bare.GetFilterChains() {
		if tc := chainTCPProxy(fc); tc != nil && tc.GetStatPrefix() == prefix {
			n++
		}
	}
	assert.Equal(t, 1, n)
}

// TestCaptureChains_TLSTwinsOnNamedPortsButNotTheServicesOwnHTTP: an SNI chain
// (address + server_names) is never considered for a connection to a port some
// chain names (#911), so it gets a twin on every named port of the listener,
// the HTTP port numbers and OTHER services' raw-TCP ports included. Except the
// ports its own service declares as HTTP: a twin there claims the service's
// address on that port, its cleartext HTTP matches the address, fails the SNI
// and leaves through the passthrough.
func TestCaptureChains_TLSTwinsOnNamedPortsButNotTheServicesOwnHTTP(t *testing.T) {
	const sni = "a.example.com"
	svc := CaptureTCPService{
		ClusterName: "tcp:mixed.t.aether.internal", ClusterIP: "10.96.0.1",
		HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000},
		TLSRouteRules: []L4ServiceRoute{{
			SNIHostnames: []string{sni},
			Backends:     []L4Backend{{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1}},
		}},
	}
	other := CaptureTCPService{ClusterName: "tcp:raw.t.aether.internal", ClusterIP: "10.96.0.2", LegacyTCPPort: 5432, AnyPort: true}
	l := perPortListener(t, []CaptureTCPService{svc, other}, []uint32{80, 8080, 9090, 18081}, true)

	got := map[uint32]bool{}
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if !slices.Contains(m.GetServerNames(), sni) {
			continue
		}
		require.Len(t, m.GetPrefixRanges(), 1)
		assert.Equal(t, "10.96.0.1", m.GetPrefixRanges()[0].GetAddressPrefix(), "an SNI chain only ever claims its own service's address")
		got[m.GetDestinationPort().GetValue()] = true
	}
	want := map[uint32]bool{
		0:  true,                          // the unqualified chain: every port nothing names
		80: true, 9090: true, 18081: true, // HTTP port numbers of the mesh that are not this service's
		9000:  true, // its own raw-TCP port
		5432:  true, // another service's raw-TCP port
		18082: true, // the TCP mesh port, named by the other service's legacy chain
	}
	assert.Equal(t, want, got, "no twin on 8080: that is the service's own HTTP port")
	assert.Empty(t, duplicateMatches(l.GetFilterChains()))

	// The helper on its own, with its negative control: the same chains with
	// no HTTP port declared DO get the 8080 twin.
	tls := BuildCaptureTLSRouteFilterChains(svc, svc.TLSRouteRules, perPortSource)
	require.NotEmpty(t, tls)
	named := namedDestinationPorts([]CaptureTCPService{svc, other}, []uint32{80, 8080, 9090, 18081})
	assert.Equal(t, []uint32{80, 5432, 8080, 9000, 9090, 18081, 18082}, named)
	assert.False(t, hasSNIChainAt(qualifyTLSChainsByPorts(svc, tls, named), 8080))
	noHTTP := svc
	noHTTP.HTTPPorts = nil
	assert.True(t, hasSNIChainAt(qualifyTLSChainsByPorts(noHTTP, tls, named), 8080))
	// No named port, or no TLS chain: nothing to do.
	assert.Len(t, qualifyTLSChainsByPorts(svc, tls, nil), len(tls))
	assert.Empty(t, qualifyTLSChainsByPorts(svc, nil, named))
}

// TestCaptureHTTPRouteName_RoundTrip: the chain/table name of a port parses
// back to that port, and nothing else parses to a port. The cache reads the
// table set off the chain names of the listeners it publishes; a name that
// parsed wrongly would publish a table for a port no chain serves, or miss one.
func TestCaptureHTTPRouteName_RoundTrip(t *testing.T) {
	for _, p := range []uint32{1, 80, 8080, 18081, 65535} {
		name := CaptureHTTPRouteName(p)
		assert.Equal(t, fmt.Sprintf("cap_http_%d", p), name)
		got, ok := CaptureHTTPRoutePort(name)
		assert.True(t, ok, name)
		assert.Equal(t, p, got, name)
	}
	for _, name := range []string{
		CaptureHTTPOtherRouteName, "cap_http_", "cap_http", "cap_http_0", "cap_http_65536", "cap_http_4294967376",
		"cap_http_80x", "cap_http_-1", "cap_http_ 80", "cap_tcp_80", "out_http", "", "capture_default_p1", "xcap_http_80",
	} {
		p, ok := CaptureHTTPRoutePort(name)
		assert.False(t, ok, "%q must not parse as a port's table (got %d)", name, p)
	}
	assert.True(t, strings.HasPrefix(CaptureHTTPOtherRouteName, CaptureHTTPRoutePrefix),
		"cap_http_other shares the prefix, which is why it has to be told apart by the parse")
}
