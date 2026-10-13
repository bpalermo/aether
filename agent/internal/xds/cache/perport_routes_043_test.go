package cache

// Per-port addressing (proposal 043): where a route's rules and backends land.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rtpH   = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	rtpTCP = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	rtpUDP = registryv1.PortProtocol_PORT_PROTOCOL_UDP
)

// rtpPod is one pod of a service.
type rtpPod struct {
	svc    string // "<ns>/<name>"
	ip     string
	legacy uint32
	ports  map[uint32]registryv1.PortProtocol
}

// rtpRegistry lists every pod under every protocol it serves a port of, with
// the same row (what both backends do after this PR). A pod with no ports map
// is listed under HTTP only.
func rtpRegistry(pods []rtpPod) *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			out := map[string][]*registryv1.ServiceEndpoint{}
			for _, p := range pods {
				ep := makeEndpoint(p.ip, "cluster-1", "node-2", p.legacy)
				ns, _, _ := strings.Cut(p.svc, "/")
				ep.KubernetesMetadata.Namespace = ns
				ep.PortProtocols = p.ports
				serves := len(p.ports) == 0 && protocol == registryv1.Service_PROTOCOL_HTTP
				for port, class := range p.ports {
					ep.Ports = append(ep.Ports, port)
					if registryv1.Service_Protocol(class) == protocol {
						serves = true
					}
				}
				slices.Sort(ep.Ports)
				if serves {
					out[p.svc] = append(out[p.svc], ep)
				}
			}
			return out, nil
		},
	}
}

func rtpMarks(t *testing.T, table *routev3.RouteConfiguration, host string) []string {
	t.Helper()
	require.NotNil(t, table)
	var out []string
	for _, vh := range table.GetVirtualHosts() {
		if !slices.Contains(vh.GetDomains(), host) {
			continue
		}
		for _, r := range vh.GetRoutes() {
			for _, h := range r.GetResponseHeadersToAdd() {
				if h.GetHeader().GetKey() == "x-rv" {
					out = append(out, h.GetHeader().GetValue())
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

func rtpMark(v string) *proxy.GammaHeaderMutation {
	return &proxy.GammaHeaderMutation{AddResponse: []proxy.GammaHeaderKV{{Name: "x-rv", Value: v}}}
}

// An HTTPRoute whose parentRef names port 80 or 18081 of a GENERATED Service
// that does not declare it: neither is a port of the service (until chart
// 3.0.0 both meant "the port the pods declare"). The rule attaches to NOTHING,
// on either path, and the service answers as if the route did not exist. A
// parentRef that names a real port still lands on it (the control).
func TestPerPort_ParentRefThatIsNotAPortAttachesNowhere(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/httponly": {{ParentPort: 80, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("p80")}},
		"demo/multi": {
			{ParentPort: 18081, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("p18081")},
			{ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("p9090")},
		},
		// eighty DECLARES port 80: there it is a real port.
		"demo/eighty": {{ParentPort: 80, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("e80")}},
	})
	c.SetRouteTargetPorts(map[string][]uint32{"demo/httponly": {80}, "demo/multi": {18081, 9090}, "demo/eighty": {80}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)

	for _, tc := range []struct{ table, host string }{
		{"cap_http_80", "httponly.demo"},
		{"cap_http_80", "httponly.demo.svc.cluster.local:80"},
		{"cap_http_8080", "httponly.demo.aether.internal:8080"},
		{proxy.OutboundHTTPRouteName, "httponly.demo.aether.internal"},
		{proxy.OutboundHTTPRouteName, "httponly.demo.aether.internal:8080"},
		{"cap_http_18081", "multi.demo.aether.internal:18081"},
		{"cap_http_8080", "multi.demo.aether.internal:8080"},
		{proxy.OutboundHTTPRouteName, "multi.demo.aether.internal"},
		{proxy.OutboundHTTPRouteName, "multi.demo.aether.internal:8080"},
	} {
		assert.Emptyf(t, rtpMarks(t, tables[tc.table], tc.host), "%s, Host %s: a rule whose parentRef names no port of the service applied", tc.table, tc.host)
	}
	// The controls: a real port.
	assert.Equal(t, []string{"p9090"}, rtpMarks(t, tables["cap_http_9090"], "multi.demo.aether.internal:9090"))
	assert.Equal(t, []string{"p9090"}, rtpMarks(t, tables[proxy.OutboundHTTPRouteName], "multi.demo.aether.internal:9090"))
	assert.Equal(t, []string{"e80"}, rtpMarks(t, tables["cap_http_80"], "eighty.demo.aether.internal"))
	assert.Equal(t, []string{"e80"}, rtpMarks(t, tables[proxy.OutboundHTTPRouteName], "eighty.demo.aether.internal:80"))
}

// A rule imported from a peer cluster (026) carries neither port. It now
// applies on EVERY HTTP port of the parent, and sends each to the backend's
// default cluster: a request to multi:9090 is answered by another service's
// legacy port. On main the per-port virtual host carried no rule.
func TestPerPort_ImportedRuleTakesEveryPort(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{
		// Authored in the peer as: parentRefs [{name: multi, port: 8080}],
		// backendRefs [{name: httponly, port: 8080}].
		"demo/multi": {{Backends: []proxy.GammaBackend{{Service: "demo/httponly", Cluster: "httponly.demo.aether.internal", Weight: 1}}}},
	})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)
	const multi = "multi.demo.aether.internal"
	for _, table := range []string{"cap_http_9090", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, tables[table], multi+":9090")
		assert.Equalf(t, multi+":9090", got.cluster, "%s: port 9090 of multi, which the peer's route never named: %+v", table, got)
	}
}

// A backendRef that names no HTTP port of its Service is INVALID: the request
// is answered 500 (the Gateway API's answer for an invalid backendRef), on
// both paths, and is never delivered to another port. Until chart 3.0.0 the
// mesh HTTP port and an undeclared 80 meant "the port the pods declare", and
// an unresolvable port fell back to the backend's default cluster: a request
// was answered by a port the route never named. A NAMED targetPort that has
// not resolved to a pod port was the one case left that did (the backend's
// default cluster); by the owner's ruling of 2026-10-13 it is invalid like
// the others until it resolves.
func TestPerPort_InvalidBackendRefAnswers500(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	const multi = "multi.demo.aether.internal"
	for name, tc := range map[string]struct {
		backendSvc string
		portMap    map[uint32]uint32
		port       uint32
		want       uint32 // 500, or 0: routed to wantCluster
		cluster    string
	}{
		"generated Service: a declared HTTP port (control)": {"demo/multi", nil, 9090, 0, multi + ":9090"},
		"generated Service: an undeclared port":             {"demo/multi", nil, 7777, 500, ""},
		"generated Service: the mesh HTTP port":             {"demo/multi", nil, 18081, 500, ""},
		"generated Service: an undeclared 80":               {"demo/multi", nil, 80, 500, ""},
		"generated Service: a raw-TCP port":                 {"demo/mixed", nil, 9000, 500, ""},
		"user Service: a port that maps to an HTTP port":    {"demo/multi", map[uint32]uint32{80: 9090}, 80, 0, multi + ":9090"},
		"user Service: no such port, though pods serve it":  {"demo/multi", map[uint32]uint32{80: 9090}, 8080, 500, ""},
		"user Service: 18081":                               {"demo/multi", map[uint32]uint32{80: 9090}, 18081, 500, ""},
		"user Service: a targetPort the pods do not serve":  {"demo/multi", map[uint32]uint32{443: 8443}, 443, 500, ""},
		"user Service: a named targetPort, not resolved":    {"demo/multi", map[uint32]uint32{9090: 0}, 9090, 500, ""},
		"user Service: a named targetPort, resolved":        {"demo/multi", map[uint32]uint32{9090: 9090}, 9090, 0, multi + ":9090"},
	} {
		t.Run(name, func(t *testing.T) {
			c := a043Cache(t, services)
			if tc.portMap != nil {
				c.SetServicePortMaps(map[string]map[uint32]uint32{tc.backendSvc: tc.portMap})
			}
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{
				"demo/httponly": {{Backends: []proxy.GammaBackend{{Service: tc.backendSvc, Cluster: proxy.ServiceClusterName(tc.backendSvc, a043Mesh), Port: tc.port, Weight: 1}}}},
			})
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
			tables := a043Tables(t, c)
			for _, at := range []struct{ table, host string }{
				{"cap_http_8080", "httponly.demo.aether.internal:8080"},
				{proxy.OutboundHTTPRouteName, "httponly.demo.aether.internal:8080"},
				// The proxy port on the captured path (a dial to 18081).
				{"cap_http_18081", "httponly.demo.aether.internal:8080"},
			} {
				got := a043Resolve(t, tables[at.table], at.host)
				assert.Equalf(t, tc.want, got.status, "%s, Host %s: %+v", at.table, at.host, got)
				if tc.want == 0 {
					assert.Equalf(t, tc.cluster, got.cluster, "%s, Host %s", at.table, at.host)
				} else {
					assert.Emptyf(t, got.cluster, "%s, Host %s: an invalid backendRef was routed", at.table, at.host)
				}
			}
		})
	}
}

// TestPerPort_InvalidBackendRefIsWeighted: a rule with one valid and one
// invalid backendRef answers 500 for the invalid one's share of the weight and
// sends the rest to the valid backend alone. With weight 0 the invalid one
// takes nothing.
func TestPerPort_InvalidBackendRefIsWeighted(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	const multi = "multi.demo.aether.internal"
	for _, tc := range []struct {
		name           string
		valid, invalid uint32
		wantPPM        uint32 // of 1_000_000 requests answered 500; 0 = no 500 route
	}{
		{"one in four", 3, 1, 250_000},
		{"nine in ten", 1, 9, 900_000},
		{"an invalid backendRef of weight 0 takes nothing", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := a043Cache(t, services)
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/httponly": {{Backends: []proxy.GammaBackend{
				{Service: "demo/multi", Cluster: multi, Port: 9090, Weight: tc.valid},
				{Service: "demo/multi", Cluster: multi, Port: 7777, Weight: tc.invalid},
			}}}})
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
			for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
				vh := findVHost(a043Tables(t, c)[table].GetVirtualHosts(), "httponly.demo.aether.internal:8080")
				require.NotNil(t, vh, table)
				var refused, routed []*routev3.Route
				for _, r := range vh.GetRoutes() {
					switch {
					case r.GetDirectResponse().GetStatus() == 500:
						refused = append(refused, r)
					case r.GetRoute() != nil:
						routed = append(routed, r)
					}
				}
				if tc.wantPPM == 0 {
					assert.Empty(t, refused, table)
				} else if assert.Len(t, refused, 1, table) {
					frac := refused[0].GetMatch().GetRuntimeFraction().GetDefaultValue()
					assert.Equal(t, tc.wantPPM, frac.GetNumerator(), table)
					assert.Equal(t, "MILLION", frac.GetDenominator().String(), table)
					assert.Same(t, vh.GetRoutes()[0], refused[0], "the 500 stands before the rule's own route")
				}
				// The rule's own route names the valid backend alone, and no
				// route of the virtual host names a cluster of the invalid port.
				require.NotEmpty(t, routed, table)
				assert.Equal(t, multi+":9090", routed[0].GetRoute().GetCluster(), table)
				for _, r := range routed {
					for _, wc := range r.GetRoute().GetWeightedClusters().GetClusters() {
						assert.NotContains(t, wc.GetName(), "7777", table)
					}
				}
			}
		})
	}
}

func rtpCLAIPs(t *testing.T, res any) []string {
	t.Helper()
	cla, ok := res.(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok)
	var ips []string
	for _, l := range cla.GetEndpoints() {
		for _, e := range l.GetLbEndpoints() {
			ips = append(ips, e.GetEndpoint().GetAddress().GetSocketAddress().GetAddress())
		}
	}
	slices.Sort(ips)
	return ips
}

// "HTTP clusters and endpoint sets are built per (port, class)". The
// default cluster's endpoint set is every row of the HTTP listing, whatever
// the row says about the default port.
func TestPerPort_DefaultClusterMembershipIgnoresClass(t *testing.T) {
	ctx := context.Background()
	pods := []rtpPod{
		{svc: "demo/split", ip: "10.0.8.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH}},
		{svc: "demo/split", ip: "10.0.8.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH}},
		// Declares 8080 raw TCP; is in the HTTP listing for its port 7070.
		{svc: "demo/split", ip: "10.0.8.3", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: rtpTCP, 8080: rtpTCP, 7070: rtpH}},
	}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, []a043Service{{name: "split", ns: a043NS, vip: "10.96.8.1"}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(pods)))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	cl, ok := snap.GetResources(resourcev3.ClusterType)["split.demo.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "the default cluster (legacy target 8080, HTTP by 2 votes to 1)")
	eds := cl.GetEdsClusterConfig().GetServiceName()
	ips := rtpCLAIPs(t, snap.GetResources(resourcev3.EndpointType)[eds])
	t.Logf("default cluster %s (SNI = legacy port) EDS %q members: %v", cl.GetName(), eds, ips)
	assert.NotContains(t, ips, "10.0.8.3", "a pod that declares 8080 raw TCP is a member of the h2 cluster of port 8080")
}

// rtpConsistent asserts what the proxy needs of one snapshot.
func rtpConsistent(t *testing.T, c *SnapshotCache, what string) {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)
	endpoints := snap.GetResources(resourcev3.EndpointType)
	for name, res := range snap.GetResources(resourcev3.RouteType) {
		rc := res.(*routev3.RouteConfiguration)
		seen := map[string]string{}
		for _, vh := range rc.GetVirtualHosts() {
			assert.NotEmptyf(t, vh.GetDomains(), "%s: table %s: virtual host %s has no domain", what, name, vh.GetName())
			for _, d := range vh.GetDomains() {
				if prev, dup := seen[d]; dup {
					t.Errorf("%s: table %s: domain %q claimed by %q and %q", what, name, d, prev, vh.GetName())
				}
				seen[d] = vh.GetName()
			}
			if vh.GetName() == "on_demand_catch_all" {
				continue
			}
			for _, r := range vh.GetRoutes() {
				ra := r.GetRoute()
				if ra == nil {
					continue
				}
				cluster := ra.GetCluster()
				if cluster == "" {
					cluster = quicSelectionDefault(t, ra)
				}
				if _, ok := clusters[cluster]; !ok {
					t.Errorf("%s: table %s vhost %s (%v) routes to %q, not published", what, name, vh.GetName(), vh.GetDomains(), cluster)
				}
			}
		}
		names := map[string]bool{}
		for _, vh := range rc.GetVirtualHosts() {
			if names[vh.GetName()] {
				t.Errorf("%s: table %s: two virtual hosts named %q", what, name, vh.GetName())
			}
			names[vh.GetName()] = true
		}
	}
	for name, res := range clusters {
		cl := res.(*clusterv3.Cluster)
		if cl.GetType() != clusterv3.Cluster_EDS {
			continue
		}
		eds := cl.GetEdsClusterConfig().GetServiceName()
		if eds == "" {
			eds = name
		}
		if _, ok := endpoints[eds]; !ok {
			t.Errorf("%s: cluster %s subscribes to EDS %q, not published", what, name, eds)
		}
	}
	for _, res := range snap.GetResources(resourcev3.ListenerType) {
		l, ok := res.(*listenerv3.Listener)
		if !ok || l == nil {
			continue
		}
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				tp := &tcp_proxyv3.TcpProxy{}
				if f.GetTypedConfig().UnmarshalTo(tp) != nil || tp.GetCluster() == "" {
					continue
				}
				if _, ok := clusters[tp.GetCluster()]; !ok {
					t.Errorf("%s: listener %s chain %s forwards to %q, not published", what, l.GetName(), fc.GetName(), tp.GetCluster())
				}
			}
		}
	}
}

// rtpShapes are the states one service "demo/x" can be in.
func rtpShapes() map[string][]rtpPod {
	one := func(legacy uint32, ports map[uint32]registryv1.PortProtocol) []rtpPod {
		return []rtpPod{{svc: "demo/x", ip: "10.0.9.1", legacy: legacy, ports: ports}}
	}
	pp := func(kv ...any) map[uint32]registryv1.PortProtocol {
		m := map[uint32]registryv1.PortProtocol{}
		for i := 0; i < len(kv); i += 2 {
			m[uint32(kv[i].(int))] = kv[i+1].(registryv1.PortProtocol)
		}
		return m
	}
	return map[string][]rtpPod{
		"absent":            nil,
		"http8080":          one(8080, pp(8080, rtpH)),
		"http8080+18081h":   one(8080, pp(8080, rtpH, 18081, rtpH)),
		"legacy18081h":      one(18081, pp(18081, rtpH)),
		"http8080+18081tcp": one(8080, pp(8080, rtpH, 18081, rtpTCP)),
		"http8080+18082h":   one(8080, pp(8080, rtpH, 18082, rtpH)),
		"legacy18082h":      one(18082, pp(18082, rtpH)),
		"http8080+80tcp":    one(8080, pp(8080, rtpH, 80, rtpTCP)),
		"legacy80tcp+8080h": one(80, pp(80, rtpTCP, 8080, rtpH)),
		"legacy80h+18081h":  one(80, pp(80, rtpH, 18081, rtpH)),
		"legacyudp+8080h":   one(5353, pp(5353, rtpUDP, 8080, rtpH)),
		"legacy18081tcp+h":  one(18081, pp(18081, rtpTCP, 8080, rtpH)),
		"legacy18082tcp+h":  one(18082, pp(18082, rtpTCP, 8080, rtpH)),
		"tcponly9000":       one(9000, pp(9000, rtpTCP)),
		"tcponly18081":      one(18081, pp(18081, rtpTCP)),
		"udponly":           one(5353, pp(5353, rtpUDP)),
		"noports":           one(0, nil),
		"legacy9090of2":     one(9090, pp(8080, rtpH, 9090, rtpH)),
		"legacy8080of2":     one(8080, pp(8080, rtpH, 9090, rtpH)),
		"8080tcp+9090h":     one(8080, pp(8080, rtpTCP, 9090, rtpH)),
		"split-tcp-wins": {
			{svc: "demo/x", ip: "10.0.9.1", legacy: 8080, ports: pp(8080, rtpH)},
			{svc: "demo/x", ip: "10.0.9.2", legacy: 9000, ports: pp(9000, rtpTCP, 8080, rtpTCP)},
			{svc: "demo/x", ip: "10.0.9.3", legacy: 9000, ports: pp(9000, rtpTCP, 8080, rtpTCP)},
		},
		"split-http-wins": {
			{svc: "demo/x", ip: "10.0.9.1", legacy: 8080, ports: pp(8080, rtpH)},
			{svc: "demo/x", ip: "10.0.9.2", legacy: 8080, ports: pp(8080, rtpH)},
			{svc: "demo/x", ip: "10.0.9.3", legacy: 9000, ports: pp(9000, rtpTCP, 8080, rtpTCP, 7070, rtpH)},
		},
		// The legacy pair vote says (8080, HTTP); the class vote for 8080 says TCP.
		"split-three-way": {
			{svc: "demo/x", ip: "10.0.9.1", legacy: 8080, ports: pp(8080, rtpH)},
			{svc: "demo/x", ip: "10.0.9.2", legacy: 9000, ports: pp(9000, rtpTCP, 8080, rtpTCP)},
			{svc: "demo/x", ip: "10.0.9.3", legacy: 9001, ports: pp(9001, rtpTCP, 8080, rtpTCP)},
		},
		"split-18081": {
			{svc: "demo/x", ip: "10.0.9.1", legacy: 8080, ports: pp(8080, rtpH, 18081, rtpH)},
			{svc: "demo/x", ip: "10.0.9.2", legacy: 8080, ports: pp(8080, rtpH, 18081, rtpTCP)},
		},
		"split-legacy-port": {
			{svc: "demo/x", ip: "10.0.9.1", legacy: 8080, ports: pp(8080, rtpH)},
			{svc: "demo/x", ip: "10.0.9.2", legacy: 9090, ports: pp(9090, rtpH)},
		},
	}
}

// Every state, and every move from one state to another (with the old
// entries retained, then past the grace), yields a snapshot the proxy accepts
// and in which every reference resolves.
func TestPerPort_EveryStateAndTransitionIsConsistent(t *testing.T) {
	ctx := context.Background()
	shapes := rtpShapes()
	names := make([]string, 0, len(shapes))
	for n := range shapes {
		names = append(names, n)
	}
	slices.Sort(names)
	others := []rtpPod{
		{svc: "demo/other", ip: "10.0.7.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH, 9000: rtpTCP}},
		{svc: "demo/xy", ip: "10.0.7.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH}},
	}
	project := []a043Service{
		{name: "x", ns: a043NS, vip: "10.96.9.1"},
		{name: "other", ns: a043NS, vip: "10.96.7.1"},
		{name: "xy", ns: a043NS, vip: "10.96.7.2"},
	}
	for _, from := range names {
		for _, to := range names {
			if from == to || from == "noports" || to == "noports" {
				continue
			}
			t.Run(from+"->"+to, func(t *testing.T) {
				c := a043Cache(t, nil)
				a043Project(c, project)
				load := func(shape string) {
					require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(append(slices.Clone(others), shapes[shape]...))))
				}
				load(from)
				if from != "absent" {
					rtpConsistent(t, c, "first")
				}
				load(to)
				// A service whose pods declared nothing but a port reserved for
				// the mesh had no entry to retain: gone, it is at once a name
				// nothing is known of, like any absent service.
				reservedOnly := from == "legacy18081h" || from == "legacy18082h" || from == "tcponly18081"
				if to != "absent" || !reservedOnly {
					rtpConsistent(t, c, "moved (old entries retained)")
				}
				c.serviceRetentionGrace = 1
				load(to)
				if to != "absent" {
					rtpConsistent(t, c, "past the grace")
				}
			})
		}
	}
}

// A backend this node holds no endpoint of is named by the port the
// backendRef names from the start (its cluster is fetched on demand), in both
// tables: never by the bare name, which is the cluster of the port the
// backend's pods declare and answered a route to 9090 from 8080 until the
// endpoints arrived. Nothing changes when they do.
func TestPerPort_BackendKnownLater(t *testing.T) {
	ctx := context.Background()
	parent := []rtpPod{{svc: "demo/parent", ip: "10.0.6.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH}}}
	backend := []rtpPod{{svc: "demo/be", ip: "10.0.6.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH, 9090: rtpH}}}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{{name: "parent", ns: a043NS, vip: "10.96.6.1"}, {name: "be", ns: a043NS, vip: "10.96.6.2"}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/parent": {{Backends: []proxy.GammaBackend{{Service: "demo/be", Cluster: "be.demo.aether.internal", Port: 9090, Weight: 1}}}},
	})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(parent)))
	for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, a043Tables(t, c)[table], "parent.demo.aether.internal:8080")
		assert.Equal(t, "be.demo.aether.internal:9090", got.cluster, table+": cold")
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(append(slices.Clone(parent), backend...))))
	for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, a043Tables(t, c)[table], "parent.demo.aether.internal:8080")
		assert.Equal(t, "be.demo.aether.internal:9090", got.cluster, table+": the backend's endpoints arrived")
	}
	// The port map of the backend changes alone (a user Service appears over it).
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/be": {9090: 8080}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(append(slices.Clone(parent), backend...))))
	for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, a043Tables(t, c)[table], "parent.demo.aether.internal:8080")
		assert.Equal(t, "be.demo.aether.internal", got.cluster, table+": Service port 9090 is now pod port 8080, the default cluster")
	}
	// The backend's port changes class.
	backend[0].ports = map[uint32]registryv1.PortProtocol{8080: rtpH, 9090: rtpTCP}
	c.SetServicePortMaps(nil)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(append(slices.Clone(parent), backend...))))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, a043Tables(t, c)[table], "parent.demo.aether.internal:8080")
		t.Logf("%s after 9090 became raw TCP: %s (published: %v)", table, got.cluster, snap.GetResources(resourcev3.ClusterType)[got.cluster] != nil)
	}
}

// The captured and the loopback table give one answer for a route in the very
// snapshot a route event produces, with NO registry refresh in between. The
// loopback table's virtual hosts live on the cluster entries, which a refresh
// builds; they are rebuilt when the rule inputs change as well
// (refreshEntryVhosts). Without that the capture table had the new rule and
// the loopback table the old one until the next refresh.
func TestPerPort_TablesAgreeAfterARouteEventAlone(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/httponly": {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: "multi.demo.aether.internal", Port: 9090, Weight: 1}}}},
	})
	require.NoError(t, c.generateSnapshot(ctx))
	tables := a043Tables(t, c)
	capGot := a043Resolve(t, tables["cap_http_8080"], "httponly.demo.aether.internal:8080")
	outGot := a043Resolve(t, tables[proxy.OutboundHTTPRouteName], "httponly.demo.aether.internal:8080")
	t.Logf("captured: %s; loopback: %s", capGot.cluster, outGot.cluster)
	assert.Equal(t, "multi.demo.aether.internal:9090", capGot.cluster, "the route's backendRef")
	assert.Equal(t, capGot.cluster, outGot.cluster, "one snapshot, one answer for the two entry paths")

	// And the route going away, alone.
	c.SetServiceRoutes(nil)
	require.NoError(t, c.generateSnapshot(ctx))
	tables = a043Tables(t, c)
	for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, tables[table], "httponly.demo.aether.internal:8080")
		assert.Equal(t, "httponly.demo.aether.internal", got.cluster, "%s: the deleted route still routes", table)
	}
}

// TestPerPort_EntryVhostsAreRebuiltOnlyWhenTheRuleInputsChange: the loopback
// virtual hosts are stored so that a snapshot (one per pod event) does not
// build and hash the route table again. They are rebuilt when a rule input
// changes, and at no other time: the same objects are published until then.
func TestPerPort_EntryVhostsAreRebuiltOnlyWhenTheRuleInputsChange(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	vhostOf := func() *routev3.VirtualHost {
		c.clusterMu.RLock()
		defer c.clusterMu.RUnlock()
		return c.clusters["demo/multi"].vhost
	}
	require.NoError(t, c.generateSnapshot(ctx))
	first := vhostOf()
	require.NotNil(t, first)
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Same(t, first, vhostOf(), "nothing changed: the stored virtual host is published again")
	// An event that changes no rule input (an identical route set).
	c.SetServiceRoutes(nil)
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Same(t, first, vhostOf(), "an event that changed nothing rebuilds nothing")
	// Each kind of rule input, changed alone.
	for name, change := range map[string]func(){
		"a route": func() {
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{Matches: []proxy.GammaMatch{{Prefix: "/x"}}, HeaderMutation: rtpMark("x")}}})
		},
		"an imported route": func() {
			c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{Matches: []proxy.GammaMatch{{Prefix: "/y"}}, HeaderMutation: rtpMark("y")}}})
		},
		"a port map": func() { c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/httponly": {80: 8080}}) },
	} {
		before := vhostOf()
		change()
		require.NoError(t, c.generateSnapshot(ctx))
		assert.NotSamef(t, before, vhostOf(), "%s changed and the loopback virtual hosts were not rebuilt", name)
	}
}

var _ = fmt.Sprintf

// rtpCountingHandler counts WARN records by message prefix.
type rtpCountingHandler struct{ warns map[string]int }

func (h *rtpCountingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *rtpCountingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.warns[r.Message]++
	}
	return nil
}
func (h *rtpCountingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *rtpCountingHandler) WithGroup(string) slog.Handler      { return h }

// "Pods of one service that disagree are decided once for the whole
// service, and every builder reads that answer." With 8080 decided raw TCP
// (2 pods to 1), the capture path treats 8080 as raw TCP; the loopback table
// routes "<fqdn>:8080" as HTTP to the minority pod, the refusal that should
// answer is dropped by dropClaimedDomainsLocked, and that is logged at WARN on
// every snapshot build.
func TestPerPort_DisagreementLoopbackAndWarnPerSnapshot(t *testing.T) {
	ctx := context.Background()
	h := &rtpCountingHandler{warns: map[string]int{}}
	c := NewSnapshotCache("node-1", slog.New(h))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, []a043Service{{name: "x", ns: a043NS, vip: "10.96.9.1"}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(rtpShapes()["split-tcp-wins"])))
	c.clusterMu.RLock()
	ports := c.servicePorts["demo/x"]
	c.clusterMu.RUnlock()
	require.Equal(t, registryv1.PortProtocol_PORT_PROTOCOL_TCP, ports.classOf(8080), "decided raw TCP")
	const claim = "a refusing virtual host and a cluster's virtual host claim the same authority; keeping the cluster's"
	before := h.warns[claim]
	for range 10 {
		require.NoError(t, c.generateSnapshot(ctx)) // what every pod ADD/DEL does
	}
	t.Logf("WARN %q: %d before, %d after 10 snapshot builds with no input change", claim, before, h.warns[claim])
	got := a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "x.demo.aether.internal:8080")
	t.Logf("loopback Host x.demo.aether.internal:8080 -> %+v", got)
	assert.Equal(t, proxy.RefusalPortNotHTTP, got.refusal, "the one answer for the service is raw TCP; the loopback table routes it as HTTP")
	assert.Equal(t, before, h.warns[claim], "a WARN per snapshot build for an unchanged state")
}

// Edge: an HTTPRoute backendRef to a mesh service whose legacy target is
// raw TCP. It has no default entry any more, and edgeMeshClusterNameLocked
// decides "is this a mesh service" by that entry: anything but one of its
// per-port HTTP clusters now falls to the NON-mesh path, a cleartext
// STRICT_DNS cluster to the Kubernetes Service.
func TestPerPort_EdgeBackendOfTCPLegacyServiceLeavesTheMesh(t *testing.T) {
	ctx := context.Background()
	pods := []rtpPod{{svc: "demo/tcpfirst", ip: "10.0.1.5", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: rtpTCP, 8080: rtpH}}}
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	declareDeps(c, "demo/tcpfirst")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "edge-1", rtpRegistry(pods)))
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	var keys []string
	for k := range c.clusters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	t.Logf("entries: %v", keys)
	assert.Equal(t, "tcpfirst.demo.aether.internal:8080", c.edgeClusterNameLocked("tcpfirst", "demo", 8080), "its HTTP port")
	for _, port := range []uint32{0, 80, 9000, 18081} {
		got := c.edgeClusterNameLocked("tcpfirst", "demo", port)
		t.Logf("backendRef port %d -> %s", port, got)
		assert.Falsef(t, strings.HasPrefix(got, "edge_k8s_"), "backendRef port %d of a MESH service resolved to the cleartext Kubernetes path %q", port, got)
	}
}
