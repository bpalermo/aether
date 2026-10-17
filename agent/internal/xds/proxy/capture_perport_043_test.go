package proxy

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/agent/test/xdseval"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Proposal 043, "The chains of a capture listener": the port a client DIALLED
// decides what a request reaches. Since this change the listener has ONE HTTP
// chain for every port but the proxy port, selected by a matching tree that
// backtracks (capturematcher.go), and cap_http's one route table branches on
// the dialled port. These tests hold the listener against the silent wrong
// answers the layout exists to remove: HTTP on a port number some other
// service declares raw TCP leaving through the passthrough, a per-address
// chain swallowing a service's own HTTP, a TLSRoute that is inert on a port,
// and an action that names no chain (which closes the connection).
//
// They select chains with xdseval.SelectChain, which evaluates the listener's
// filter_chain_matcher the way the proxy does.

const perPortSource = "spiffe://aether.internal/ns/default/sa/client"

func perPortPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{Name: "p1", Namespace: "default", NetworkNamespace: "/var/run/netns/p1"}
}

func perPortListener(t *testing.T, svcs []CaptureTCPService, withPassthrough bool) *listenerv3.Listener {
	t.Helper()
	l, err := GenerateCaptureListener(perPortPod(), perPortSource, meshconst.ProxyCapturePort, "aether.internal", false, svcs, withPassthrough, nil)
	require.NoError(t, err)
	requireMatcherNamesChains(t, l)
	return l
}

// requireMatcherNamesChains is gate (a): every action of the listener's
// filter_chain_matcher names a chain the listener has, and no two chains share
// a name. An action naming a chain that is not there closes the connection
// (filter_chain_manager_impl.cc NoopFilterChainAction): neither the default
// chain nor a rejected listener says so.
func requireMatcherNamesChains(t interface {
	require.TestingT
	Helper()
}, l *listenerv3.Listener,
) {
	t.Helper()
	require.NotNil(t, l.GetFilterChainMatcher(), "the capture listener selects its chains with filter_chain_matcher")
	have := map[string]int{}
	for _, fc := range l.GetFilterChains() {
		have[fc.GetName()]++
		require.Nil(t, fc.GetFilterChainMatch(), "chain %s carries a filter_chain_match the proxy would ignore", fc.GetName())
	}
	for name, n := range have {
		require.Equal(t, 1, n, "chain name %q used %d times: the proxy rejects the listener", name, n)
		require.NotEmpty(t, name, "every chain needs a name when the listener has a matcher")
	}
	for _, name := range xdseval.MatcherActionNames(l.GetFilterChainMatcher()) {
		require.Contains(t, have, name, "a matcher action names chain %q, which the listener does not have", name)
	}
}

func sel(t *testing.T, l *listenerv3.Listener, c xdseval.Conn) string {
	t.Helper()
	name, err := xdseval.SelectChain(l, c)
	require.NoError(t, err)
	return name
}

func httpConn(ip string, port uint32) xdseval.Conn {
	return xdseval.Conn{IP: ip, Port: port, Transport: "raw_buffer", App: []string{"http/1.1"}}
}

func rawConn(ip string, port uint32) xdseval.Conn {
	return xdseval.Conn{IP: ip, Port: port, Transport: "raw_buffer"}
}

func tlsConn(ip string, port uint32, sni string) xdseval.Conn {
	return xdseval.Conn{IP: ip, Port: port, Transport: "tls", SNI: sni, App: []string{"h2", "http/1.1"}}
}

// matchKey renders every criterion of a filter_chain_match that Envoy tells two
// chains apart by, for the listeners that still select chains that way (the
// inbound listener). Two chains with the same key are a listener Envoy rejects
// whole ("multiple filter chains with the same matching rules").
func matchKey(fc *listenerv3.FilterChain) string {
	m := fc.GetFilterChainMatch()
	prefixes := make([]string, 0, len(m.GetPrefixRanges()))
	for _, pr := range m.GetPrefixRanges() {
		prefixes = append(prefixes, fmt.Sprintf("%s/%d", pr.GetAddressPrefix(), pr.GetPrefixLen().GetValue()))
	}
	slices.Sort(prefixes)
	sni := slices.Clone(m.GetServerNames())
	slices.Sort(sni)
	alpn := slices.Clone(m.GetApplicationProtocols())
	slices.Sort(alpn)
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
	slices.Sort(dups)
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

// testPorts are the ports the selection tests dial: HTTP port numbers of a
// typical mesh, raw-TCP ones, the mesh's own two and a port nothing names.
var testPorts = []uint32{80, 443, 5432, 7777, 8080, 9000, 9090, 9100, meshconst.ProxyOutboundPort, meshconst.ProxyL4OutboundPort}

// TestCaptureHTTPChains_OneChainWhateverTheMesh: the listener has ONE HTTP
// chain, cap_http, serving the route table of its own name; cleartext HTTP
// to any port, the proxy port 18081 included, reaches it, and it records the
// dialled port for its table. (Through chart 3.0.1 the proxy port had a chain
// of its own, cap_http_18081.) Nothing about the HTTP port numbers of the
// mesh is in the listener: a port number entering or leaving the mesh does
// not rebuild it.
func TestCaptureHTTPChains_OneChainWhateverTheMesh(t *testing.T) {
	for _, redirectAll := range []bool{true, false} {
		l := perPortListener(t, nil, redirectAll)
		var httpChains []string
		hcms := 0
		for _, fc := range l.GetFilterChains() {
			if strings.HasPrefix(fc.GetName(), CaptureHTTPChainName) {
				httpChains = append(httpChains, fc.GetName())
				assert.Equal(t, fc.GetName(), hcmRouteName(t, fc), "chain %s must serve the table of its own name", fc.GetName())
			}
			for _, f := range fc.GetFilters() {
				if strings.HasSuffix(f.GetName(), "http_connection_manager") {
					hcms++
				}
			}
		}
		assert.Equal(t, []string{CaptureHTTPChainName}, httpChains, "redirect-all %v", redirectAll)
		assert.Equal(t, 1, hcms, "one connection manager per capture listener (redirect-all %v)", redirectAll)
		assert.True(t, chainHasDialledPort(chainByName(l.GetFilterChains(), CaptureHTTPChainName)))
	}

	l := perPortListener(t, nil, true)
	for _, p := range testPorts {
		want := CaptureHTTPChainName
		assert.Equal(t, want, sel(t, l, httpConn("10.96.0.9", p)), "HTTP to port %d", p)
		h2c := httpConn("10.96.0.9", p)
		h2c.App = []string{"h2c"}
		assert.Equal(t, want, sel(t, l, h2c), "h2c to port %d", p)
		// Not HTTP, or TLS: the passthrough.
		assert.Equal(t, "cap_passthrough", sel(t, l, rawConn("10.96.0.9", p)), "raw bytes to port %d", p)
		assert.Equal(t, "cap_passthrough", sel(t, l, tlsConn("10.96.0.9", p, "example.com")), "TLS to port %d", p)
	}

	// The dialled port is recorded by cap_http, connection-level, never
	// shared upstream, for every port: a dial to the proxy port is one more
	// branch of its table.
	assert.True(t, chainHasDialledPort(chainByName(l.GetFilterChains(), CaptureHTTPChainName)))
	assert.Nil(t, chainByName(l.GetFilterChains(), "cap_http_18081"))
}

func chainHasDialledPort(fc *listenerv3.FilterChain) bool {
	for _, f := range fc.GetFilters() {
		if strings.Contains(f.GetTypedConfig().String(), DialledPortFilterStateKey) && strings.Contains(f.GetTypedConfig().String(), "%DOWNSTREAM_LOCAL_PORT%") {
			return true
		}
	}
	return false
}

// TestCaptureHTTPChains_ScopedMode: without the passthrough (the scoped shape
// only unit tests build) the HTTP chains are selected by the port alone, and
// the TCP mesh port goes to the blackhole chain.
func TestCaptureHTTPChains_ScopedMode(t *testing.T) {
	l := perPortListener(t, nil, false)
	assert.Nil(t, l.GetDefaultFilterChain())
	require.NotNil(t, chainByName(l.GetFilterChains(), "cap_tcp_blackhole"))
	assert.Equal(t, CaptureHTTPChainName, sel(t, l, httpConn("10.96.0.9", 8080)))
	assert.Equal(t, CaptureHTTPChainName, sel(t, l, rawConn("10.96.0.9", 8080)), "scoped: the port alone decides")
	assert.Equal(t, CaptureHTTPChainName, sel(t, l, rawConn("10.96.0.9", 18081)), "scoped: the proxy port is cap_http's too")
	assert.Equal(t, CaptureHTTPChainName, sel(t, l, httpConn("10.96.0.9", 18081)))
	assert.Equal(t, "cap_tcp_blackhole", sel(t, l, rawConn("10.96.0.9", meshconst.ProxyL4OutboundPort)))
	assert.Equal(t, "cap_tcp_blackhole", sel(t, l, httpConn("10.96.0.9", meshconst.ProxyL4OutboundPort)))
}

// TestCaptureChains_EveryActionNamesAChain runs gate (a) over the shapes most
// likely to name a chain twice or one that is not there, and shows the gate
// sees both defects.
func TestCaptureChains_EveryActionNamesAChain(t *testing.T) {
	tls := []L4ServiceRoute{{
		SNIHostnames: []string{"a.example.com", "*.wild.example.com"},
		Backends:     []L4Backend{{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1}},
	}}
	shapes := map[string][]CaptureTCPService{
		"legacy target IS the TCP mesh port": {{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: meshconst.ProxyL4OutboundPort, TCPPorts: []uint32{meshconst.ProxyL4OutboundPort}}},
		"legacy port repeated in TCPPorts":   {{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TCPPorts: []uint32{9000, 9001}}},
		"one number, HTTP for one service and raw TCP for another": {
			{ClusterName: "tcp:mixed.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
			{ClusterName: "tcp:nine.t.aether.internal", ClusterIP: "10.96.0.2", HTTPPorts: []uint32{9000}, TCPPorts: []uint32{8080}},
		},
		"TLSRoute on a mixed service and on a pure-TCP one": {
			{ClusterName: "tcp:mixed.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}, TLSRouteRules: tls},
			{ClusterName: "tcp:raw.t.aether.internal", ClusterIP: "10.96.0.2", LegacyTCPPort: 9000, TLSRouteRules: tls},
		},
		"a TLSRoute alone": {{ClusterName: "tcp:tls.t.aether.internal", ClusterIP: "10.96.0.3", TLSRouteRules: tls}},
		"two services, the same address by mistake, different ports": {
			{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
			{ClusterName: "tcp:b.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9001}},
		},
		"no address": {{ClusterName: "tcp:none.t.aether.internal", LegacyTCPPort: 9000}},
	}
	for name, svcs := range shapes {
		for _, withPassthrough := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/passthrough=%v", name, withPassthrough), func(t *testing.T) {
				perPortListener(t, svcs, withPassthrough) // runs the gate
			})
		}
	}

	// Seen red: the gate fails on a listener that lost a chain an action
	// names, and on one with a name twice.
	l := perPortListener(t, shapes["TLSRoute on a mixed service and on a pure-TCP one"], true)
	for _, drop := range []string{"cap_tls_tcp:mixed.t.aether.internal_0", "cap_tcp_tcp:raw.t.aether.internal_9000", CaptureHTTPChainName} {
		lost := listenerWithout(l, drop)
		assert.True(t, gateFails(lost), "the gate must fail when chain %s is gone", drop)
		// and what the proxy then does: closes the connection
		found := false
		for _, c := range []xdseval.Conn{tlsConn("10.96.0.1", 7777, "a.example.com"), rawConn("10.96.0.2", 9000), httpConn("10.96.0.9", 8080)} {
			name, err := xdseval.SelectChain(lost, c)
			require.NoError(t, err)
			found = found || name == ""
		}
		assert.True(t, found, "a connection to the lost chain %s is closed", drop)
	}
	twice := listenerWithout(l, "")
	twice.FilterChains = append(twice.FilterChains, twice.FilterChains[0])
	assert.True(t, gateFails(twice), "the gate must fail on a name used twice")
}

func listenerWithout(l *listenerv3.Listener, chain string) *listenerv3.Listener {
	out := &listenerv3.Listener{FilterChainMatcher: l.GetFilterChainMatcher(), DefaultFilterChain: l.GetDefaultFilterChain()}
	for _, fc := range l.GetFilterChains() {
		if fc.GetName() != chain {
			out.FilterChains = append(out.FilterChains, fc)
		}
	}
	return out
}

// fakeT records a failure instead of stopping the test, so a gate can be
// shown to fail.
type fakeT struct{ failed bool }

func (f *fakeT) Helper()               {}
func (f *fakeT) Errorf(string, ...any) { f.failed = true }
func (f *fakeT) FailNow()              { f.failed = true; panic(fakeFailNow{}) }

// gateFails runs gate against l on a fakeT and reports whether it failed.
func gateFails(l *listenerv3.Listener) (failed bool) {
	ft := &fakeT{}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(fakeFailNow); !ok {
				panic(r)
			}
		}
		failed = ft.failed
	}()
	requireMatcherNamesChains(ft, l)
	return ft.failed
}

type fakeFailNow struct{}

// TestCaptureChains_RawTCPAndHTTPShareAPortNumber is the measured finding the
// per-port layout of chart 3.0.0 existed for (proposal 043, layout L2):
// service A declares 9000 as raw TCP, service B serves HTTP on 9000. With
// filter_chain_match and one port-less HTTP chain, B's HTTP on 9000 left
// through the passthrough. With the matching tree, A's address and port reach
// A's raw-TCP chain, and HTTP to 9000 at any other address reaches cap_http.
func TestCaptureChains_RawTCPAndHTTPShareAPortNumber(t *testing.T) {
	svcs := []CaptureTCPService{
		{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
	}
	l := perPortListener(t, svcs, true)

	raw := chainByName(l.GetFilterChains(), "cap_tcp_tcp:a.t.aether.internal_9000")
	require.NotNil(t, raw)
	require.NotNil(t, chainTCPProxy(raw))
	assert.Equal(t, "tcp:a.t.aether.internal:9000", chainTCPProxy(raw).GetCluster(),
		"the raw-TCP port's own cluster, never the floor (which forwards to another port)")

	assert.Equal(t, raw.GetName(), sel(t, l, rawConn("10.96.0.1", 9000)))
	assert.Equal(t, raw.GetName(), sel(t, l, httpConn("10.96.0.1", 9000)), "A's raw-TCP port is raw TCP whatever the bytes look like")
	assert.Equal(t, CaptureHTTPChainName, sel(t, l, httpConn("10.96.0.2", 9000)), "B's HTTP on 9000 must reach cap_http although A declares 9000 raw TCP")
	// A's own HTTP, on its HTTP port and on any other: cap_http.
	for _, p := range testPorts {
		if p == 9000 || p == meshconst.ProxyOutboundPort {
			continue
		}
		assert.Equal(t, CaptureHTTPChainName, sel(t, l, httpConn("10.96.0.1", p)), "HTTP to A's address on %d", p)
	}

	// Negative control: without the declaration there is no raw-TCP chain.
	bare := perPortListener(t, []CaptureTCPService{{ClusterName: "tcp:a.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}}}, true)
	assert.Nil(t, chainByName(bare.GetFilterChains(), "cap_tcp_tcp:a.t.aether.internal_9000"))
	assert.Equal(t, "cap_passthrough", sel(t, bare, rawConn("10.96.0.1", 9000)))
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
	}, true)

	for _, p := range []uint32{9000, meshconst.ProxyL4OutboundPort} {
		name := fmt.Sprintf("cap_tcp_%s_%d", cluster, p)
		fc := chainByName(l.GetFilterChains(), name)
		require.NotNil(t, fc, "port %d", p)
		assert.Equal(t, cluster, chainTCPProxy(fc).GetCluster(), "the legacy target is reached through the floor cluster")
		assert.Equal(t, fc.GetName(), chainTCPProxy(fc).GetStatPrefix())
		assert.Equal(t, name, sel(t, l, rawConn("10.96.0.1", p)))
	}
	// The HTTP-legacy service: its own per-port chain, to its own cluster, and
	// nothing on the TCP mesh port.
	assert.Equal(t, "cap_tcp_tcp:mixed.t.aether.internal_9000", sel(t, l, rawConn("10.96.0.2", 9000)))
	assert.Nil(t, chainByName(l.GetFilterChains(), "cap_tcp_tcp:mixed.t.aether.internal_18082"),
		"raw TCP to :18082 of a service whose legacy target is HTTP names nothing")
	assert.Equal(t, "cap_passthrough", sel(t, l, rawConn("10.96.0.2", meshconst.ProxyL4OutboundPort)))
}

// rawReach returns the ports of testPorts (and extra) on which raw bytes to ip
// reach a chain of the service rather than an HTTP chain or the passthrough.
func rawReach(t *testing.T, l *listenerv3.Listener, ip string, extra ...uint32) []uint32 {
	t.Helper()
	var out []uint32
	for _, p := range append(slices.Clone(testPorts), extra...) {
		name := sel(t, l, rawConn(ip, p))
		if strings.HasPrefix(name, "cap_tcp_") && name != "cap_tcp_blackhole" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestCaptureChains_NoAnyPortChainForAServiceWithHTTP: since chart 3.0.0 a
// raw-TCP service is reached on the ports it declares and nowhere else (its
// legacy target's own port and :18082); the any-port chain is gone. Over
// every shape of service, raw bytes to its address reach it on exactly those
// ports.
func TestCaptureChains_NoAnyPortChainForAServiceWithHTTP(t *testing.T) {
	const cluster = "tcp:first.t.aether.internal"
	tls := []L4ServiceRoute{{
		SNIHostnames: []string{"a.example.com"},
		Backends:     []L4Backend{{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1}},
	}}
	weighted := []L4ServiceRoute{{Backends: []L4Backend{
		{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1},
		{Service: "t/y", Cluster: "tcp:y.t.aether.internal", Weight: 1},
	}}}
	for name, tt := range map[string]struct {
		svc   CaptureTCPService
		ports []uint32
	}{
		"raw-TCP legacy target, with an HTTP port": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, LegacyTCPPort: 9000},
			ports: []uint32{9000, 18082},
		},
		"raw-TCP legacy target, no HTTP port": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000},
			ports: []uint32{9000, 18082},
		},
		"raw-TCP legacy target and a second raw-TCP port": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TCPPorts: []uint32{9000, 9100}},
			ports: []uint32{9000, 9100, 18082},
		},
		"raw-TCP legacy target on an HTTP port number of the mesh": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 8080},
			ports: []uint32{8080, 18082},
		},
		"raw-TCP legacy target behind a weighted TCPRoute": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TCPRouteRules: weighted},
			ports: []uint32{9000, 18082},
		},
		"raw-TCP legacy target with a TLSRoute": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TLSRouteRules: tls},
			ports: []uint32{9000, 18082},
		},
		"HTTP legacy target, one raw-TCP port": {
			svc:   CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}},
			ports: []uint32{9000},
		},
		"HTTP only": {
			svc: CaptureTCPService{ClusterName: cluster, ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}},
		},
	} {
		for _, withPassthrough := range []bool{true, false} {
			l := perPortListener(t, []CaptureTCPService{tt.svc}, withPassthrough)
			for _, fc := range l.GetFilterChains() {
				assert.NotContains(t, fc.GetName(), "anyport", name)
				if tc := chainTCPProxy(fc); tc != nil {
					assert.NotContains(t, tc.GetStatPrefix(), "anyport", name)
				}
			}
			got := rawReach(t, l, "10.96.0.1", 1234, 31000)
			assert.Equal(t, tt.ports, got, "%s (passthrough %v): the ports raw bytes reach the service on", name, withPassthrough)
		}
	}
}

// TestCaptureChains_NoChainOnAnHTTPPortNumberTheServiceDoesNotDeclare: on an
// HTTP port number of the mesh that a raw-TCP service does not declare,
// nothing of the service is reached at its address. The ports it does
// declare are served by their own chains, to their own clusters.
func TestCaptureChains_NoChainOnAnHTTPPortNumberTheServiceDoesNotDeclare(t *testing.T) {
	const cluster = "tcp:raw.t.aether.internal"
	l := perPortListener(t, []CaptureTCPService{
		{ClusterName: cluster, ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TCPPorts: []uint32{9100}},
	}, true)
	assert.Equal(t, []uint32{9000, 9100, 18082}, rawReach(t, l, "10.96.0.1"))
	for _, p := range []uint32{80, 8080, 9090, 18081} {
		assert.Equal(t, "cap_passthrough", sel(t, l, rawConn("10.96.0.1", p)), "raw bytes to %d", p)
	}
	assert.Equal(t, cluster, chainTCPProxy(chainByName(l.GetFilterChains(), "cap_tcp_"+cluster+"_9000")).GetCluster())
	assert.Equal(t, cluster, chainTCPProxy(chainByName(l.GetFilterChains(), "cap_tcp_"+cluster+"_18082")).GetCluster())
	assert.Equal(t, cluster+":9100", chainTCPProxy(chainByName(l.GetFilterChains(), "cap_tcp_"+cluster+"_9100")).GetCluster())
	for _, p := range []uint32{9000, 9100, 18082} {
		name := sel(t, l, rawConn("10.96.0.1", p))
		assert.Equal(t, name, chainTCPProxy(chainByName(l.GetFilterChains(), name)).GetStatPrefix(), "every chain counts under its own name")
	}
}

// TestCaptureChains_TLSRouteOnEveryPortButTheServicesHTTP: a TLSRoute's server
// name selects its chain on EVERY port of the service's address but the ones
// that carry the service's cleartext HTTP: its declared HTTP ports, and the
// proxy port (18081) when it has any. TLS to one of those leaves through the
// passthrough, as it did with the per-port listener of chart 3.0.0 (the
// owner's ruling for this change). On a declared raw-TCP port the server name
// wins over the port's chain, and any other name gets the port's chain.
func TestCaptureChains_TLSRouteOnEveryPortButTheServicesHTTP(t *testing.T) {
	const sni = "a.example.com"
	rules := []L4ServiceRoute{{
		SNIHostnames: []string{sni},
		Backends:     []L4Backend{{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1}},
	}}
	other := CaptureTCPService{ClusterName: "tcp:raw.t.aether.internal", ClusterIP: "10.96.0.2", LegacyTCPPort: 5432}

	for name, tt := range map[string]struct {
		svc   CaptureTCPService
		chain string
		// noTLS are the ports where TLS with the route's name does NOT reach it
		noTLS []uint32
	}{
		"HTTP and raw TCP": {
			svc:   CaptureTCPService{ClusterName: "tcp:web.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}, TLSRouteRules: rules},
			chain: "cap_tls_tcp:web.t.aether.internal_0",
			noTLS: []uint32{8080, 18081},
		},
		"HTTP only": {
			svc:   CaptureTCPService{ClusterName: "tcp:web.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{8080, 9090}, TLSRouteRules: rules},
			chain: "cap_tls_tcp:web.t.aether.internal_0",
			noTLS: []uint32{8080, 9090, 18081},
		},
		"raw TCP only": {
			svc:   CaptureTCPService{ClusterName: "tcp:db.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TLSRouteRules: rules},
			chain: "cap_tls_tcp:db.t.aether.internal_0",
		},
		"port 80 declared HTTP": {
			svc:   CaptureTCPService{ClusterName: "tcp:web.t.aether.internal", ClusterIP: "10.96.0.1", HTTPPorts: []uint32{80, 8080}, TLSRouteRules: rules},
			chain: "cap_tls_tcp:web.t.aether.internal_0",
			noTLS: []uint32{80, 8080, 18081},
		},
	} {
		l := perPortListener(t, []CaptureTCPService{tt.svc, other}, true)
		for _, p := range testPorts {
			got := sel(t, l, tlsConn("10.96.0.1", p, sni))
			if slices.Contains(tt.noTLS, p) {
				assert.Equal(t, "cap_passthrough", got, "%s: TLS with the route's name to the service's own HTTP port %d leaves through the passthrough", name, p)
			} else {
				assert.Equal(t, tt.chain, got, "%s: TLS with the route's name to port %d", name, p)
			}
			// Case does not matter to a server name.
			if !slices.Contains(tt.noTLS, p) {
				assert.Equal(t, tt.chain, sel(t, l, tlsConn("10.96.0.1", p, strings.ToUpper(sni))), "%s: upper-case server name to %d", name, p)
			}
			// Another name: the raw-TCP port's chain, else the passthrough.
			want := "cap_passthrough"
			for _, tp := range rawReach(t, l, "10.96.0.1") {
				if tp == p {
					want = sel(t, l, rawConn("10.96.0.1", p))
				}
			}
			assert.Equal(t, want, sel(t, l, tlsConn("10.96.0.1", p, "b.example.com")), "%s: TLS with another name to %d", name, p)
			// The route's name at ANOTHER address is never the route's.
			assert.NotEqual(t, tt.chain, sel(t, l, tlsConn("10.96.0.2", p, sni)), "%s: an SNI chain only ever claims its own service's address", name)
		}
		// Cleartext HTTP to the service's address reaches cap_http on every
		// port that is not raw TCP: the TLSRoute does not take it.
		for _, p := range testPorts {
			if slices.Contains(rawReach(t, l, "10.96.0.1"), p) {
				continue
			}
			assert.Equal(t, CaptureHTTPChainName, sel(t, l, httpConn("10.96.0.1", p)), "%s: HTTP to %d", name, p)
		}
	}
}

// TestCaptureChains_TLSRouteWildcardHostname: "*.example.com" in a TLSRoute
// matches any name under example.com, and an exact name of another route wins
// over it, as filter_chain_match's server_names did.
func TestCaptureChains_TLSRouteWildcardHostname(t *testing.T) {
	svc := CaptureTCPService{ClusterName: "tcp:w.t.aether.internal", ClusterIP: "10.96.0.1", LegacyTCPPort: 9000, TLSRouteRules: []L4ServiceRoute{
		{SNIHostnames: []string{"*.example.com"}, Backends: []L4Backend{{Service: "t/x", Cluster: "tcp:x.t.aether.internal", Weight: 1}}},
		{SNIHostnames: []string{"exact.example.com"}, Backends: []L4Backend{{Service: "t/y", Cluster: "tcp:y.t.aether.internal", Weight: 1}}},
		{SNIHostnames: []string{"*.deep.example.com"}, Backends: []L4Backend{{Service: "t/z", Cluster: "tcp:z.t.aether.internal", Weight: 1}}},
	}}
	l := perPortListener(t, []CaptureTCPService{svc}, true)
	for sni, want := range map[string]string{
		"a.example.com":      "cap_tls_tcp:w.t.aether.internal_0",
		"a.b.example.com":    "cap_tls_tcp:w.t.aether.internal_0",
		"exact.example.com":  "cap_tls_tcp:w.t.aether.internal_1",
		"x.deep.example.com": "cap_tls_tcp:w.t.aether.internal_2",
		"example.com":        "cap_passthrough",
		"other.org":          "cap_passthrough",
	} {
		assert.Equal(t, want, sel(t, l, tlsConn("10.96.0.1", 7777, sni)), sni)
	}
	assert.Equal(t, "cap_tcp_tcp:w.t.aether.internal_9000", sel(t, l, tlsConn("10.96.0.1", 9000, "other.org")))
}

// TestCaptureHTTPRouteName: the name of a per-port table, and of the proxy
// port's chain and table.
func TestCaptureHTTPRouteName(t *testing.T) {
	for _, p := range []uint32{1, 80, 8080, 18081, 65535} {
		assert.Equal(t, fmt.Sprintf("cap_http_%d", p), CaptureHTTPRouteName(p))
	}
	assert.True(t, strings.HasPrefix(CaptureHTTPOtherRouteName, CaptureHTTPRoutePrefix))
	assert.False(t, strings.HasPrefix(CaptureHTTPChainName, CaptureHTTPRoutePrefix), "the published table is not a per-port table")
}
