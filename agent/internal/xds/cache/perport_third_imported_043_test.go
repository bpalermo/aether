package cache

// Third adversarial pass on the proposal-043 stack: review test, uncommitted.

import (
	"context"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A rule IMPORTED from a peer cluster (proposal 026) carries no port: it is
// kept on the virtual hosts of the service's LEGACY TARGET (ruleApplies). The
// legacy target is derived from the registry's rows. A service the node holds
// no endpoint of, known from its generated Service's ports (#1696's strict
// cold), has no legacy target: its declared ports carry none. So while the
// node is cold the imported rule is placed NOWHERE, the request is parked on
// the port's own cluster and delivered without the rule's matches, filters
// and backends; once the node holds an endpoint the rule applies.
func TestImportedRule_ColdAndWarmAlike(t *testing.T) {
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	build := func(held bool) *SnapshotCache {
		c := a043Cache(t, nil)
		a043Project(c, []a043Service{svc})
		c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{
			Matches:          []proxy.GammaMatch{{Prefix: "/"}},
			HeaderMutation:   rtpMark("imported"),
			ExtensionFilters: []proxy.ExtensionFilter{r3ChainCfg(t)},
		}}})
		var listed []a043Service
		if held {
			listed = []a043Service{svc}
		}
		require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(listed)))
		return c
	}
	legacy := map[string]uint32{"echo.demo." + a043Mesh: 8080}
	cold, warm := a043Tables(t, build(false)), a043Tables(t, build(true))
	for _, tc := range []struct{ table, host, ns string }{
		{proxy.OutboundHTTPRouteName, "echo.demo." + a043Mesh + ":8080", "demo"},
		{"cap_http_18081", "echo.demo." + a043Mesh + ":8080", "demo"},
		{"cap_http_8080", "echo.demo." + a043Mesh + ":8080", "demo"},
		{"cap_http_8080", "echo.demo.svc.cluster.local", "demo"},
		{"cap_http_8080", "echo:8080", "demo"},
	} {
		require.NotNil(t, cold[tc.table], tc.table)
		require.NotNil(t, warm[tc.table], tc.table)
		cs := r3Resolve(t, cold[tc.table], tc.ns, tc.host, legacy)
		ws := r3Resolve(t, warm[tc.table], tc.ns, tc.host, legacy)
		t.Logf("%-15s Host %-34s cold: %s", tc.table, tc.host, cs.first)
		t.Logf("%-15s Host %-34s warm: %s", tc.table, tc.host, ws.first)
		require.True(t, strings.Contains(ws.first, "+x-rv=imported"), "warm, the imported rule applies on the legacy target's port")
		assert.Equalf(t, ws.first, cs.first, "table %s, Host %s: a cold request is delivered without the imported route's rule (its matches, its filters, its backends); a warm one runs it", tc.table, tc.host)
	}
}
