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
			a043Project(c, a043Fixture())
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

			for _, host := range []string{multi, multi + ":8080", multi + ":9090", multi + ":7777"} {
				got := a043Resolve(t, table, host)
				assert.NotEqualf(t, "on_demand_catch_all", got.vhost, "Host %s takes the catch-all: its route's rules are skipped", host)
				assert.Truef(t, got.onDemand, "Host %s: a request no rule matches still names its own cluster on demand", host)
			}
			assert.Equal(t, []string{"all"}, rtpMarks(t, table, multi), "port-less authority")
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
