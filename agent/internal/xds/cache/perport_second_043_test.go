package cache

// Per-port addressing (proposal 043): partial listings, services the node
// holds no endpoint of, events between registry refreshes, row order, cluster
// membership, and a randomized walk over the cache's operations that holds
// the invariants after every step.

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const spFilter = "envoy.filters.http.header_mutation"

// ---------------------------------------------------------------------------
// 6. "Membership is by (port, class)" (buildHTTPServiceEntryLocked). Pods of
// one service that declare DIFFERENT ports of one class: a rollout that moves
// the declared port. The cluster of port P must hold the pods that serve P.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// A service that serves HTTP on its declared port and raw TCP on another is in
// two listings, and the listings do not arrive together (a registration lands
// one protocol at a time). Here the raw-TCP row is listed first: the service
// HAS a cluster on the node (its floor), and nothing of its HTTP side yet.
// While that lasts no authority of the service may be delivered without its
// service-wide filter: every one is refused by name (the node knows the
// service and none of its HTTP ports), and is routed, with the filter, once
// the HTTP row is listed.
// ---------------------------------------------------------------------------

func TestPerPort_Loopback_PortlessAuthorityWhileOnlyTheRawTCPRowIsListed(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	pod := a043Service{name: "mixed", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.1.4", vip: "10.96.1.4"}
	listed := []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_TCP}
	reg := &mockRegistry{tcpAware: true, listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		out := map[string][]*registryv1.ServiceEndpoint{}
		if slices.Contains(listed, protocol) {
			ep := makeEndpoint(pod.ip, "cluster-1", "node-2", pod.legacy)
			ep.KubernetesMetadata.Namespace = pod.ns
			ep.PortProtocols = pod.ports
			ep.Ports = []uint32{8080, 9000}
			out[pod.key()] = []*registryv1.ServiceEndpoint{ep}
		}
		return out, nil
	}}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client"}, a043Mesh))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, []a043Service{pod})
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{pod.key(): spChainCfg(t)})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	const mesh = "mixed.demo." + a043Mesh
	c.clusterMu.RLock()
	t.Logf("only the raw-TCP row listed: ports %s", spPortsString(c.servicePorts[pod.key()]))
	c.clusterMu.RUnlock()
	out := a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	for _, vh := range out.GetVirtualHosts() {
		t.Logf("out_http virtual host %-44s %v", vh.GetName(), vh.GetDomains())
	}
	for _, host := range []string{mesh, mesh + ":18081", mesh + ":8080", mesh + ":9000"} {
		got := spResolve(t, out, host)
		t.Logf("out_http Host %-36s -> %s", host, got)
		if got.kind == "ondemand" || got.kind == "cluster" {
			assert.Truef(t, got.filtered, "out_http, Host %s: %s routes it (the request is parked until the HTTP row is listed) WITHOUT the service's filter", host, got.vhost)
		}
	}
	// The captured path in the same state, for comparison.
	tables := spCaptureTables(c, 80, 8080, 18081)
	for _, tc := range []struct{ table, host string }{{"cap_http_18081", mesh + ":18081"}, {"cap_http_80", mesh}, {"cap_http_8080", mesh + ":8080"}} {
		got := spResolve(t, tables[tc.table], tc.host)
		t.Logf("%-15s Host %-36s -> %s", tc.table, tc.host, got)
		if got.kind == "ondemand" || got.kind == "cluster" {
			assert.Truef(t, got.filtered, "%s, Host %s: routed without the filter", tc.table, tc.host)
		}
	}
	// Control: both rows listed.
	listed = []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_TCP, registryv1.Service_PROTOCOL_HTTP}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	// (The service's HTTP port by number: a name without a port is port 80,
	// which this service does not declare, and is refused in both states.)
	got := spResolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], mesh+":8080")
	t.Logf("both rows listed: out_http Host %s -> %s", mesh+":8080", got)
	require.Equal(t, "cluster", got.kind, "control: routed once the HTTP row is listed")
	require.True(t, got.filtered, "control")
}

func spEDSName(res types.Resource) string {
	cl, ok := res.(*clusterv3.Cluster)
	if !ok {
		return ""
	}
	if n := cl.GetEdsClusterConfig().GetServiceName(); n != "" {
		return n
	}
	return cl.GetName()
}

func TestPerPort_ClusterOfAPortHoldsAPodThatDoesNotDeclareIt(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	for _, tc := range []struct {
		name    string
		class   registryv1.PortProtocol
		a, b    uint32
		cluster string
	}{
		{"http: two pods declare 8080, the new one 9090", h, 8080, 9090, "x.demo." + a043Mesh},
		{"raw tcp: two pods declare 9000, the new one 9001", tcp, 9000, 9001, "tcp:x.demo." + a043Mesh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := map[string][]spPod{"demo/x": {
				{ip: "10.0.9.1", legacy: tc.a, ports: map[uint32]registryv1.PortProtocol{tc.a: tc.class}},
				{ip: "10.0.9.2", legacy: tc.a, ports: map[uint32]registryv1.PortProtocol{tc.a: tc.class}},
				{ip: "10.0.9.3", legacy: tc.b, ports: map[uint32]registryv1.PortProtocol{tc.b: tc.class}},
			}}
			c := newTestCache("node-1")
			c.SetCaptureEnabled(true)
			c.SetCaptureRedirectAll(true)
			require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client"}, a043Mesh))
			require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
			a043Project(c, []a043Service{{name: "x", ns: "demo", vip: "10.96.9.1"}})
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", spPodsRegistry(pods, false)))
			snap, err := c.GetSnapshot("node-1")
			require.NoError(t, err)
			c.clusterMu.RLock()
			t.Logf("ports: %s", spPortsString(c.servicePorts["demo/x"]))
			c.clusterMu.RUnlock()
			for _, name := range slices.Sorted(maps.Keys(snap.GetResources(resourcev3.ClusterType))) {
				if !strings.Contains(name, "x.demo.") {
					continue
				}
				eds := spEDSName(snap.GetResources(resourcev3.ClusterType)[name])
				t.Logf("cluster %-40s eds=%-36s members=%v", name, eds, spCLAIPs(snap.GetResources(resourcev3.EndpointType)[eds]))
			}
			// What a dial to port a selects, and the members of that cluster.
			var target string
			if tc.class == h {
				got := spResolve(t, spCaptureTables(c)[proxy.CaptureHTTPRouteName(tc.a)], fmt.Sprintf("x.demo.%s:%d", a043Mesh, tc.a))
				t.Logf("HTTP dial :%d -> %s", tc.a, got)
				target = got.cluster
			} else {
				chain := shpSelectChain(a043CaptureListener(t, c), shpRaw("10.96.9.1", tc.a))
				t.Logf("raw TCP dial :%d -> chain %s", tc.a, chain)
				require.Equal(t, fmt.Sprintf("cap_tcp_%s_%d", tc.cluster, tc.a), chain)
				target = tc.cluster
			}
			require.Equal(t, tc.cluster, target)
			members := spCLAIPs(snap.GetResources(resourcev3.EndpointType)[spEDSName(snap.GetResources(resourcev3.ClusterType)[target])])
			assert.NotContainsf(t, members, "10.0.9.3", "a dial to port %d is balanced over %v: 10.0.9.3 declares only port %d, and its inbound default chain forwards to that port", tc.a, members, tc.b)
		})
	}
}

// spVhost picks the virtual host the way the proxy does: the host is
// lower-cased; an exact domain, then the longest prefix wildcard, then "*".
func spVhost(table *routev3.RouteConfiguration, host string) *routev3.VirtualHost {
	host = strings.ToLower(host)
	var exact, wildcard, catchAll *routev3.VirtualHost
	wl := -1
	for _, vh := range table.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			switch {
			case d == "*":
				catchAll = vh
			case strings.HasSuffix(d, "*"):
				if p := strings.TrimSuffix(d, "*"); strings.HasPrefix(host, p) && len(p) > wl {
					wildcard, wl = vh, len(p)
				}
			case d == host:
				exact = vh
			}
		}
	}
	switch {
	case exact != nil:
		return exact
	case wildcard != nil:
		return wildcard
	}
	return catchAll
}

// spOutcome is what a table does with "GET /" for an authority.
type spOutcome struct {
	vhost    string
	kind     string // refuse | notfound | ondemand | passthrough | cluster
	cluster  string
	reason   string
	filtered bool // the virtual host enables spFilter
	rule     bool // the route that took the request comes from a rule (it is not the virtual host's last route)
}

func (o spOutcome) String() string {
	return fmt.Sprintf("vhost=%s %s cluster=%q reason=%q filter=%v", o.vhost, o.kind, o.cluster, o.reason, o.filtered)
}

func spResolve(t *testing.T, table *routev3.RouteConfiguration, host string) spOutcome {
	t.Helper()
	vh := spVhost(table, host)
	require.NotNilf(t, vh, "table %s has no catch-all", table.GetName())
	_, filtered := vh.GetTypedPerFilterConfig()[spFilter]
	out := spOutcome{vhost: vh.GetName(), filtered: filtered}
	routes := vhostRoutes(t, vh)
	for i, r := range routes {
		m := r.GetMatch()
		if p := m.GetPath(); p != "" && p != "/" {
			continue
		}
		if p := m.GetPrefix(); p != "" && p != "/" {
			continue
		}
		if m.GetSafeRegex() != nil || m.GetPathSeparatedPrefix() != "" {
			continue
		}
		ok := true
		for _, h := range m.GetHeaders() {
			if h.GetName() != ":authority" {
				ok = false
				break
			}
			ok = ok && regexp.MustCompile(h.GetStringMatch().GetSafeRegex().GetRegex()).MatchString(host)
		}
		if !ok {
			continue
		}
		out.rule = i != len(routes)-1
		// Inside a short name's virtual host the service's filter rides on
		// the route (the virtual host is shared by several namespaces).
		if _, onRoute := r.GetTypedPerFilterConfig()[spFilter]; onRoute {
			out.filtered = true
		}
		if dr := r.GetDirectResponse(); dr != nil {
			out.kind = "notfound"
			for _, h := range r.GetResponseHeadersToAdd() {
				if h.GetHeader().GetKey() == proxy.NoHTTPPortErrorHeader {
					out.kind, out.reason = "refuse", h.GetHeader().GetValue()
				}
			}
			if dr.GetStatus() == 200 {
				out.kind = "direct200"
			}
			return out
		}
		ra := r.GetRoute()
		if ra == nil {
			out.kind = "other"
			return out
		}
		switch {
		case ra.GetClusterHeader() != "":
			out.kind, out.cluster = "ondemand", host
		case ra.GetCluster() == proxy.PassthroughClusterName:
			out.kind, out.cluster = "passthrough", ra.GetCluster()
		case ra.GetCluster() != "":
			out.kind, out.cluster = "cluster", ra.GetCluster()
		case ra.GetWeightedClusters() != nil:
			out.kind = "cluster"
			for _, w := range ra.GetWeightedClusters().GetClusters() {
				out.cluster += w.GetName() + ","
			}
		default:
			out.kind, out.cluster = "cluster", quicSelectionDefault(t, ra)
		}
		return out
	}
	out.kind = "nomatch"
	return out
}

func spCaptureTables(c *SnapshotCache, ports ...uint32) map[string]*routev3.RouteConfiguration {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	set := append(c.captureHTTPPorts(), ports...)
	slices.Sort(set)
	set = slices.Compact(set)
	c.captureTables = captureTablesMemo{}
	out := map[string]*routev3.RouteConfiguration{}
	perPort, other := c.capturePerPortTables(c.captureTableInputs(), set)
	for _, rc := range append(slices.Collect(maps.Values(perPort)), other) {
		out[rc.GetName()] = rc
	}
	c.captureTables = captureTablesMemo{}
	// The proxy port's table is not a per-port table: it is published with
	// the snapshot, from the loopback table's virtual hosts.
	if snap, err := c.GetSnapshot("node-1"); err == nil {
		name := proxy.CaptureHTTPRouteName(meshconst.ProxyOutboundPort)
		if rc, ok := snap.GetResources(resourcev3.RouteType)[name].(*routev3.RouteConfiguration); ok {
			out[name] = rc
		}
	}
	return out
}

func spChainCfg(t *testing.T) proxy.ExtensionFilter {
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	return proxy.ExtensionFilter{Name: spFilter, Config: cfg}
}

// ---------------------------------------------------------------------------
// 1. A service the node holds no endpoint of yet (registry not listed at
// start), with a generated Service and a service-wide filter. route.go says:
// "A name that IS a mesh service never reaches the fallthrough: every table
// carries a virtual host for it, one that routes or one that refuses."
// ---------------------------------------------------------------------------

func TestPerPort_ColdService_EveryNameOnARealPortTable(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, services)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": spChainCfg(t)})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	tables := spCaptureTables(c, 80, 8080, 9090, 18081)

	const mesh = "multi.demo." + a043Mesh
	for _, tc := range []struct{ table, host string }{
		{"cap_http_8080", mesh + ":8080"},
		{"cap_http_8080", mesh},
		{"cap_http_8080", "multi.demo.svc.cluster.local:8080"},
		{"cap_http_8080", "multi.demo.svc.cluster.local"},
		{"cap_http_8080", "multi.demo:8080"},
		{"cap_http_8080", "multi.demo.svc:8080"},
		{"cap_http_9090", "multi.demo.svc.cluster.local:9090"},
		{"cap_http_9090", mesh},
		{"cap_http_9090", "multi.demo.svc:9090"},
		{"cap_http_80", "multi.demo.svc.cluster.local"},
		{"cap_http_80", mesh},
		// A Host that names another port than the dialled one: refused, not
		// passed through, for a name the node knows no port of as well.
		{"cap_http_8080", mesh + ":9090"},
		{"cap_http_8080", "multi.demo.svc.cluster.local:9090"},
		// The proxy port: mesh names only, and the port in Host is the port.
		{"cap_http_18081", mesh + ":8080"},
		{"cap_http_18081", mesh},
		// A port no service is known to declare as HTTP.
		{"cap_http_other", mesh + ":7777"},
	} {
		got := spResolve(t, tables[tc.table], tc.host)
		t.Logf("%-15s Host %-42s -> %s", tc.table, tc.host, got)
		assert.NotEqualf(t, "passthrough", got.kind, "%s, Host %s: a mesh service's name takes the catch-all's passthrough (leaves the mesh, to the address that was dialled)", tc.table, tc.host)
		if got.kind == "ondemand" || got.kind == "cluster" {
			assert.Truef(t, got.filtered, "%s, Host %s: delivered by %s without the service's filter", tc.table, tc.host, got.vhost)
		}
	}
	// The cluster a parked request asks for is the one of the port that was
	// dialled, whatever name was used: never the bare name's.
	for _, host := range []string{mesh + ":9090", mesh, "multi.demo.svc.cluster.local", "multi.demo:9090"} {
		got := spResolve(t, tables["cap_http_9090"], host)
		assert.Equalf(t, mesh+":9090", got.cluster, "cap_http_9090, Host %s: %s", host, got)
	}
	// COLD AND WARM DO NOT DIFFER. The node holds no endpoint of multi, and
	// knows its ports from its generated Service (8080 and 9090, HTTP). So
	// what it does not declare is refused by name, exactly as once an
	// endpoint is held, on every table: nothing is left to the authority the
	// client typed, and nothing falls to the passthrough.
	for _, tc := range []struct{ table, host, reason string }{
		// A dial to port 80, which multi does not declare.
		{"cap_http_80", mesh, proxy.RefusalPortNotDeclared},
		{"cap_http_80", "multi.demo.svc.cluster.local", proxy.RefusalPortNotDeclared},
		// A dial to a port no service is known to serve as HTTP. Before the
		// Service's ports were read, the mesh name with a port was routed by
		// the port in Host (a Host that disagreed with the dialled port
		// decided), and every other name was passed through.
		{"cap_http_other", mesh + ":7777", proxy.RefusalPortNotDeclared},
		{"cap_http_other", mesh + ":8080", proxy.RefusalPortNotDeclared},
		{"cap_http_other", "multi.demo.svc.cluster.local:7777", proxy.RefusalPortNotDeclared},
		{"cap_http_other", mesh, proxy.RefusalPortNotDeclared},
		// The proxy port: a name without a port is port 80, which multi does
		// not declare; 18081 is a port it does not declare, like 7777.
		{"cap_http_18081", mesh, proxy.RefusalPortRequired},
		{"cap_http_18081", mesh + ":18081", proxy.RefusalPortNotDeclared},
		{"cap_http_18081", mesh + ":7777", proxy.RefusalPortNotDeclared},
	} {
		got := spResolve(t, tables[tc.table], tc.host)
		assert.Equalf(t, "refuse", got.kind, "%s, Host %s: %s", tc.table, tc.host, got)
		assert.Equalf(t, tc.reason, got.reason, "%s, Host %s: %s", tc.table, tc.host, got)
	}
	// And what it declares is asked for by its port's cluster, never by the
	// name the authority spells and never by the bare cluster name.
	for _, tc := range []struct{ table, host, cluster string }{
		{"cap_http_18081", mesh + ":8080", mesh + ":8080"},
		{"cap_http_18081", mesh + ":9090", mesh + ":9090"},
		{"cap_http_8080", "multi.demo.svc.cluster.local", mesh + ":8080"},
	} {
		got := spResolve(t, tables[tc.table], tc.host)
		assert.Equalf(t, "cluster", got.kind, "%s, Host %s: %s", tc.table, tc.host, got)
		assert.Equalf(t, tc.cluster, got.cluster, "%s, Host %s", tc.table, tc.host)
		assert.Truef(t, got.filtered, "%s, Host %s", tc.table, tc.host)
	}
	// A service that DOES declare 80, cold as well: a name without a port is
	// its port 80, on both paths.
	const eighty = "eighty.demo." + a043Mesh
	for _, tc := range []struct{ table, host string }{
		{"cap_http_18081", eighty},
		{"cap_http_80", eighty},
		{"cap_http_80", "eighty.demo.svc.cluster.local"},
	} {
		got := spResolve(t, tables[tc.table], tc.host)
		assert.Equalf(t, "cluster", got.kind, "%s, Host %s: %s", tc.table, tc.host, got)
		assert.Equalf(t, eighty+":80", got.cluster, "%s, Host %s", tc.table, tc.host)
	}
	// The proxy port serves mesh names only: a Kubernetes name there is not
	// the mesh's, cold or warm.
	got := spResolve(t, tables["cap_http_18081"], "multi.demo.svc.cluster.local:18081")
	assert.Equal(t, "passthrough", got.kind, got.String())
}

// 2. A service with a service-wide filter and endpoints, whose generated
// Service this agent does not know (the registrar's generator runs on a
// ticker after the first endpoint registers; or a user Service owns the name
// and the generator skips it). On the loopback table it has its virtual hosts.
// On the capture tables it has none: its mesh authority names its cluster on
// the catch-all.
func TestPerPort_ChainFilterServiceWithoutAGeneratedService(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	var projected []a043Service
	for _, s := range services {
		if s.key() != "demo/multi" {
			projected = append(projected, s)
		}
	}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client"}, a043Mesh))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, projected)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": spChainCfg(t)})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	const mesh = "multi.demo." + a043Mesh
	require.Contains(t, snap.GetResources(resourcev3.ClusterType), mesh+":9090", "the service's clusters are published (a chain-filter service is always in the dependency set)")
	tables := spCaptureTables(c, 80, 8080, 9090, 18081)
	out := a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	got := spResolve(t, out, mesh+":9090")
	t.Logf("out_http        Host %s -> %s", mesh+":9090", got)
	assert.True(t, got.filtered, "loopback control")
	for _, tc := range []struct{ table, host string }{
		{"cap_http_9090", mesh + ":9090"},
		{"cap_http_8080", mesh + ":8080"},
		{"cap_http_18081", mesh + ":18081"},
		{"cap_http_18081", mesh},
		{"cap_http_80", mesh},
	} {
		got := spResolve(t, tables[tc.table], tc.host)
		t.Logf("%-15s Host %-42s -> %s", tc.table, tc.host, got)
		if got.kind == "ondemand" || got.kind == "cluster" {
			assert.Truef(t, got.filtered, "%s, Host %s: routed to the service's cluster by %s WITHOUT its service-wide filter", tc.table, tc.host, got.vhost)
		}
	}
}

// A snapshot generated after a FILTER event alone, with no registry refresh in
// between, carries the filter on both entry paths: the loopback table's
// virtual hosts live on the cluster entries, and are rebuilt when the rule
// inputs change, not only when the registry is listed again. (The same for a
// route event: TestPerPort_TablesAgreeAfterARouteEventAlone.)
func TestPerPort_ChainFilterAfterAFilterEventAlone(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": spChainCfg(t)})
	require.NoError(t, c.generateSnapshot(ctx))
	const mesh = "multi.demo." + a043Mesh
	capGot := spResolve(t, spCaptureTables(c)["cap_http_9090"], mesh+":9090")
	outGot := spResolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], mesh+":9090")
	t.Logf("captured: %s", capGot)
	t.Logf("loopback: %s", outGot)
	assert.True(t, capGot.filtered, "captured")
	assert.True(t, outGot.filtered, "loopback: the snapshot published after the filter event delivers without the filter until the next registry refresh")
}

// ---------------------------------------------------------------------------
// 4. Order: the derivation of a service's ports, and everything built from it,
// must not depend on the order the registry returned the rows in.
// ---------------------------------------------------------------------------

type spPod struct {
	ip     string
	legacy uint32
	ports  map[uint32]registryv1.PortProtocol
	bare   bool // a row with no port_protocols
}

func spRows(svc string, pods []spPod, skew26 bool) map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint {
	out := map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{}
	for _, p := range pods {
		ep := makeEndpoint(p.ip, "cluster-1", "node-2", p.legacy)
		ep.KubernetesMetadata.Namespace = strings.Split(svc, "/")[0]
		for port := range p.ports {
			ep.Ports = append(ep.Ports, port)
		}
		slices.Sort(ep.Ports)
		if !p.bare {
			ep.PortProtocols = p.ports
		}
		served := map[registryv1.Service_Protocol]bool{}
		for _, class := range p.ports {
			served[registryv1.Service_Protocol(class)] = true
		}
		if skew26 || p.bare {
			served = map[registryv1.Service_Protocol]bool{registryv1.Service_Protocol(p.ports[p.legacy]): true}
		}
		for proto := range served {
			out[proto] = append(out[proto], ep)
		}
	}
	return out
}

func spPodsRegistry(bySvc map[string][]spPod, skew26 bool) *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			out := map[string][]*registryv1.ServiceEndpoint{}
			for svc, pods := range bySvc {
				if rows := spRows(svc, pods, skew26)[protocol]; len(rows) > 0 {
					out[svc] = rows
				}
			}
			return out, nil
		},
	}
}

func spRandomPods(rng *rand.Rand, dupIPs bool) []spPod {
	classes := []registryv1.PortProtocol{registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_UDP}
	numbers := []uint32{80, 8080, 9000, 9090, 18081, 18082, 5353}
	n := 1 + rng.Intn(4)
	pods := make([]spPod, 0, n)
	for i := 0; i < n; i++ {
		p := spPod{ip: fmt.Sprintf("10.7.0.%d", i+1), ports: map[uint32]registryv1.PortProtocol{}}
		if dupIPs {
			p.ip = fmt.Sprintf("10.7.0.%d", 1+rng.Intn(2))
		}
		for k := 1 + rng.Intn(3); k > 0; k-- {
			class := classes[rng.Intn(10)%3]
			if rng.Intn(10) < 7 {
				class = classes[rng.Intn(2)]
			}
			p.ports[numbers[rng.Intn(len(numbers))]] = class
		}
		keys := slices.Sorted(maps.Keys(p.ports))
		p.legacy = keys[rng.Intn(len(keys))]
		pods = append(pods, p)
	}
	return pods
}

func spPortsString(p servicePorts) string {
	return fmt.Sprintf("http=%v tcp=%v udp=%v legacy=%d/%s conflicts=%v", p.http, p.tcp, p.udp, p.legacyPort, className(p.legacyClass), p.conflicts)
}

// The unit: deriveServicePorts over shuffled rows. Without and with two rows
// of one (cluster, address).
func TestPerPort_DerivationDoesNotDependOnRowOrder(t *testing.T) {
	for _, dup := range []bool{false, true} {
		name := "distinct addresses"
		if dup {
			name = "two rows of one address (an address reused before the old row is gone)"
		}
		t.Run(name, func(t *testing.T) {
			bad := 0
			for seed := int64(1); seed <= 3000 && bad < 3; seed++ {
				rng := rand.New(rand.NewSource(seed))
				pods := spRandomPods(rng, dup)
				derive := func(pods []spPod) servicePorts {
					rows := spRows("demo/x", pods, false)
					return deriveServicePorts(
						listedEndpoints{protocol: registryv1.Service_PROTOCOL_HTTP, endpoints: rows[registryv1.Service_PROTOCOL_HTTP]},
						listedEndpoints{protocol: registryv1.Service_PROTOCOL_TCP, endpoints: rows[registryv1.Service_PROTOCOL_TCP]},
						listedEndpoints{protocol: registryv1.Service_PROTOCOL_UDP, endpoints: rows[registryv1.Service_PROTOCOL_UDP]},
					)
				}
				want := derive(pods)
				for k := 0; k < 6; k++ {
					shuffled := slices.Clone(pods)
					rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
					got := derive(shuffled)
					if spPortsString(got) != spPortsString(want) {
						bad++
						t.Errorf("seed %d: the derivation depends on the order of the rows\n  rows  %+v -> %s\n  rows  %+v -> %s", seed, pods, spPortsString(want), shuffled, spPortsString(got))
						break
					}
				}
			}
		})
	}
}

// The whole: one service with several pods that may disagree, listed in two
// orders, through the cache: the capture tables, the loopback table, the
// capture listener's chains, and the cluster set with each load assignment's
// members must be the same.
func TestPerPort_SnapshotDoesNotDependOnRowOrder(t *testing.T) {
	ctx := context.Background()
	others := map[string][]spPod{
		"demo/other": {{ip: "10.0.7.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP, 9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP}}},
	}
	build := func(pods []spPod, skew bool) (string, *SnapshotCache) {
		c := newTestCache("node-1")
		c.SetCaptureEnabled(true)
		c.SetCaptureRedirectAll(true)
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client"}, a043Mesh))
		require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
		a043Project(c, []a043Service{{name: "x", ns: "demo", vip: "10.96.9.1"}, {name: "other", ns: "demo", vip: "10.96.9.2"}})
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/other": {{Backends: []proxy.GammaBackend{{Service: "demo/x", Cluster: "x.demo." + a043Mesh, Weight: 1, Port: 8080}}}}})
		reg := maps.Clone(others)
		reg["demo/x"] = pods
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", spPodsRegistry(reg, skew)))
		snap, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		var b strings.Builder
		c.clusterMu.RLock()
		fmt.Fprintf(&b, "ports: %s\n", spPortsString(c.servicePorts["demo/x"]))
		c.clusterMu.RUnlock()
		published := map[string]*routev3.RouteConfiguration{}
		for _, name := range slices.Sorted(maps.Keys(snap.GetResources(resourcev3.RouteType))) {
			rc := snap.GetResources(resourcev3.RouteType)[name].(*routev3.RouteConfiguration)
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(rc)
			fmt.Fprintf(&b, "table %s: %x\n", name, fnv32(raw))
			published[name] = rc
		}
		// What each per-port table answers (cap_http is compiled from them,
		// and withPerPortView holds it to their answers).
		view := withPerPortView(t, c, published)
		for _, name := range slices.Sorted(maps.Keys(view)) {
			rc := view[name]
			for _, vh := range rc.GetVirtualHosts() {
				if strings.HasPrefix(vh.GetName(), "x.demo.") {
					o := spOutcome{}
					if len(vh.GetDomains()) > 0 {
						o = spResolve(t, rc, vh.GetDomains()[0])
					}
					fmt.Fprintf(&b, "  %s %v -> %s %s %s\n", vh.GetName(), vh.GetDomains(), o.kind, o.cluster, o.reason)
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(snap.GetResources(resourcev3.ClusterType))) {
			fmt.Fprintf(&b, "cluster %s\n", name)
		}
		for _, name := range slices.Sorted(maps.Keys(snap.GetResources(resourcev3.EndpointType))) {
			fmt.Fprintf(&b, "cla %s: %v\n", name, spCLAIPs(snap.GetResources(resourcev3.EndpointType)[name]))
		}
		l := a043CaptureListener(t, c)
		var chains []string
		for _, fc := range l.GetFilterChains() {
			chains = append(chains, fc.GetName())
		}
		sort.Strings(chains)
		matcher, _ := proto.MarshalOptions{Deterministic: true}.Marshal(l.GetFilterChainMatcher())
		chains = append(chains, fmt.Sprintf("matcher=%x", fnv32(matcher)))
		fmt.Fprintf(&b, "chains: %s\n", strings.Join(chains, "; "))
		return b.String(), c
	}
	bad := 0
	for seed := int64(1); seed <= 400 && bad < 3; seed++ {
		rng := rand.New(rand.NewSource(seed))
		pods := spRandomPods(rng, false)
		skew := seed%5 == 0
		want, _ := build(pods, skew)
		for k := 0; k < 3; k++ {
			shuffled := slices.Clone(pods)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			got, _ := build(shuffled, skew)
			if got != want {
				bad++
				t.Errorf("seed %d (skew26=%v): the snapshot depends on the order of the rows.\nrows %+v\n%s\nrows %+v\n%s", seed, skew, pods, want, shuffled, got)
				break
			}
		}
	}
}

func fnv32(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

func spCLAIPs(res types.Resource) []string {
	var ips []string
	cla, ok := res.(*endpointv3.ClusterLoadAssignment)
	if !ok {
		return nil
	}
	for _, loc := range cla.GetEndpoints() {
		for _, e := range loc.GetLbEndpoints() {
			ips = append(ips, e.GetEndpoint().GetAddress().GetSocketAddress().GetAddress())
		}
	}
	sort.Strings(ips)
	return ips
}

// ---------------------------------------------------------------------------
// 5. The randomized walk, wide, with three more properties checked at every
// step (after the refresh a dependency change triggers has run):
//
//   F  a request for a service that has a service-wide filter is never
//      delivered (a cluster, or the on-demand route) by a virtual host that
//      does not enable it, in any capture table or in out_http;
//   P  a name of a mesh service with a generated Service never takes the
//      passthrough;
//   D  in the table of dialled port D, a service's own name is never routed to
//      one of the service's clusters of another port;
//   C  a connection to an address of service S selects no chain that names
//      another service, and a raw-TCP chain only on a port S serves as raw TCP
//      (or 18082 with a raw-TCP legacy target).
//
// PERPORT_WALK_SEEDS=from-to selects the seeds (default 1-4: the suite also runs
// under the race detector, where each seed costs tens of seconds; a longer
// range, e.g. 1-500, is for a run by hand). PERPORT_WALK_PLAIN=1 runs
// the plain walk (13 operations, the first set of checks only).
// ---------------------------------------------------------------------------

func spSeeds(t *testing.T) (int64, int64) {
	from, to := int64(1), int64(4)
	if v := os.Getenv("PERPORT_WALK_SEEDS"); v != "" {
		parts := strings.SplitN(v, "-", 2)
		a, err := strconv.ParseInt(parts[0], 10, 64)
		require.NoError(t, err)
		b, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		require.NoError(t, err)
		from, to = a, b
	}
	return from, to
}

var spMeshName = regexp.MustCompile(`[a-z0-9-]+\.[a-z0-9-]+\.` + regexp.QuoteMeta(a043Mesh))

func TestPerPort_Walk(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	cfg := spChainCfg(t).Config
	plain := os.Getenv("PERPORT_WALK_PLAIN") != ""
	from, to := spSeeds(t)
	violations := map[string]string{}
	counts := map[string]int{}
	note := func(category, detail string) {
		counts[category]++
		if _, seen := violations[category]; !seen {
			violations[category] = detail
		}
	}
	hard := 0
	for seed := from; seed <= to && hard < 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		vipSeq := 0
		services := a043Fixture()
		var extra []a043Service
		skew26 := false
		registry := func() *mockRegistry {
			all := append(slices.Clone(services), extra...)
			if !skew26 {
				return a043Registry(all)
			}
			// A chart 2.6 registrar on the kubernetes backend: a pod is listed
			// under the class of its declared port alone.
			return &mockRegistry{tcpAware: true, listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				out := map[string][]*registryv1.ServiceEndpoint{}
				for _, s := range all {
					if registryv1.Service_Protocol(s.ports[s.legacy]) != protocol {
						continue
					}
					ep := makeEndpoint(s.ip, "cluster-1", "node-2", s.legacy)
					ep.KubernetesMetadata.Namespace = s.ns
					ep.PortProtocols = s.ports
					for port := range s.ports {
						ep.Ports = append(ep.Ports, port)
					}
					slices.Sort(ep.Ports)
					out[s.key()] = append(out[s.key()], ep)
				}
				return out, nil
			}}
		}
		c := a043Cache(t, services)
		c.SetAuthzSidecar(0, false)
		projected := slices.Clone(services)
		routes := map[string][]proxy.GammaRoute{}
		chain := map[string]proxy.ExtensionFilter{}
		imported := map[string]proxy.ExtensionFilter{}
		portMaps := map[string]map[uint32]uint32{}
		targetPorts := map[string][]uint32{}
		tlsParents := map[string]bool{}
		var trail []string
		pick := func() *a043Service { return &services[rng.Intn(len(services))] }
		nOps := 13
		if !plain {
			nOps = 18
		}
		failed := false
		fatalf := func(format string, args ...any) {
			failed = true
			hard++
			t.Errorf(format+"\nafter:\n  %s", append(args, strings.Join(trail, "\n  "))...)
		}
		for step := 0; step < 60 && !failed; step++ {
			var op string
			switch rng.Intn(nOps) {
			case 0:
				s := pick()
				s.ports = maps.Clone(s.ports)
				p := []uint32{80, 7000, 8080, 9000, 9090, 18081}[rng.Intn(6)]
				if _, ok := s.ports[p]; ok && len(s.ports) > 1 && p != s.legacy {
					delete(s.ports, p)
				} else {
					s.ports[p] = []registryv1.PortProtocol{h, tcp}[rng.Intn(2)]
				}
				op = fmt.Sprintf("ports %s=%v", s.key(), s.ports)
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
			case 1:
				s := pick()
				keys := slices.Sorted(maps.Keys(s.ports))
				s.legacy = keys[rng.Intn(len(keys))]
				op = fmt.Sprintf("legacy %s=%d", s.key(), s.legacy)
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
			case 2:
				s := pick()
				s.ip = fmt.Sprintf("10.9.%d.%d", rng.Intn(250), 1+rng.Intn(250))
				op = "churn " + s.key()
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
			case 3:
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
			case 4:
				s := pick()
				op = "imported route " + s.key()
				c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{s.key(): {{
					Matches:  []proxy.GammaMatch{{Prefix: fmt.Sprintf("/i%d", rng.Intn(3))}},
					Backends: []proxy.GammaBackend{{Service: s.key(), Cluster: proxy.ServiceClusterName(s.key(), a043Mesh), Weight: 1}},
				}}})
			case 5:
				s := pick()
				if _, ok := chain[s.key()]; ok {
					delete(chain, s.key())
				} else {
					chain[s.key()] = proxy.ExtensionFilter{Name: spFilter, Config: cfg}
				}
				op = "chain filter " + s.key()
				c.SetServiceChainFilters(maps.Clone(chain))
			case 6:
				s := pick()
				op = "imported chain filter " + s.key()
				imported = map[string]proxy.ExtensionFilter{s.key(): {Name: spFilter, Config: cfg}}
				c.SetImportedServiceChainFilters(maps.Clone(imported))
			case 7:
				s := pick()
				if _, ok := portMaps[s.key()]; ok {
					delete(portMaps, s.key())
				} else {
					portMaps[s.key()] = map[uint32]uint32{80: 8080, 81: []uint32{0, 9090}[rng.Intn(2)]}
				}
				op = "port map " + s.key()
				c.SetServicePortMaps(maps.Clone(portMaps))
			case 8:
				s := pick()
				targetPorts[s.key()] = []uint32{[]uint32{80, 8080, 1234}[rng.Intn(3)]}
				op = "target port " + s.key()
				c.SetRouteTargetPorts(maps.Clone(targetPorts))
			case 9:
				if rng.Intn(2) == 0 && len(projected) > 2 {
					i := rng.Intn(len(projected))
					op = "unproject " + projected[i].key()
					projected = slices.Delete(slices.Clone(projected), i, i+1)
				} else {
					s := *pick()
					s.ns = []string{"other", "third", a043NS}[rng.Intn(3)]
					op = "project " + s.key()
					if !slices.ContainsFunc(projected, func(o a043Service) bool { return o.key() == s.key() }) {
						// A Service has an address of its own: two Services on
						// one ClusterIP is not a state a cluster can be in, and
						// it gives two services' chains one match.
						vipSeq++
						s.vip = fmt.Sprintf("10.97.%d.%d", vipSeq/250, 1+vipSeq%250)
						projected = append(slices.Clone(projected), s)
					}
				}
				a043Project(c, projected)
			case 10:
				s := pick()
				op = "observe " + s.key()
				c.ObserveDependency(ctx, s.key())
			case 11:
				if len(services) > 3 {
					i := rng.Intn(len(services))
					op = "leave " + services[i].key()
					services = slices.Delete(slices.Clone(services), i, i+1)
					require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
				}
			case 12:
				s := pick()
				op = "tls route " + s.key()
				tlsParents = map[string]bool{s.key(): true}
				c.SetTLSServiceRoutes(shpTLSRoutes(s.key()))
			case 13: // a second pod of a service that declares something else
				s := *pick()
				if rng.Intn(3) == 0 {
					extra = nil
					op = "no second pods"
				} else {
					s.ip = fmt.Sprintf("10.8.%d.%d", rng.Intn(250), 1+rng.Intn(250))
					s.ports = maps.Clone(s.ports)
					for p := range s.ports {
						if rng.Intn(2) == 0 {
							s.ports[p] = []registryv1.PortProtocol{h, tcp}[rng.Intn(2)]
						}
					}
					s.ports[[]uint32{80, 8080, 9000, 9090}[rng.Intn(4)]] = []registryv1.PortProtocol{h, tcp}[rng.Intn(2)]
					keys := slices.Sorted(maps.Keys(s.ports))
					s.legacy = keys[rng.Intn(len(keys))]
					extra = append(slices.Clone(extra), s)
					op = fmt.Sprintf("second pod %s legacy=%d %v", s.key(), s.legacy, s.ports)
				}
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
			case 14:
				skew26 = !skew26
				op = fmt.Sprintf("registrar 2.6 listing=%v", skew26)
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
			case 15:
				op = "no imported rules or filters"
				imported = map[string]proxy.ExtensionFilter{}
				c.SetImportedServiceRoutes(nil)
				c.SetImportedServiceChainFilters(nil)
			case 16: // a filter on a Service the registry lists nothing of
				if len(projected) > 0 {
					s := projected[rng.Intn(len(projected))]
					chain[s.key()] = proxy.ExtensionFilter{Name: spFilter, Config: cfg}
					op = "chain filter (any projected) " + s.key()
					c.SetServiceChainFilters(maps.Clone(chain))
				}
			case 17: // a route with a legacy parent port
				s := pick()
				b := pick()
				routes[s.key()] = []proxy.GammaRoute{{
					ParentPort: []uint32{80, 18081, 9000, 7000}[rng.Intn(4)],
					Matches:    []proxy.GammaMatch{{Prefix: "/"}},
					Backends:   []proxy.GammaBackend{{Service: b.key(), Cluster: proxy.ServiceClusterName(b.key(), a043Mesh), Weight: 1, Port: []uint32{0, 80, 18081, 9000, 7777}[rng.Intn(5)]}},
				}}
				op = "route (legacy numbers) " + s.key()
				c.SetServiceRoutes(maps.Clone(routes))
			}
			trail = append(trail, op)
			require.NoError(t, c.generateSnapshot(ctx))

			// --- the first set of checks ---
			c.snapshotMu.Lock()
			ports := c.captureHTTPPorts()
			memo := c.captureRouteTables(ports)
			c.captureTables = captureTablesMemo{}
			fresh := c.captureRouteTables(ports)
			c.snapshotMu.Unlock()
			if name, ok := shpTablesEqual(memo, fresh); !ok {
				fatalf("seed %d step %d: the memoized table %s differs from a fresh build", seed, step, name)
				break
			}
			for _, r := range fresh {
				rc := r.(*routev3.RouteConfiguration)
				seen := map[string]string{}
				for _, vh := range rc.GetVirtualHosts() {
					for _, d := range vh.GetDomains() {
						if prev, dup := seen[d]; dup {
							fatalf("seed %d step %d: table %s: domain %q claimed by %q and %q", seed, step, rc.GetName(), d, prev, vh.GetName())
						}
						seen[d] = vh.GetName()
					}
				}
			}
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
				fatalf("seed %d step %d: the published capture listener is STALE.\n published: %v\n fresh:     %v", seed, step, chainNames(published.(*listenerv3.Listener)), chainNames(freshL.(*listenerv3.Listener)))
				break
			}
			snap, err := c.GetSnapshot("node-1")
			require.NoError(t, err)
			if d := duplicateDomains(snap.GetResources(resourcev3.RouteType)); len(d) > 0 {
				sort.Strings(d)
				fatalf("seed %d step %d: duplicate domains in the snapshot %v", seed, step, d)
				break
			}
			for name, res := range snap.GetResources(resourcev3.ListenerType) {
				if !strings.HasPrefix(name, "capture_") {
					continue
				}
				if l, ok := res.(*listenerv3.Listener); ok {
					if d := shpDuplicateChainMatches(l); len(d) > 0 {
						fatalf("seed %d step %d: duplicate chain matches %v", seed, step, d)
					}
					for _, fc := range l.GetFilterChains() {
						if strings.HasPrefix(fc.GetName(), proxy.CaptureHTTPRoutePrefix) {
							if _, ok := snap.GetResources(resourcev3.RouteType)[fc.GetName()]; !ok {
								fatalf("seed %d step %d: chain %s has no route table in the snapshot", seed, step, fc.GetName())
							}
						}
					}
				}
			}
			if plain || failed {
				continue
			}

			// --- the added properties, after the refresh a dependency change triggers ---
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", registry()))
			snap, err = c.GetSnapshot("node-1")
			require.NoError(t, err)
			tables := map[string]*routev3.RouteConfiguration{}
			for name, res := range snap.GetResources(resourcev3.RouteType) {
				tables[name] = res.(*routev3.RouteConfiguration)
			}
			where := fmt.Sprintf("seed %d step %d (%s)", seed, step, op)
			c.clusterMu.RLock()
			known := maps.Clone(c.servicePorts)
			c.clusterMu.RUnlock()
			isProjected := map[string]a043Service{}
			for _, s := range projected {
				isProjected[s.key()] = s
			}
			filters := map[string]bool{}
			for k := range chain {
				filters[k] = true
			}
			for k := range imported {
				filters[k] = true
			}
			keys := map[string]bool{}
			for k := range filters {
				keys[k] = true
			}
			for k := range isProjected {
				keys[k] = true
			}
			for _, key := range slices.Sorted(maps.Keys(keys)) {
				parts := strings.SplitN(key, "/", 2)
				ns, name := parts[0], parts[1]
				mesh := fmt.Sprintf("%s.%s.%s", name, ns, a043Mesh)
				kube := []string{fmt.Sprintf("%s.%s.svc.cluster.local", name, ns), fmt.Sprintf("%s.%s.svc", name, ns), fmt.Sprintf("%s.%s", name, ns)}
				_, proj := isProjected[key]
				ports := known[key]
				_, mapped := portMaps[key]
				state := "informed"
				if !ports.known() {
					state = "no endpoint held"
				}
				for tname, table := range tables {
					var names []string
					d, isCapture := capturePortOf(tname)
					switch {
					case tname == proxy.OutboundHTTPRouteName:
						names = []string{mesh}
					case isCapture && d == meshconst.ProxyOutboundPort:
						// The proxy port's table is the loopback table under
						// another name: mesh names only, and the port in Host
						// is the port. None of the dialled-port rules apply.
						names, isCapture = []string{mesh}, false
					case isCapture || tname == proxy.CaptureHTTPOtherRouteName:
						names = append([]string{mesh}, kube...)
					default:
						continue
					}
					for _, n := range names {
						for _, suffix := range []string{"", ":80", ":8080", ":9090", ":9000", ":7000", ":18081", ":7777"} {
							got := spResolve(t, table, n+suffix)
							// S: the SHORT name. From the service's own
							// namespace it answers exactly as the full mesh
							// name does, in every table and for every
							// suffix; from a namespace that has no service of
							// the name it reaches nothing.
							if n == mesh {
								prevNS := testCallerNamespace
								testCallerNamespace = ns
								short := spResolve(t, table, name+suffix)
								testCallerNamespace = "a-namespace-with-no-services"
								foreign := spResolve(t, table, name+suffix)
								testCallerNamespace = prevNS
								same := short.kind == got.kind && short.reason == got.reason && short.filtered == got.filtered && short.rule == got.rule
								if got.kind == "cluster" {
									same = same && short.cluster == got.cluster
								}
								// A full name the table has no virtual host for
								// takes the table's floor, and its short name
								// has an arm that does what the floor does:
								// on demand, the cluster the FULL name spells,
								// named by the route and not by the authority.
								if got.kind == "ondemand" && short.kind == "cluster" && short.cluster == n+suffix {
									same = true
								}
								// The one stated difference: a service known by
								// name only, on a port the short domain does
								// not spell (its wildcard). The full name is
								// parked for the cluster it names; a short
								// authority names none, and is refused at once.
								if got.kind == "ondemand" && short.kind == "refuse" && short.reason == proxy.RefusalShortNameCold {
									same = true
								}
								// A name a dialled-port table claims by its mesh
								// name alone (a mesh service behind a Service the
								// agent knows nothing of: no generated Service, no
								// route). On a captured dial the short name is a
								// Kubernetes name, and it does what the service's
								// other Kubernetes names do there: nothing of the
								// mesh's, like a name that is no service at all.
								if !same && (tname == proxy.CaptureHTTPOtherRouteName || (isCapture && d != meshconst.ProxyOutboundPort)) {
									k := spResolve(t, table, name+"."+ns+suffix)
									none := spResolve(t, table, "no-such-name.nowhere"+suffix)
									same = short.kind == k.kind && short.reason == k.reason && short.cluster == k.cluster &&
										k.kind == none.kind && k.reason == none.reason && k.cluster == none.cluster
								}
								if !same {
									note("S: a short name does not answer as the full name of the caller's namespace",
										fmt.Sprintf("%s: table %s, Host %s -> %s; Host %s -> %s\nafter:\n  %s", where, tname, name+suffix, short, n+suffix, got, strings.Join(trail, "\n  ")))
								}
								if foreign.kind == "cluster" || foreign.kind == "ondemand" {
									note("S: a short name is delivered for a caller whose namespace has no service of that name",
										fmt.Sprintf("%s: table %s, Host %s -> %s\nafter:\n  %s", where, tname, name+suffix, foreign, strings.Join(trail, "\n  ")))
								}
							}
							// A route delivers when the cluster it names has an
							// endpoint in this snapshot (a retained entry's is
							// empty), or does not exist yet and is named on
							// demand (the request is parked for it).
							delivers, how := false, ""
							if got.kind == "ondemand" || got.kind == "cluster" {
								for _, cn := range strings.Split(strings.TrimSuffix(got.cluster, ","), ",") {
									cl, exists := snap.GetResources(resourcev3.ClusterType)[cn]
									switch {
									case exists && len(spCLAIPs(snap.GetResources(resourcev3.EndpointType)[spEDSName(cl)])) > 0:
										delivers, how = true, "has endpoints"
									case !exists && got.kind == "ondemand":
										delivers, how = true, "parks"
									}
								}
							}
							tkind := "capture"
							if !isCapture {
								tkind = tname
							}
							if filters[key] && delivers && !got.filtered {
								note(fmt.Sprintf("F: %s table, %s, generated Service known=%v, catch-all=%v, cluster %s", tkind, state, proj, got.vhost == "on_demand_catch_all", how),
									fmt.Sprintf("%s: table %s, Host %s -> %s\nafter:\n  %s", where, tname, n+suffix, got, strings.Join(trail, "\n  ")))
							}
							// The one place a name of a mesh service may still reach
							// the fallthrough: cap_http_other, for a service this
							// node knows no port of, by a Kubernetes name or by any
							// name without a port. That table does not know which
							// port was dialled, so only an authority that spells a
							// cluster ("<mesh name>:<port>") can be given one
							// (coldOtherVhost).
							unnameable := tname == proxy.CaptureHTTPOtherRouteName && !ports.known() && !mapped && (n != mesh || suffix == "")
							if proj && got.kind == "passthrough" && !unnameable {
								note(fmt.Sprintf("P: %s table, %s, mesh name=%v", tkind, state, n == mesh),
									fmt.Sprintf("%s: table %s, Host %s -> %s\nafter:\n  %s", where, tname, n+suffix, got, strings.Join(trail, "\n  ")))
							}
							// D: the service's own default route, in the table of d.
							if isCapture && proj && ports.known() && !mapped && got.kind == "cluster" && !got.rule {
								own := got.cluster == mesh || strings.HasPrefix(got.cluster, mesh+":")
								want := httpClusterForPort(key, a043Mesh, ports, d)
								if own && !(ports.hasHTTP(d) && got.cluster == want) {
									note("D: a service's own name routed to its cluster of another port",
										fmt.Sprintf("%s: table %s, Host %s -> %s; ports %s\nafter:\n  %s", where, tname, n+suffix, got, spPortsString(ports), strings.Join(trail, "\n  ")))
								}
								if suffix != "" && suffix != fmt.Sprintf(":%d", d) {
									note("D: a Host port other than the dialled one is routed",
										fmt.Sprintf("%s: table %s, Host %s -> %s\nafter:\n  %s", where, tname, n+suffix, got, strings.Join(trail, "\n  ")))
								}
							}
						}
					}
				}
			}
			// C: the chains.
			l := a043CaptureListener(t, c)
			for _, s := range projected {
				mesh := fmt.Sprintf("%s.%s.%s", s.name, s.ns, a043Mesh)
				ports := known[s.key()]
				sni := ""
				for p := range tlsParents {
					sni = "tls." + strings.ReplaceAll(p, "/", ".") + ".example"
				}
				for _, p := range []uint32{80, 443, 5555, 7000, 8080, 9000, 9090, 18081, 18082} {
					conns := map[string]shpConn{"raw": shpRaw(s.vip, p), "http": shpHTTP(s.vip, p), "tls": {ip: s.vip, port: p, transport: "tls", sni: "unknown.example"}}
					if sni != "" {
						conns["tls+sni"] = shpConn{ip: s.vip, port: p, transport: "tls", sni: sni}
					}
					for kind, conn := range conns {
						chainName := shpSelectChain(l, conn)
						for _, m := range spMeshName.FindAllString(chainName, -1) {
							if m != mesh && !(kind == "tls+sni") {
								note("C: a connection to one service's address selects a chain naming another service",
									fmt.Sprintf("%s: %s to %s (%s):%d -> chain %s\nafter:\n  %s", where, kind, s.key(), s.vip, p, chainName, strings.Join(trail, "\n  ")))
							}
						}
						if strings.HasPrefix(chainName, "cap_tcp_tcp:"+mesh+"_") && kind != "tls+sni" {
							ok := slices.Contains(ports.tcp, p) || (p == 18082 && ports.legacyTCPPort() != 0)
							if !ok {
								note("C: a raw-TCP chain on a port the service does not serve as raw TCP",
									fmt.Sprintf("%s: %s to %s:%d -> chain %s; ports %s\nafter:\n  %s", where, kind, s.key(), p, chainName, spPortsString(ports), strings.Join(trail, "\n  ")))
							}
						}
						if kind == "raw" && strings.HasPrefix(chainName, proxy.CaptureHTTPChainName) {
							note("C: bytes that are not HTTP select an HTTP chain", fmt.Sprintf("%s: raw to %s:%d -> %s", where, s.key(), p, chainName))
						}
						if _, mapped := portMaps[s.key()]; kind == "http" && !mapped && ports.hasHTTP(p) && chainName != proxy.CaptureHTTPChainName {
							note("C: HTTP to a declared HTTP port does not select that port's HTTP chain",
								fmt.Sprintf("%s: http to %s:%d -> chain %q; ports %s\nafter:\n  %s", where, s.key(), p, chainName, spPortsString(ports), strings.Join(trail, "\n  ")))
						}
						if kind == "raw" && slices.Contains(ports.tcp, p) && !strings.HasPrefix(chainName, "cap_tcp_tcp:"+mesh) {
							note("C: raw TCP to a declared raw-TCP port does not select the service's chain",
								fmt.Sprintf("%s: raw to %s:%d -> chain %q; ports %s\nafter:\n  %s", where, s.key(), p, chainName, spPortsString(ports), strings.Join(trail, "\n  ")))
						}
					}
				}
			}
		}
	}
	for _, cat := range slices.Sorted(maps.Keys(violations)) {
		t.Errorf("%d times: %s\nfirst: %s", counts[cat], cat, violations[cat])
	}
	t.Logf("seeds %d-%d, plain=%v", from, to, plain)
}
