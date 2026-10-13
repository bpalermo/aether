package cache

// Seventh adversarial review of the proposal-043 stack (head 7ecbd2a3). Tests
// adopted from the review, where each failed.

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/require"
)

// R7-9. Ruling 9 on the shape the sixth review's N4 fix touched: a mesh
// service the dialled-port tables have by its mesh name alone (behind a
// user's Service with a port map; or with no Service object seen at all).
// Whatever applies to the service (a chain filter, an imported chain filter,
// a local rule, an imported rule), no table may DELIVER a mesh authority of
// it without that filter or rule. A passthrough and a refusal deliver nothing.
func TestPerPort_NoDeliveryWithoutTheFilterOrRuleForAMeshOnlyName(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	checked, delivered := 0, 0
	for _, mapped := range []bool{true, false} {
		for _, held := range []bool{false, true} {
			for _, what := range []string{"chain filter", "imported chain filter", "local rule", "imported rule"} {
				c := a043Cache(t, []a043Service{multi})
				if mapped {
					c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080}})
				}
				if held {
					c.SetStaticDependencies([]string{"demo/own", "demo/multi"})
				}
				wantFilter, wantMark := false, ""
				switch what {
				case "chain filter":
					c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
					wantFilter = true
				case "imported chain filter":
					c.SetImportedServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
					wantFilter = true
				case "local rule":
					c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/own": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("local")}}})
					wantMark = "local"
				case "imported rule":
					c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{"demo/own": {{Imported: true, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("imported")}}})
					wantMark = "imported"
				}
				reg := a043Registry([]a043Service{own, multi})
				for i := 0; i < 3; i++ {
					require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
				}
				require.NoError(t, c.generateSnapshot(ctx))
				for tname, table := range a043Tables(t, c) {
					if !r5HTTPTable(tname) {
						continue
					}
					for _, host := range []string{mesh, mesh + ":80", mesh + ":8000", mesh + ":8080", mesh + ":9090", mesh + ":18081", "own", "own:8000", "own:8080", "own.demo", "own.demo:8080", "own.demo.svc.cluster.local:8080", "own.demo.svc.cluster.local:8000"} {
						a := r5Resolve(t, table, "demo", host, "/")
						checked++
						if !a.delivered {
							continue
						}
						delivered++
						if wantFilter && !a.filter {
							t.Errorf("mapped=%v held=%v %s: table %s delivers Host %s WITHOUT the filter: %s (vhost %q)", mapped, held, what, tname, host, a.sig, a.vhost)
						}
						if wantMark != "" && !s7Contains(a.marks, wantMark) {
							t.Errorf("mapped=%v held=%v %s: table %s delivers Host %s WITHOUT the rule: %s (vhost %q)", mapped, held, what, tname, host, a.sig, a.vhost)
						}
					}
				}
			}
		}
	}
	t.Logf("%d answers checked, %d of them deliveries", checked, delivered)
	require.NotZero(t, delivered)
}

func s7Contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
