package cache

// Per-port addressing (proposal 043) over hostile service shapes: every test
// here fails when the property it names does not hold.

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"net"
	"slices"
	"sort"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// ---------------------------------------------------------------------------
// A model of Envoy's filter_chain_match selection (FilterChainManagerImpl::
// findFilterChain), as far as the capture listener uses it: destination port,
// then the MOST SPECIFIC destination prefix only, then the server name, then
// the transport protocol, then the application protocols. No tier backtracks:
// a port with its own subtree never falls back to the port-less chains, and a
// connection that matched a prefix never falls back to a shorter one.
// ---------------------------------------------------------------------------

type shpConn struct {
	ip        string
	port      uint32
	sni       string
	transport string   // "raw_buffer" or "tls"
	app       []string // what http_inspector / ALPN reported
}

func shpSelectChain(l *listenerv3.Listener, c shpConn) string {
	chains := l.GetFilterChains()
	// tier 1: destination port
	var tier []*listenerv3.FilterChain
	for _, fc := range chains {
		if fc.GetFilterChainMatch().GetDestinationPort().GetValue() == c.port {
			tier = append(tier, fc)
		}
	}
	if len(tier) == 0 {
		for _, fc := range chains {
			if fc.GetFilterChainMatch().GetDestinationPort() == nil {
				tier = append(tier, fc)
			}
		}
	}
	// tier 2: most specific prefix
	best := -1
	ip := net.ParseIP(c.ip)
	for _, fc := range tier {
		prs := fc.GetFilterChainMatch().GetPrefixRanges()
		if len(prs) == 0 {
			if best < 0 {
				best = 0
			}
			continue
		}
		for _, pr := range prs {
			_, n, err := net.ParseCIDR(fmt.Sprintf("%s/%d", pr.GetAddressPrefix(), pr.GetPrefixLen().GetValue()))
			if err == nil && n.Contains(ip) && int(pr.GetPrefixLen().GetValue())+1 > best {
				best = int(pr.GetPrefixLen().GetValue()) + 1
			}
		}
	}
	var t2 []*listenerv3.FilterChain
	for _, fc := range tier {
		prs := fc.GetFilterChainMatch().GetPrefixRanges()
		if len(prs) == 0 && best == 0 {
			t2 = append(t2, fc)
		}
		for _, pr := range prs {
			_, n, err := net.ParseCIDR(fmt.Sprintf("%s/%d", pr.GetAddressPrefix(), pr.GetPrefixLen().GetValue()))
			if err == nil && n.Contains(ip) && int(pr.GetPrefixLen().GetValue())+1 == best {
				t2 = append(t2, fc)
			}
		}
	}
	// tier 3: server name (exact, else chains with none)
	var t3 []*listenerv3.FilterChain
	for _, fc := range t2 {
		if c.sni != "" && slices.Contains(fc.GetFilterChainMatch().GetServerNames(), c.sni) {
			t3 = append(t3, fc)
		}
	}
	if len(t3) == 0 {
		for _, fc := range t2 {
			if len(fc.GetFilterChainMatch().GetServerNames()) == 0 {
				t3 = append(t3, fc)
			}
		}
	}
	// tier 4: transport protocol
	var t4 []*listenerv3.FilterChain
	for _, fc := range t3 {
		if fc.GetFilterChainMatch().GetTransportProtocol() == c.transport {
			t4 = append(t4, fc)
		}
	}
	if len(t4) == 0 {
		for _, fc := range t3 {
			if fc.GetFilterChainMatch().GetTransportProtocol() == "" {
				t4 = append(t4, fc)
			}
		}
	}
	// tier 5: application protocols
	for _, fc := range t4 {
		for _, a := range c.app {
			if slices.Contains(fc.GetFilterChainMatch().GetApplicationProtocols(), a) {
				return fc.GetName()
			}
		}
	}
	for _, fc := range t4 {
		if len(fc.GetFilterChainMatch().GetApplicationProtocols()) == 0 {
			return fc.GetName()
		}
	}
	if l.GetDefaultFilterChain() != nil {
		return l.GetDefaultFilterChain().GetName()
	}
	return ""
}

func shpHTTP(ip string, port uint32) shpConn {
	return shpConn{ip: ip, port: port, transport: "raw_buffer", app: []string{"http/1.1"}}
}

func shpRaw(ip string, port uint32) shpConn {
	return shpConn{ip: ip, port: port, transport: "raw_buffer"}
}

// shpDuplicateChainMatches returns the chains of l whose filter_chain_match is
// identical to another chain's. Envoy rejects the whole listener for one.
func shpDuplicateChainMatches(l *listenerv3.Listener) []string {
	seen := map[string]string{}
	var dups []string
	for _, fc := range l.GetFilterChains() {
		b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(fc.GetFilterChainMatch())
		if prev, ok := seen[string(b)]; ok {
			dups = append(dups, prev+" == "+fc.GetName())
		}
		seen[string(b)] = fc.GetName()
	}
	// names must be unique too
	names := map[string]bool{}
	for _, fc := range l.GetFilterChains() {
		if names[fc.GetName()] {
			dups = append(dups, "name twice: "+fc.GetName())
		}
		names[fc.GetName()] = true
	}
	return dups
}

func shpHostileFixture() []a043Service {
	h, tcp, udp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_UDP
	pp := func(m map[uint32]registryv1.PortProtocol) map[uint32]registryv1.PortProtocol { return m }
	return []a043Service{
		{name: "w80", ns: a043NS, legacy: 80, ports: pp(map[uint32]registryv1.PortProtocol{80: h}), ip: "10.0.3.1", vip: "10.96.3.1"},
		{name: "t80", ns: a043NS, legacy: 8080, ports: pp(map[uint32]registryv1.PortProtocol{8080: h, 80: tcp}), ip: "10.0.3.2", vip: "10.96.3.2"},
		{name: "m81", ns: a043NS, legacy: 8080, ports: pp(map[uint32]registryv1.PortProtocol{8080: h, 18081: h}), ip: "10.0.3.3", vip: "10.96.3.3"},
		{name: "l81", ns: a043NS, legacy: 18081, ports: pp(map[uint32]registryv1.PortProtocol{18081: h}), ip: "10.0.3.4", vip: "10.96.3.4"},
		{name: "t81", ns: a043NS, legacy: 18081, ports: pp(map[uint32]registryv1.PortProtocol{18081: tcp}), ip: "10.0.3.5", vip: "10.96.3.5"},
		{name: "h82", ns: a043NS, legacy: 18082, ports: pp(map[uint32]registryv1.PortProtocol{18082: h}), ip: "10.0.3.6", vip: "10.96.3.6"},
		{name: "t82", ns: a043NS, legacy: 8080, ports: pp(map[uint32]registryv1.PortProtocol{8080: h, 18082: tcp}), ip: "10.0.3.7", vip: "10.96.3.7"},
		{name: "l82", ns: a043NS, legacy: 18082, ports: pp(map[uint32]registryv1.PortProtocol{18082: tcp}), ip: "10.0.3.8", vip: "10.96.3.8"},
		{name: "u53", ns: a043NS, legacy: 8080, ports: pp(map[uint32]registryv1.PortProtocol{8080: h, 5353: udp}), ip: "10.0.3.9", vip: "10.96.3.9"},
		{name: "tcp80", ns: a043NS, legacy: 80, ports: pp(map[uint32]registryv1.PortProtocol{80: tcp}), ip: "10.0.3.10", vip: "10.96.3.10"},
		{name: "tmulti", ns: a043NS, legacy: 9000, ports: pp(map[uint32]registryv1.PortProtocol{9000: tcp, 8080: tcp, 80: tcp, 18081: tcp}), ip: "10.0.3.11", vip: "10.96.3.11"},
		{name: "udponly", ns: a043NS, legacy: 5353, ports: pp(map[uint32]registryv1.PortProtocol{5353: udp}), ip: "10.0.3.12", vip: "10.96.3.12"},
		{name: "h18082m", ns: a043NS, legacy: 8080, ports: pp(map[uint32]registryv1.PortProtocol{8080: h, 18082: h}), ip: "10.0.3.13", vip: "10.96.3.13"},
	}
}

func shpTLSRoutes(parents ...string) map[string][]proxy.L4ServiceRoute {
	out := map[string][]proxy.L4ServiceRoute{}
	for _, p := range parents {
		out[p] = []proxy.L4ServiceRoute{{
			SNIHostnames: []string{"tls." + strings.ReplaceAll(p, "/", ".") + ".example"},
			Backends:     []proxy.L4Backend{{Service: "demo/tcponly", Cluster: "tcp:tcponly.demo." + a043Mesh, Weight: 1}},
		}}
	}
	return out
}

// TestPerPort_HostileShapes_NoDuplicateDomainOrChain: services that
// declare the mesh ports (18081, 18082) and 80 in every class, with TLSRoutes
// on some of them. A duplicate domain makes Envoy reject a route table; a
// duplicate filter_chain_match makes it reject the listener.
func TestPerPort_HostileShapes_NoDuplicateDomainOrChain(t *testing.T) {
	services := append(a043Fixture(), shpHostileFixture()...)
	c := a043Cache(t, services)
	c.SetTLSServiceRoutes(shpTLSRoutes("demo/w80", "demo/m81", "demo/tcp80", "demo/httponly", "demo/tmulti", "demo/l82"))
	require.NoError(t, c.generateSnapshot(context.Background()))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	assert.Empty(t, duplicateDomains(snap.GetResources("type.googleapis.com/envoy.config.route.v3.RouteConfiguration")))
	l := a043CaptureListener(t, c)
	assert.Empty(t, shpDuplicateChainMatches(l), "duplicate filter chain matches")
	t.Logf("chains: %d", len(l.GetFilterChains()))
}

// TestPerPort_TLSRouteTwinTakesLegacyHTTPSpellings: a service whose HTTP is
// reached by the legacy spellings (":18081", and port 80 of a service that
// does not declare 80) and that is ALSO the parent of a TLSRoute.
//
// qualifyTLSChainsByPorts skips the ports the service DECLARES as HTTP, "so the
// service's own cleartext HTTP still reaches cap_http_<P>". The two legacy
// numbers are HTTP ports of every such service too (their tables hold its
// legacy virtual host), but they are not in HTTPPorts, so the twins are built
// on them: cleartext HTTP to <vip>:18081 or <vip>:80 matches the address,
// fails the SNI and leaves through the passthrough.
func TestPerPort_TLSRouteTwinTakesLegacyHTTPSpellings(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	l0 := a043CaptureListener(t, c)
	const vip = "10.96.1.1" // httponly: 8080 HTTP, legacy target 8080
	// Control: without a TLSRoute all three reach an HTTP chain.
	require.Equal(t, "cap_http_8080", shpSelectChain(l0, shpHTTP(vip, 8080)))
	require.Equal(t, "cap_http_18081", shpSelectChain(l0, shpHTTP(vip, 18081)))
	require.Equal(t, "cap_http_80", shpSelectChain(l0, shpHTTP(vip, 80)))

	c.SetTLSServiceRoutes(shpTLSRoutes("demo/httponly"))
	require.NoError(t, c.generateSnapshot(context.Background()))
	l := a043CaptureListener(t, c)
	// The declared port is protected, as the comment says.
	assert.Equal(t, "cap_http_8080", shpSelectChain(l, shpHTTP(vip, 8080)))
	// The legacy spellings of the same service are not.
	assert.Equal(t, "cap_http_18081", shpSelectChain(l, shpHTTP(vip, 18081)), "cleartext HTTP to <vip>:18081 of a TLSRoute parent")
	assert.Equal(t, "cap_http_80", shpSelectChain(l, shpHTTP(vip, 80)), "cleartext HTTP to <vip>:80 of a TLSRoute parent")
}

// TestPerPort_RawTCPServiceIsReachedOnItsDeclaredPortsOnly: a raw-TCP-only
// service is reached on its declared port and on :18082, and a connection to
// any other port of its address selects no chain of ANY service: it takes the
// listener's default, the passthrough to the address that was dialled. Until
// chart 2.7.0 a port-less any-port chain delivered every such connection to
// the declared port.
func TestPerPort_RawTCPServiceIsReachedOnItsDeclaredPortsOnly(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	l := a043CaptureListener(t, c)
	const vip = "10.96.1.7" // tcponly: 9000 raw TCP, nothing else
	own := "cap_tcp_tcp:tcponly.demo." + a043Mesh
	assert.Equal(t, own+"_9000", shpSelectChain(l, shpRaw(vip, 9000)))
	assert.Equal(t, own+"_18082", shpSelectChain(l, shpRaw(vip, 18082)))
	// A port nothing names, HTTP port numbers of other services, the legacy
	// HTTP numbers, and a raw-TCP port number of another service.
	for _, p := range []uint32{5555, 80, 8080, 9090, 7070, 18081, 443} {
		got := shpSelectChain(l, shpRaw(vip, p))
		assert.Falsef(t, strings.HasPrefix(got, "cap_tcp_") || strings.HasPrefix(got, "cap_http_"), "raw TCP to tcponly:%d selected chain %q; it must take the passthrough", p, got)
	}
	for _, fc := range l.GetFilterChains() {
		assert.NotContains(t, fc.GetName(), "anyport")
	}
}

// TestPerPort_ChainFilterOfAServiceWithNoEndpointHere: a service that has a
// service-wide chain filter (ext_authz is one) and a generated Service, on a
// node that holds no endpoint of it (registry not reachable at start, or not
// listed yet). The filter is enabled by the service's own virtual host; a
// request that lands on the "*" virtual host runs without it.
func TestPerPort_ChainFilterOfAServiceWithNoEndpointHere(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	// The agent knows every Service (capture reconciler), a pod has started,
	// and the registry has listed NOTHING yet.
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, services)
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	const filter = "envoy.filters.http.header_mutation"
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": {Name: filter, Config: cfg}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	tables := map[string]*routev3.RouteConfiguration{}
	for _, r := range c.captureRouteTables([]uint32{80, 8080, 9090, 18081}) {
		rc := r.(*routev3.RouteConfiguration)
		tables[rc.GetName()] = rc
	}
	filtered := func(table, host string) (a043Answer, bool) {
		ans := a043Resolve(t, tables[table], host)
		for _, vh := range tables[table].GetVirtualHosts() {
			if vh.GetName() == ans.vhost {
				_, ok := vh.GetTypedPerFilterConfig()[filter]
				return ans, ok
			}
		}
		return ans, false
	}
	const mesh = "multi.demo." + a043Mesh
	// The legacy tables keep the service's virtual host, with the filter.
	ans, ok := filtered("cap_http_18081", mesh+":18081")
	assert.Truef(t, ok, "legacy table: %+v", ans)
	// The real-port tables: the request names its cluster on demand, on the
	// catch-all, which carries no per-filter config.
	for _, tc := range []struct{ table, host string }{
		{"cap_http_8080", mesh + ":8080"},
		{"cap_http_9090", mesh + ":9090"},
	} {
		ans, ok := filtered(tc.table, tc.host)
		t.Logf("%s %s -> vhost=%s cluster=%s onDemand=%v status=%d filter=%v", tc.table, tc.host, ans.vhost, ans.cluster, ans.onDemand, ans.status, ok)
		assert.Falsef(t, ans.onDemand && !ok, "%s, Host %s: parked on the on-demand route of %q with the service's chain filter NOT enabled", tc.table, tc.host, ans.vhost)
	}
}

// ---------------------------------------------------------------------------
// The memo of the capture route tables: after ANY sequence of mutations the
// tables a snapshot gets must equal the tables built from scratch.
// ---------------------------------------------------------------------------

func shpTablesEqual(a, b []types.Resource) (string, bool) {
	if len(a) != len(b) {
		return fmt.Sprintf("%d tables != %d", len(a), len(b)), false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return a[i].(*routev3.RouteConfiguration).GetName(), false
		}
	}
	return "", true
}

func TestPerPort_CaptureTablesMemoEqualsFreshBuild(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	// Eight walks of sixty steps: enough to visit every pair of operations,
	// and short enough for the race detector's leg, where forty did not fit
	// the test timeout.
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		services := a043Fixture()
		c := a043Cache(t, services)
		c.SetAuthzSidecar(0, false)
		projected := slices.Clone(services)
		routes := map[string][]proxy.GammaRoute{}
		chain := map[string]proxy.ExtensionFilter{}
		portMaps := map[string]map[uint32]uint32{}
		targetPorts := map[string][]uint32{}
		var trail []string
		pick := func() *a043Service { return &services[rng.Intn(len(services))] }
		for step := 0; step < 60; step++ {
			var op string
			switch rng.Intn(13) {
			case 0: // a port appears / disappears
				s := pick()
				s.ports = maps.Clone(s.ports)
				p := []uint32{80, 7000, 8080, 9000, 9090, 18081}[rng.Intn(6)]
				if _, ok := s.ports[p]; ok && len(s.ports) > 1 && p != s.legacy {
					delete(s.ports, p)
				} else {
					s.ports[p] = []registryv1.PortProtocol{h, tcp}[rng.Intn(2)]
				}
				op = fmt.Sprintf("ports %s=%v", s.key(), s.ports)
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
			case 1: // the legacy target moves
				s := pick()
				keys := slices.Sorted(maps.Keys(s.ports))
				s.legacy = keys[rng.Intn(len(keys))]
				op = fmt.Sprintf("legacy %s=%d", s.key(), s.legacy)
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
			case 2: // endpoint churn
				s := pick()
				s.ip = fmt.Sprintf("10.9.%d.%d", rng.Intn(250), 1+rng.Intn(250))
				op = "churn " + s.key()
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
			case 3: // a route
				s := pick()
				b := pick()
				if rng.Intn(3) == 0 {
					delete(routes, s.key())
				} else {
					routes[s.key()] = []proxy.GammaRoute{{
						ParentPort: []uint32{0, 8080, 9090}[rng.Intn(3)],
						Matches:    []proxy.GammaMatch{{Prefix: fmt.Sprintf("/p%d", rng.Intn(3))}},
						Backends:   []proxy.GammaBackend{{Service: b.key(), Cluster: proxy.ServiceClusterName(b.key(), a043Mesh), Weight: 1, Port: []uint32{0, 8080, 9090, 9000}[rng.Intn(4)]}},
					}}
				}
				op = "route " + s.key()
				c.SetServiceRoutes(maps.Clone(routes))
			case 4: // imported route
				s := pick()
				op = "imported route " + s.key()
				c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{s.key(): {{
					Matches:  []proxy.GammaMatch{{Prefix: fmt.Sprintf("/i%d", rng.Intn(3))}},
					Backends: []proxy.GammaBackend{{Service: s.key(), Cluster: proxy.ServiceClusterName(s.key(), a043Mesh), Weight: 1}},
				}}})
			case 5: // a chain filter
				s := pick()
				if _, ok := chain[s.key()]; ok {
					delete(chain, s.key())
				} else {
					chain[s.key()] = proxy.ExtensionFilter{Name: "envoy.filters.http.header_mutation", Config: cfg}
				}
				op = "chain filter " + s.key()
				c.SetServiceChainFilters(maps.Clone(chain))
			case 6: // imported chain filter
				s := pick()
				op = "imported chain filter " + s.key()
				c.SetImportedServiceChainFilters(map[string]proxy.ExtensionFilter{s.key(): {Name: "envoy.filters.http.header_mutation", Config: cfg}})
			case 7: // a port map
				s := pick()
				if _, ok := portMaps[s.key()]; ok {
					delete(portMaps, s.key())
				} else {
					portMaps[s.key()] = map[uint32]uint32{80: 8080, 81: []uint32{0, 9090}[rng.Intn(2)]}
				}
				op = "port map " + s.key()
				c.SetServicePortMaps(maps.Clone(portMaps))
			case 8: // a route target port
				s := pick()
				targetPorts[s.key()] = []uint32{[]uint32{80, 8080, 1234}[rng.Intn(3)]}
				op = "target port " + s.key()
				c.SetRouteTargetPorts(maps.Clone(targetPorts))
			case 9: // a Service appears / disappears / a bare name is shared
				if rng.Intn(2) == 0 && len(projected) > 2 {
					i := rng.Intn(len(projected))
					projected = slices.Delete(slices.Clone(projected), i, i+1)
				} else {
					s := *pick()
					s.ns = []string{"other", "third", a043NS}[rng.Intn(3)]
					if !slices.ContainsFunc(projected, func(o a043Service) bool { return o.key() == s.key() }) {
						projected = append(slices.Clone(projected), s)
					}
				}
				op = "project"
				a043Project(c, projected)
			case 10: // observed dependency
				s := pick()
				op = "observe " + s.key()
				c.ObserveDependency(ctx, s.key())
			case 11: // a service leaves the registry
				if len(services) > 3 {
					i := rng.Intn(len(services))
					op = "leave " + services[i].key()
					services = slices.Delete(slices.Clone(services), i, i+1)
					require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
				}
			case 12: // TLS / TCP routes
				s := pick()
				op = "tls route " + s.key()
				c.SetTLSServiceRoutes(shpTLSRoutes(s.key()))
			}
			trail = append(trail, op)
			require.NoError(t, c.generateSnapshot(ctx))

			c.snapshotMu.Lock()
			ports := c.captureHTTPPorts()
			memo := c.captureRouteTables(ports)
			c.captureTables = captureTablesMemo{}
			fresh := c.captureRouteTables(ports)
			c.snapshotMu.Unlock()
			if name, ok := shpTablesEqual(memo, fresh); !ok {
				t.Fatalf("seed %d step %d: the memoized table %s differs from a fresh build after:\n  %s", seed, step, name, strings.Join(trail, "\n  "))
			}
			// and nothing the proxy would reject
			for _, r := range fresh {
				rc := r.(*routev3.RouteConfiguration)
				seen := map[string]string{}
				for _, vh := range rc.GetVirtualHosts() {
					for _, d := range vh.GetDomains() {
						if prev, dup := seen[d]; dup {
							t.Fatalf("seed %d step %d: table %s: domain %q claimed by %q and %q after:\n  %s", seed, step, rc.GetName(), d, prev, vh.GetName(), strings.Join(trail, "\n  "))
						}
						seen[d] = vh.GetName()
					}
				}
			}
			// The published capture listener must be what a fresh build makes
			// NOW: a missed rebuild trigger leaves chains of a stale port set.
			c.listenerMu.Lock()
			var pod *cniv1.CNIPod
			var published types.Resource
			for _, e := range c.listeners {
				pod, published = e.cniPod, e.capture
			}
			c.listenerMu.Unlock()
			freshL, err := c.generateCaptureListener(pod, a043Mesh, c.podExtensionHTTPFilters(pod, c.extensionHTTPFilters()))
			require.NoError(t, err)
			sig := func(r types.Resource) string {
				var b strings.Builder
				for _, fc := range r.(*listenerv3.Listener).GetFilterChains() {
					b.WriteString(fc.GetName() + "=" + fc.GetFilterChainMatch().String() + ";")
				}
				return b.String()
			}
			if sig(published) != sig(freshL) {
				t.Fatalf("seed %d step %d: the published capture listener is STALE.\n published: %v\n fresh:     %v\nafter:\n  %s", seed, step, chainNames(published.(*listenerv3.Listener)), chainNames(freshL.(*listenerv3.Listener)), strings.Join(trail, "\n  "))
			}
			snap, err := c.GetSnapshot("node-1")
			require.NoError(t, err)
			if d := duplicateDomains(snap.GetResources("type.googleapis.com/envoy.config.route.v3.RouteConfiguration")); len(d) > 0 {
				sort.Strings(d)
				t.Fatalf("seed %d step %d: duplicate domains in the snapshot %v after:\n  %s", seed, step, d, strings.Join(trail, "\n  "))
			}
			for name, res := range snap.GetResources("type.googleapis.com/envoy.config.listener.v3.Listener") {
				if !strings.HasPrefix(name, "capture_") {
					continue
				}
				if l, ok := res.(*listenerv3.Listener); ok {
					if d := shpDuplicateChainMatches(l); len(d) > 0 {
						t.Fatalf("seed %d step %d: duplicate chain matches %v after:\n  %s", seed, step, d, strings.Join(trail, "\n  "))
					}
					// every cap_http_<P> chain has its table in the same snapshot
					for _, fc := range l.GetFilterChains() {
						if strings.HasPrefix(fc.GetName(), proxy.CaptureHTTPRoutePrefix) {
							if _, ok := snap.GetResources("type.googleapis.com/envoy.config.route.v3.RouteConfiguration")[fc.GetName()]; !ok {
								t.Fatalf("seed %d step %d: chain %s has no route table in the snapshot after:\n  %s", seed, step, fc.GetName(), strings.Join(trail, "\n  "))
							}
						}
					}
				}
			}
		}
	}
}

var _ = capture.CaptureTCPService{}

// TestPerPort_UndeclaredPortOfARawTCPOnlyService: what a dial to an
// undeclared port of a raw-TCP-only service selects. Informational: it logs
// with the any-port chain (head dabf2b0b) and, run under the local mutation
// `AnyPort: false`, what the owner's ruling (remove the chain) leaves.
func TestPerPort_UndeclaredPortOfARawTCPOnlyService(t *testing.T) {
	c := a043Cache(t, a043Fixture())
	l := a043CaptureListener(t, c)
	tables := a043Tables(t, c)
	const vip = "10.96.1.7" // tcponly: 9000 raw TCP
	for _, p := range []uint32{9000, 18082, 5555, 80, 8080, 18081, 443} {
		raw := shpSelectChain(l, shpRaw(vip, p))
		http := shpSelectChain(l, shpHTTP(vip, p))
		line := fmt.Sprintf("tcponly:%-5d raw TCP -> %-55s HTTP -> %s", p, raw, http)
		if tb, ok := tables[http]; ok {
			ans := a043Resolve(t, tb, fmt.Sprintf("tcponly.demo.%s:%d", a043Mesh, p))
			line += fmt.Sprintf(" => vhost=%s status=%d %s cluster=%q", ans.vhost, ans.status, ans.refusal, ans.cluster)
			assert.Truef(t, ans.status == 421 || strings.Contains(ans.cluster, "tcponly"), "HTTP to tcponly:%d reached %q", p, ans.cluster)
		}
		t.Log(line)
		// Never another service's chain.
		for _, got := range []string{raw, http} {
			if strings.HasPrefix(got, "cap_tcp_") || strings.HasPrefix(got, "cap_tls_") {
				assert.Containsf(t, got, "tcponly", "a dial to tcponly:%d selected another service's chain %q", p, got)
			}
		}
	}
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			_ = f
		}
	}
}

// TestPerPort_NamedTargetPortOfARouteParent: a user Service that owns a
// mesh service's name, is the parent of an HTTPRoute, and whose Service port
// has a NAMED targetPort (`targetPort: http`, the common spelling). The port
// map cannot resolve it (pod port 0). What does a request to that Service port
// get?
func TestPerPort_NamedTargetPortOfARouteParent(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	services := append(a043Fixture(), a043Service{name: "web", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.1.20", vip: "10.96.1.20"})
	c := a043Cache(t, a043Fixture()) // "web" has NO generated Service: a user Service owns the name
	mesh := "web.demo." + a043Mesh
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/web": {{
		ParentPort: 8080,
		Matches:    []proxy.GammaMatch{{Prefix: "/"}},
		Backends:   []proxy.GammaBackend{{Service: "demo/web", Cluster: mesh, Weight: 1, Port: 8080}},
	}}})
	c.SetRouteTargetPorts(map[string][]uint32{"demo/web": {8080}})
	// Service port 8080 -> targetPort "http" (a name): pod port unknown, 0.
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {8080: 0}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)
	for _, host := range []string{"web.demo.svc.cluster.local:8080", "web.demo.svc.cluster.local", "web.demo:8080", "web:8080"} {
		ans := a043Resolve(t, tables["cap_http_8080"], host)
		t.Logf("dial :8080, Host %-34s -> vhost=%s cluster=%q status=%d refusal=%q", host, ans.vhost, ans.cluster, ans.status, ans.refusal)
		assert.Zerof(t, ans.status, "Host %s: refused with %d %s (%s)", host, ans.status, ans.refusal, strings.TrimSpace(ans.body))
	}
}

func TestPerPort_PrintClusters(t *testing.T) {
	c := a043Cache(t, a043Fixture())
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	names := slices.Sorted(maps.Keys(snap.GetResources("type.googleapis.com/envoy.config.cluster.v3.Cluster")))
	t.Logf("clusters:\n%s", strings.Join(names, "\n"))
	for typ, n := range map[string]string{"lds": "type.googleapis.com/envoy.config.listener.v3.Listener", "rds": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration", "ecds": "type.googleapis.com/envoy.config.core.v3.TypedExtensionConfig", "sds": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"} {
		t.Logf("%s: %v", typ, slices.Sorted(maps.Keys(snap.GetResources(n))))
	}
}
