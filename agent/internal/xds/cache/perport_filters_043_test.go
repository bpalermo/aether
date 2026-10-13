package cache

import (
	"context"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestPerPort_ServiceFiltersOnEveryVirtualHost: a service's chain-scope HTTP
// filter is enabled by the service's own virtual host, so EVERY virtual host
// that can deliver a request to the service must carry it: each per-port one,
// the legacy ones, the loopback table's default, per-port and mesh-port ones,
// and the one a service this node holds no endpoint of gets on a real port. A
// virtual host built without it answers the request with the filter skipped.
//
// The walk is over every route of every table of the snapshot: a route that
// names a cluster of the service, from a virtual host that does not enable
// the filter, fails the test whatever built it.
func TestPerPort_ServiceFiltersOnEveryVirtualHost(t *testing.T) {
	ctx := context.Background()
	const filter = "envoy.filters.http.header_mutation"
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)

	for _, known := range []bool{true, false} {
		name := "the node holds the service's endpoints"
		if !known {
			name = "the node holds no endpoint of the service yet"
		}
		t.Run(name, func(t *testing.T) {
			services := a043Fixture()
			c := a043Cache(t, services)
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
				"demo/multi":    {Name: filter, Config: cfg},
				"demo/tcpfirst": {Name: filter, Config: cfg},
			})
			listed := services
			if !known {
				c = newTestCache("node-1")
				c.SetCaptureEnabled(true)
				c.SetCaptureRedirectAll(true)
				require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
				a043Project(c, services)
				c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
					"demo/multi":    {Name: filter, Config: cfg},
					"demo/tcpfirst": {Name: filter, Config: cfg},
				})
				listed = nil
			}
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(listed)))

			tables := map[string]*routev3.RouteConfiguration{}
			for _, r := range c.captureRouteTables(append(c.captureHTTPPorts(), 8080, 9090)) {
				rc := r.(*routev3.RouteConfiguration)
				tables[rc.GetName()] = rc
			}
			tables[proxy.OutboundHTTPRouteName] = a043Tables(t, c)[proxy.OutboundHTTPRouteName]
			// The table of the proxy port's chain is built with the snapshot,
			// from the loopback table's virtual hosts.
			tables["cap_http_18081"] = a043Tables(t, c)["cap_http_18081"]

			delivering := 0
			for tableName, table := range tables {
				for _, vh := range table.GetVirtualHosts() {
					_, enabled := vh.GetTypedPerFilterConfig()[filter]
					for _, r := range vh.GetRoutes() {
						ra := r.GetRoute()
						if ra == nil {
							continue
						}
						cluster := ra.GetCluster()
						if cluster == "" && ra.GetClusterHeader() == "" {
							cluster = quicSelectionDefault(t, ra)
						}
						for _, svc := range []string{"multi.demo." + a043Mesh, "tcpfirst.demo." + a043Mesh} {
							if cluster != svc && !strings.HasPrefix(cluster, svc+":") {
								continue
							}
							delivering++
							assert.Truef(t, enabled, "table %s, virtual host %s (%v) routes to %s without the service's HTTP filter", tableName, vh.GetName(), vh.GetDomains(), cluster)
						}
					}
				}
			}
			if known {
				// multi: 8080 and 9090, each in its captured table, on the
				// loopback table and on the proxy port's. tcpfirst: 8080 in
				// the same three.
				assert.GreaterOrEqual(t, delivering, 9, "the walk must have seen the service's virtual hosts")
			} else {
				assert.GreaterOrEqual(t, delivering, 4)
			}

			// A request for the service never resolves to the catch-all's
			// on-demand route on the captured path: it has a virtual host of
			// its own in every table it can be named in.
			for _, tc := range []struct {
				table, host string
			}{
				{"cap_http_8080", "multi.demo." + a043Mesh + ":8080"},
				{"cap_http_9090", "multi.demo." + a043Mesh + ":9090"},
				{"cap_http_18081", "multi.demo." + a043Mesh + ":18081"},
				{"cap_http_80", "multi.demo." + a043Mesh},
			} {
				got := a043Resolve(t, tables[tc.table], tc.host)
				assert.NotEqualf(t, "on_demand_catch_all", got.vhost, "%s, Host %s", tc.table, tc.host)
			}
			// Nor on the loopback path: whatever port the Host spells, and
			// with none, the virtual host that takes the request is the
			// service's own and enables its filter. For a service with no
			// cluster here that is the on-demand route under the service's
			// name, which no cluster name identifies, so the walk above does
			// not see it.
			for _, host := range []string{"multi.demo." + a043Mesh, "multi.demo." + a043Mesh + ":8080", "multi.demo." + a043Mesh + ":9090", "multi.demo." + a043Mesh + ":18081", "multi.demo." + a043Mesh + ":7777"} {
				got := a043Resolve(t, tables[proxy.OutboundHTTPRouteName], host)
				if !assert.NotEqualf(t, "on_demand_catch_all", got.vhost, "out_http, Host %s: the request takes the catch-all, without the filter", host) {
					continue
				}
				var vh *routev3.VirtualHost
				for _, v := range tables[proxy.OutboundHTTPRouteName].GetVirtualHosts() {
					if v.GetName() == got.vhost {
						vh = v
					}
				}
				require.NotNil(t, vh)
				delivers := false
				for _, r := range vh.GetRoutes() {
					delivers = delivers || r.GetRoute() != nil
				}
				if _, enabled := vh.GetTypedPerFilterConfig()[filter]; delivers {
					assert.Truef(t, enabled, "out_http, Host %s: virtual host %s delivers without the service's HTTP filter", host, vh.GetName())
				}
			}
		})
	}
}

// TestPerPort_TablesForEveryPortAPublishedChainNames: the tables a snapshot
// carries are those of the node's current HTTP port set AND of every port a
// cap_http_<P> chain of a listener in that snapshot names. A listener built a
// moment ago against a set that has since shrunk still finds its table.
func TestPerPort_TablesForEveryPortAPublishedChainNames(t *testing.T) {
	stale := &listenerv3.Listener{Name: "capture_demo_old", FilterChains: []*listenerv3.FilterChain{
		{Name: "cap_http_1234"}, {Name: "cap_http_other"}, {Name: "cap_tcp_tcp:x_5678"},
	}}
	other := &listenerv3.Listener{Name: "inbound_demo_old", FilterChains: []*listenerv3.FilterChain{{Name: "cap_http_4321"}}}
	got := captureTablePorts([]types.Resource{stale, other}, []uint32{80, 18081})
	assert.Equal(t, []uint32{80, 1234, 18081}, got, "the chain's port is added; a raw-TCP chain and another listener's chains are not")
}

// TestPerPort_ParentPortResolvesThroughThePortMap: a rule whose parentRef
// names a port of a USER Service applies where that Service port leads: in
// the table of the Service port on the captured path, and on the pod port's
// authority on the loopback path. And a change of the parentRef's port alone
// is a change of the route.
func TestPerPort_ParentPortResolvesThroughThePortMap(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	services := []a043Service{{name: "web", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.4.1", vip: "10.96.4.1"}}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	declareDeps(c, "demo/web")
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {80: 9090, 81: 8080}})
	rule := func(parentPort uint32) map[string][]proxy.GammaRoute {
		return map[string][]proxy.GammaRoute{"demo/web": {{
			ParentPort: parentPort, Matches: []proxy.GammaMatch{{Prefix: "/x"}},
			HeaderMutation: &proxy.GammaHeaderMutation{AddResponse: []proxy.GammaHeaderKV{{Name: "x-rule", Value: "on"}}},
		}}}
	}
	has := func(table *routev3.RouteConfiguration, host string) bool {
		vh := findVHost(table.GetVirtualHosts(), host)
		require.NotNilf(t, vh, "no virtual host for %s in %s", host, table.GetName())
		for _, r := range vh.GetRoutes() {
			for _, hd := range r.GetResponseHeadersToAdd() {
				if hd.GetHeader().GetKey() == "x-rule" {
					return true
				}
			}
		}
		return false
	}
	c.SetServiceRoutes(rule(80))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)
	const mesh = "web.demo." + a043Mesh
	assert.True(t, has(tables["cap_http_80"], "web.demo"), "Service port 80")
	assert.False(t, has(tables["cap_http_81"], "web.demo"), "Service port 81 is another port")
	assert.True(t, has(tables[proxy.OutboundHTTPRouteName], mesh+":9090"), "loopback: Service port 80 is pod port 9090")
	assert.False(t, has(tables[proxy.OutboundHTTPRouteName], mesh+":8080"))

	// The parentRef moves to port 81: nothing else of the rule changes.
	changed := c.DependencyChanges()
	for len(changed) > 0 {
		<-changed
	}
	c.SetServiceRoutes(rule(81))
	assert.Len(t, changed, 1, "a rule whose parent port changed is a changed route")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables = a043Tables(t, c)
	assert.False(t, has(tables["cap_http_80"], "web.demo"))
	assert.True(t, has(tables["cap_http_81"], "web.demo"))
	assert.True(t, has(tables[proxy.OutboundHTTPRouteName], mesh+":8080"))

	// A port map that changes alone asks for a rebuild too.
	for len(changed) > 0 {
		<-changed
	}
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {80: 9090, 81: 9090}})
	assert.Len(t, changed, 1, "a changed port map must signal a rebuild")
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {80: 9090, 81: 9090}})
	assert.Len(t, changed, 1, "an unchanged one must not")
}

// TestPerPort_EdgePublishesPerPortRawTCPClusters: the edge publishes a
// service's per-port raw-TCP clusters beside its floor: the declared raw-TCP
// port as an alias of the floor (no SNI), every other raw-TCP port with its
// own membership and its port as SNI.
func TestPerPort_EdgePublishesPerPortRawTCPClusters(t *testing.T) {
	ctx := context.Background()
	tcp, h := registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	services := []a043Service{{name: "db", ns: a043NS, legacy: 5432, ports: map[uint32]registryv1.PortProtocol{5432: tcp, 6432: tcp, 8080: h}, ip: "10.0.5.1"}}
	c := newTestCache("edge-1")
	c.SetEdgeMode(80)
	declareDeps(c, "demo/db")
	require.NoError(t, c.UpdateEdgeIdentity(ctx, nodeIdentity, a043Mesh))
	c.SetEdgeTCPRoutes([]proxy.EdgeL4TCPRoute{{Port: 5432, Backends: []proxy.L4Backend{{Service: "demo/db", Cluster: proxy.TCPClusterName("demo/db", a043Mesh), Weight: 1}}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "edge-1", a043Registry(services)))
	snap, err := c.GetSnapshot("edge-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)
	endpoints := snap.GetResources(resourcev3.EndpointType)
	const floor = "tcp:db.demo." + a043Mesh
	for _, name := range []string{floor, floor + ":5432", floor + ":6432"} {
		cl, ok := clusters[name].(*clusterv3.Cluster)
		if !assert.Truef(t, ok, "the edge does not publish %s", name) {
			continue
		}
		assert.Containsf(t, endpoints, cl.GetEdsClusterConfig().GetServiceName(), "%s subscribes to a load assignment nothing publishes", name)
	}
	assert.NotContains(t, clusters, floor+":8080", "an HTTP port has no raw-TCP cluster")
}
