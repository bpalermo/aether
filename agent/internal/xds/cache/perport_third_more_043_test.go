package cache

// Third adversarial pass on the proposal-043 stack: review tests, uncommitted.

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A rule event that changes ONLY a rule's ExtensionRef filters (an HTTPFilter
// attached to a rule, or its config edited). equalGammaRoute does not compare
// ExtensionFilters (it did not on main either), so SetServiceRoutes reports no
// change: the rule generation does not move (#1694's refreshEntryVhosts does
// nothing) and nothing asks for a snapshot. The dependency generation moves
// regardless, so the next snapshot build, whatever asks for it, rebuilds the
// capture tables with the filter and leaves the loopback table without it.
func TestPerPort_TablesAgreeAfterAnExtensionRefAlone(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	c := a043Cache(t, []a043Service{svc})
	rule := proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("r")}
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {rule}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{svc})))
	for len(c.depChanged) > 0 {
		<-c.depChanged
	}

	// The same rule gains an ExtensionRef filter (ext_authz for one rule, say).
	withExt := rule
	withExt.ExtensionFilters = []proxy.ExtensionFilter{r3ChainCfg(t)}
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {withExt}})
	asked := len(c.depChanged) > 0
	t.Logf("the event asked for a snapshot rebuild: %v", asked)
	// Any later snapshot build (a pod event).
	require.NoError(t, c.generateSnapshot(ctx))

	routeFiltered := func(table *routev3.RouteConfiguration, host string) bool {
		vh := r3Pick(t, table, host)
		for _, r := range vh.GetRoutes() {
			if r.GetRoute() != nil && r.GetMatch().GetPrefix() == "/" {
				_, ok := r.GetTypedPerFilterConfig()[r3Filter]
				return ok
			}
		}
		return false
	}
	tables := a043Tables(t, c)
	host := "echo.demo." + a043Mesh + ":8080"
	capOK := routeFiltered(tables["cap_http_8080"], host)
	loopOK := routeFiltered(tables[proxy.OutboundHTTPRouteName], host)
	t.Logf("captured dial to :8080: the rule's ExtensionRef filter is enabled=%v", capOK)
	t.Logf("loopback / proxy port : the rule's ExtensionRef filter is enabled=%v", loopOK)
	assert.True(t, asked, "an ExtensionRef-only change of a rule asks for no snapshot rebuild (pre-existing: equalGammaRoute ignores ExtensionFilters)")
	assert.Equal(t, capOK, loopOK, "after the next snapshot build the capture table runs the rule's filter and the loopback table does not")
}

// The data-plane half of gamma's TestStatus_UserServicePortIsNamedAsUnchecked:
// a backendRef to a user Service's port whose target port the pods declare
// raw TCP is INVALID in the tables (500).
func TestPerPort_UserServiceBackendToARawTCPPortIs500(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	echo := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	db := a043Service{name: "db", ns: "demo", legacy: 5432, ports: map[uint32]registryv1.PortProtocol{5432: tcp}, ip: "10.0.9.2", vip: "10.96.9.2"}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{echo})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/db": {5432: 5432}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/db", Cluster: "db.demo." + a043Mesh, Port: 5432, Weight: 1}},
	}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{echo, db})))
	got := r3Resolve(t, a043Tables(t, c)["cap_http_8080"], "demo", "echo.demo."+a043Mesh+":8080", nil)
	t.Logf("captured dial to echo:8080, rule backendRef db:5432 (raw TCP) -> %s", got.first)
	assert.Contains(t, got.first, "direct 500")
}
