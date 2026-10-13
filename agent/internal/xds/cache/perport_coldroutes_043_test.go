package cache

import (
	"context"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPerPort_ColdServiceCarriesItsRoutesOnTheLoopbackTable: a service this
// node holds no cluster of is reached on the loopback path through the
// on-demand route, and the request that is parked there resumes on the route
// it was matched by. That route must be one of the service's OWN virtual
// host, carrying the service's HTTPRoute rules: on the catch-all the request
// was delivered to the service's cluster with the route's matches, filters
// and backends skipped, as if the route did not exist.
//
// The rules are known without an endpoint (they come from the API), and are
// placed by what they say: a port-less parentRef on every authority, a
// parentRef with a port on the authority of the pod port behind it.
func TestPerPort_ColdServiceCarriesItsRoutesOnTheLoopbackTable(t *testing.T) {
	ctx := context.Background()
	const multi = "multi.demo." + a043Mesh
	// (A name a user's Service owns is not a stand-in case either: its port
	// map says what its ports are, TestPerPort_ColdUserServiceIsRoutedByItsPortMap.)
	for name, portMap := range map[string]map[uint32]uint32{
		"a generated Service": nil,
	} {
		t.Run(name, func(t *testing.T) {
			parent := uint32(9090)
			if portMap != nil {
				parent = 80
			}
			c := newTestCache("node-1")
			c.SetCaptureEnabled(true)
			c.SetCaptureRedirectAll(true)
			require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
			// The Services are known by name only: nothing says what multi's
			// ports are. (With its ports known from its generated Service it
			// is not a stand-in case at all: it is routed and refused by them,
			// TestStrictCold_*.)
			a043ProjectNamesOnly(c, a043Fixture())
			if portMap != nil {
				c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/multi": portMap})
			}
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {
				{Matches: []proxy.GammaMatch{{Prefix: "/all"}}, HeaderMutation: rtpMark("all")},
				{
					ParentPort: parent, Matches: []proxy.GammaMatch{{Prefix: "/one"}}, HeaderMutation: rtpMark("one"),
					Backends: []proxy.GammaBackend{{Service: "demo/other", Cluster: "other.demo." + a043Mesh, Weight: 1}},
				},
			}})
			// No endpoint of any service is listed.
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			table := a043Tables(t, c)[proxy.OutboundHTTPRouteName]

			// An authority WITH a port is offered on demand: the cluster asked
			// for is the one the authority spells, which is a port's cluster.
			for _, host := range []string{multi + ":8080", multi + ":9090", multi + ":7777"} {
				got := a043Resolve(t, table, host)
				assert.NotEqualf(t, "on_demand_catch_all", got.vhost, "Host %s takes the catch-all: its route's rules are skipped", host)
				assert.Truef(t, got.onDemand, "Host %s: a request no rule matches still names its own cluster on demand", host)
				assert.Equalf(t, host, got.cluster, "Host %s", host)
			}
			// Except the mesh's own two ports, which are a port of no
			// service: refused at once, by the stand-in for a name with a
			// rule and by the table's floor for a name with nothing. Never
			// parked for a cluster that can never exist.
			for _, host := range []string{multi + ":18081", multi + ":18082", "nothing.demo." + a043Mesh + ":18081", "nothing.demo." + a043Mesh + ":18082"} {
				got := a043Resolve(t, table, host)
				assert.Equalf(t, uint32(421), got.status, "Host %s: %+v", host, got)
				assert.Equalf(t, proxy.RefusalPortNotDeclared, got.refusal, "Host %s", host)
				assert.Emptyf(t, got.cluster, "Host %s names a cluster", host)
				assert.Falsef(t, got.onDemand, "Host %s is offered on demand", host)
			}
			// An authority WITHOUT a port is not: it would spell the bare
			// cluster name, which is no port's. Nothing says whether the
			// service has a port 80, so it is refused until something does.
			got := a043Resolve(t, table, multi)
			assert.Equal(t, uint32(421), got.status, "%+v", got)
			assert.Equal(t, proxy.RefusalPortRequired, got.refusal)
			assert.Empty(t, got.cluster)
			assert.Empty(t, rtpMarks(t, table, multi), "the port-less authority is refused: no rule rides on it")
			assert.Equal(t, []string{"all"}, rtpMarks(t, table, multi+":*"), "any other port")
			assert.Equal(t, []string{"all", "one"}, rtpMarks(t, table, multi+":9090"), "the pod port the rule's parentRef resolves to")

			// The port-specific rule's route sends to ITS backend, not to the
			// cluster the authority names.
			var toBackend bool
			vh := findVHost(table.GetVirtualHosts(), multi+":9090")
			require.NotNil(t, vh)
			for _, r := range vh.GetRoutes() {
				toBackend = toBackend || r.GetRoute().GetCluster() == "other.demo."+a043Mesh
			}
			assert.True(t, toBackend, "the rule's backendRef")
		})
	}
}

// TestPerPort_ColdUserServiceIsRoutedByItsPortMap: a service this node holds
// no endpoint of whose name a USER's Service owns. The node has that
// Service's port map, so the loopback table routes and refuses the service by
// the Service's ports, as it does once it holds an endpoint: the authority
// names a Service port, and a pod port's own number is no port of the name.
//
// Before, its stand-in claimed "<fqdn>:*": every port spelling was taken, and
// a rule's backendRef answered a port the Service does not have (the third
// review measured Host multi...:7777 delivered cold and refused warm).
//
// It holds for a name that carries a route or a filter. A port map alone does
// not make a name the mesh's (the agent has the port map of every Service in
// the cluster): TestShortName_APlainServiceIsNotAMeshName.
func TestPerPort_ColdUserServiceIsRoutedByItsPortMap(t *testing.T) {
	ctx := context.Background()
	const multi = "multi.demo." + a043Mesh
	const solo = "solo.demo." + a043Mesh
	build := func(held bool) *routev3.RouteConfiguration {
		c := newTestCache("node-1")
		c.SetCaptureEnabled(true)
		c.SetCaptureRedirectAll(true)
		require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
		declareDeps(c, "demo/multi", "demo/solo", "demo/other")
		c.SetServicePortMaps(map[string]map[uint32]uint32{
			"demo/multi": {80: 9090, 8000: 8080},
			"demo/solo":  {8000: 8080},
		})
		// solo has no route; a service-wide filter makes it a name of the mesh.
		c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/solo": spChainCfg(t)})
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {
			{Matches: []proxy.GammaMatch{{Prefix: "/all"}}, HeaderMutation: rtpMark("all")},
			{
				ParentPort: 80, Matches: []proxy.GammaMatch{{Prefix: "/one"}}, HeaderMutation: rtpMark("one"),
				Backends: []proxy.GammaBackend{{Service: "demo/other", Cluster: "other.demo." + a043Mesh, Weight: 1}},
			},
		}})
		var listed []a043Service
		if held {
			h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
			listed = []a043Service{
				{name: "multi", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.1.2"},
				{name: "solo", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.1.3"},
			}
		}
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(listed)))
		return a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	}
	cold, warm := build(false), build(true)

	// The Service's ports are delivered, to the pod port behind each.
	for _, tc := range []struct {
		host, port string
		marks      []string
	}{
		{multi + ":8000", "8080", []string{"all"}},
		{multi + ":80", "9090", []string{"all", "one"}},
		{multi, "9090", []string{"all", "one"}},
		{solo + ":8000", "8080", nil},
	} {
		got := a043Resolve(t, cold, tc.host)
		assert.Zerof(t, got.status, "cold, Host %s: %+v", tc.host, got)
		assert.Falsef(t, got.onDemand, "cold, Host %s: the cluster asked for is named by the authority, not by the port map: %+v", tc.host, got)
		assert.Truef(t, strings.HasSuffix(got.cluster, ":"+tc.port), "cold, Host %s: pod port %s is behind that Service port: %+v", tc.host, tc.port, got)
		assert.Equalf(t, tc.marks, rtpMarks(t, cold, tc.host), "cold, Host %s: the rules of that port", tc.host)
		assert.Equalf(t, tc.marks, rtpMarks(t, warm, tc.host), "warm, Host %s", tc.host)
	}
	// Every other port is refused, cold as warm: a pod port's own number,
	// a port nobody has, the mesh's port; and a name without a port when the
	// Service has no port 80.
	for _, tc := range []struct{ host, reason string }{
		{multi + ":8080", proxy.RefusalPortNotDeclared},
		{multi + ":9090", proxy.RefusalPortNotDeclared},
		{multi + ":7777", proxy.RefusalPortNotDeclared},
		{multi + ":18081", proxy.RefusalPortNotDeclared},
		{solo + ":8080", proxy.RefusalPortNotDeclared},
		{solo + ":7777", proxy.RefusalPortNotDeclared},
		{solo, proxy.RefusalPortRequired},
	} {
		for state, table := range map[string]*routev3.RouteConfiguration{"cold": cold, "warm": warm} {
			got := a043Resolve(t, table, tc.host)
			assert.Equalf(t, uint32(421), got.status, "%s, Host %s: %+v", state, tc.host, got)
			assert.Equalf(t, tc.reason, got.refusal, "%s, Host %s", state, tc.host)
			assert.Emptyf(t, rtpMarks(t, table, tc.host), "%s, Host %s: a rule rides on a refused authority", state, tc.host)
		}
	}
	// The rule with a backendRef sends to its backend on its own port only.
	for _, table := range []*routev3.RouteConfiguration{cold, warm} {
		toBackend := func(host string) bool {
			vh := findVHost(table.GetVirtualHosts(), host)
			require.NotNil(t, vh, host)
			for _, r := range vh.GetRoutes() {
				if r.GetRoute().GetCluster() == "other.demo."+a043Mesh {
					return true
				}
			}
			return false
		}
		assert.True(t, toBackend(multi+":80"))
		assert.False(t, toBackend(multi+":8000"), "the rule's parentRef names port 80")
	}
}
