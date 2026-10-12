package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
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
	for name, portMap := range map[string]map[uint32]uint32{
		"a generated Service":             nil,
		"a user Service, port 80 -> 9090": {80: 9090, 8080: 8080},
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
