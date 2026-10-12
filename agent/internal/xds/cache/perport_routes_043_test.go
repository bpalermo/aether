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

// An HTTPRoute whose parentRef names port 80 (or 18081) of a GENERATED
// Service: both are ports that Service has (the legacy ones), they lead to the
// legacy target, and a backendRef naming them is resolved to the default
// cluster. A parentRef naming them attaches the rule to nothing.
func TestPerPort_ParentRefLegacyServicePort(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/httponly": {{ParentPort: 80, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("p80")}},
		"demo/multi":    {{ParentPort: 18081, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("p18081")}},
	})
	c.SetRouteTargetPorts(map[string][]uint32{"demo/httponly": {80}, "demo/multi": {18081}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)

	// The client dials http://httponly.demo/ (port 80 of the generated Service).
	assert.Equal(t, []string{"p80"}, rtpMarks(t, tables["cap_http_80"], "httponly.demo"), "captured, dial :80, port-less Host")
	assert.Equal(t, []string{"p80"}, rtpMarks(t, tables["cap_http_80"], "httponly.demo.svc.cluster.local:80"), "captured, dial :80, Host :80")
	// Loopback, the port-less mesh name (what main attached every rule to).
	assert.Equal(t, []string{"p80"}, rtpMarks(t, tables[proxy.OutboundHTTPRouteName], "httponly.demo.aether.internal"), "loopback, port-less")
	// 18081.
	assert.Equal(t, []string{"p18081"}, rtpMarks(t, tables["cap_http_18081"], "multi.demo.aether.internal:18081"), "captured, dial :18081")
	assert.Equal(t, []string{"p18081"}, rtpMarks(t, tables[proxy.OutboundHTTPRouteName], "multi.demo.aether.internal"), "loopback, port-less")
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

// A backendRef port of a USER Service that cannot be resolved to a pod port
// (the Service has no such port, or its targetPort is a name) keeps the
// cluster the route had before backendRef ports were read, the backend's
// default one. It was rendered as "<fqdn>:<SERVICE port>", which is the
// cluster of another port whenever the pods declare that number as HTTP: the
// request was then answered by a port the route never named.
func TestPerPort_UnresolvableBackendPortNeverNamesAnotherPort(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	for name, tc := range map[string]struct {
		portMap map[uint32]uint32
		port    uint32
	}{
		// Service multi: port 9090 -> targetPort "admin" (a name).
		"named targetPort": {map[uint32]uint32{9090: 0}, 9090},
		// Service multi: only port 80 -> 9090. The route names 8080, which
		// the Service does not have (and the pods do).
		"no such Service port": {map[uint32]uint32{80: 9090}, 8080},
		// The mesh HTTP port of a user Service that has no such port.
		"18081 of a user Service": {map[uint32]uint32{80: 9090}, 18081},
	} {
		t.Run(name, func(t *testing.T) {
			c := a043Cache(t, services)
			c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/multi": tc.portMap})
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{
				"demo/httponly": {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: "multi.demo.aether.internal", Port: tc.port, Weight: 1}}}},
			})
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
			got := a043Resolve(t, a043Tables(t, c)["cap_http_8080"], "httponly.demo.aether.internal:8080")
			assert.Equal(t, "multi.demo.aether.internal", got.cluster, "the projected cluster, not the cluster of whatever pod port has the Service port's number")
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
				rtpConsistent(t, c, "moved (old entries retained)")
				c.serviceRetentionGrace = 1
				load(to)
				if to != "absent" {
					rtpConsistent(t, c, "past the grace")
				}
			})
		}
	}
}

// A backend this node holds no endpoint of resolves to the projected
// (default) cluster; when its endpoints arrive the route must lead to the
// port the backendRef names, in both tables, without any route event.
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
		assert.Equal(t, "be.demo.aether.internal", got.cluster, table+": cold")
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

// The captured and the loopback table give one answer for a route once the
// refresh a route event triggers has run.
func TestPerPort_TablesAgreeAfterARouteEventAlone(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/httponly": {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: "multi.demo.aether.internal", Port: 9090, Weight: 1}}}},
	})
	// A route event signals a dependency change, and the refresh that follows
	// rebuilds the loopback table's virtual hosts (they live on the cluster
	// entries). The two tables are compared after it.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)
	capGot := a043Resolve(t, tables["cap_http_8080"], "httponly.demo.aether.internal:8080")
	outGot := a043Resolve(t, tables[proxy.OutboundHTTPRouteName], "httponly.demo.aether.internal:8080")
	t.Logf("captured: %s; loopback: %s", capGot.cluster, outGot.cluster)
	assert.Equal(t, capGot.cluster, outGot.cluster, "one snapshot, one answer for the two entry paths")
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
