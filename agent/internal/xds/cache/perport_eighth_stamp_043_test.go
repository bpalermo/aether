package cache

// Eighth adversarial review of the proposal-043 stack (head f5ed0ee5). Tests
// adopted from the review.

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// s8Rebuilt returns the loopback and proxy-port tables as the cache publishes
// them now (settled: one snapshot build after everything else returned), and
// as it publishes them after EVERY entry's stamp is cleared, which forces the
// snapshot build to rebuild every stored virtual host from the inputs as they
// are. The two must be the same: a stored virtual host that the stamps call
// current and that a rebuild from the current inputs changes is a virtual
// host "marked current without being rebuilt".
func s8Rebuilt(t *testing.T, c *SnapshotCache) (settled, forced map[string]*routev3.RouteConfiguration) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, c.generateSnapshot(ctx))
	settled = a043Tables(t, c)
	c.clusterMu.Lock()
	for name, e := range c.clusters {
		e.vhostAt = vhostStamp{}
		c.clusters[name] = e
	}
	c.clusterMu.Unlock()
	require.NoError(t, c.generateSnapshot(ctx))
	forced = a043Tables(t, c)
	return settled, forced
}

// The per-entry stamp against every setter that advances a generation,
// racing one or two registry refreshes and a snapshot build, with a retained
// front, a backend whose ports move, a user Service's port map, imported
// rules and filters, and the generated Services' declarations.
func TestPerPort_StampsNeverVouchForAStaleVirtualHost(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	tcp := registryv1.PortProtocol_PORT_PROTOCOL_TCP
	seed := int64(r4Env("S8_SEED", 1))
	rounds := r4Env("S8_ROUNDS", 300)
	rng := rand.New(rand.NewSource(seed))

	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	front2 := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	backs := []a043Service{
		{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"},
		{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.5.2", vip: "10.96.5.2"},
		{name: "back", ns: "demo", legacy: 9090, ports: map[uint32]registryv1.PortProtocol{9090: h}, ip: "10.0.5.3", vip: "10.96.5.2"},
		{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: tcp}, ip: "10.0.5.2", vip: "10.96.5.2"},
	}
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.4"}
	mesh := func(n string) string { return n + ".demo." + a043Mesh }
	routesOf := func(i int) map[string][]proxy.GammaRoute {
		switch i % 5 {
		case 0:
			return nil
		case 1:
			return map[string][]proxy.GammaRoute{"demo/front": {{
				Matches:  []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: mesh("back"), Port: 8080, Weight: 1}},
			}}}
		case 2:
			return map[string][]proxy.GammaRoute{"demo/front": {{
				Matches:  []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: mesh("back"), Port: 9090, Weight: 1}},
			}}}
		case 3:
			return map[string][]proxy.GammaRoute{"demo/front": {{
				Matches:  []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{{Service: "demo/own", Cluster: mesh("own"), Port: 80, Weight: 1}},
			}}}
		default:
			return map[string][]proxy.GammaRoute{
				"demo/front": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("rule")}},
				"demo/back":  {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("backrule")}},
			}
		}
	}
	filtersOf := func(i int) map[string]proxy.ExtensionFilter {
		out := map[string]proxy.ExtensionFilter{}
		if i&1 != 0 {
			out["demo/front"] = r3ChainCfg(t)
		}
		if i&2 != 0 {
			out["demo/back"] = r3ChainCfg(t)
		}
		if i&4 != 0 {
			out["demo/own"] = r3ChainCfg(t)
		}
		return out
	}
	portMapsOf := func(i int) map[string]map[uint32]uint32 {
		switch i % 4 {
		case 0:
			return map[string]map[uint32]uint32{"demo/own": {80: 8080}}
		case 1:
			return map[string]map[uint32]uint32{"demo/own": {80: 9090}}
		case 2:
			return map[string]map[uint32]uint32{"demo/own": {80: 8080, 8000: 8080}}
		default:
			return nil
		}
	}

	c := a043Cache(t, nil)
	c.SetStaticDependencies([]string{"demo/front", "demo/back", "demo/own"})
	a043Project(c, []a043Service{front, backs[0]})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{front, backs[0], own})))

	type event struct {
		name string
		run  func()
	}
	pick := func() event {
		n := rng.Intn(1 << 20)
		switch rng.Intn(9) {
		case 0:
			return event{fmt.Sprintf("SetServiceChainFilters(%d)", n%8), func() { c.SetServiceChainFilters(filtersOf(n)) }}
		case 1:
			return event{fmt.Sprintf("SetImportedServiceChainFilters(%d)", n%8), func() { c.SetImportedServiceChainFilters(filtersOf(n)) }}
		case 2:
			return event{fmt.Sprintf("SetServiceRoutes(%d)", n%5), func() { c.SetServiceRoutes(routesOf(n)) }}
		case 3:
			return event{fmt.Sprintf("SetImportedServiceRoutes(%d)", n%5), func() { c.SetImportedServiceRoutes(routesOf(n)) }}
		case 4:
			return event{fmt.Sprintf("SetServicePortMaps(%d)", n%4), func() { c.SetServicePortMaps(portMapsOf(n)) }}
		case 5:
			b := backs[n%len(backs)]
			return event{fmt.Sprintf("project(back=%v)", b.ports), func() { a043Project(c, []a043Service{front, b}) }}
		case 6:
			return event{"project(front only)", func() { a043Project(c, []a043Service{front}) }}
		case 7:
			return event{"project(front with 9090, back)", func() { a043Project(c, []a043Service{front2, backs[1]}) }}
		default:
			return event{"expire the grace", func() { s6ExpireGrace(c) }}
		}
	}
	registryOf := func() (string, *mockRegistry) {
		var svcs []a043Service
		desc := ""
		switch rng.Intn(4) {
		case 0:
		case 1:
			svcs = append(svcs, front)
			desc += "front "
		case 2:
			svcs = append(svcs, front2)
			desc += "front+9090 "
		default:
		}
		if rng.Intn(5) != 0 {
			b := backs[rng.Intn(len(backs))]
			svcs = append(svcs, b)
			desc += fmt.Sprintf("back%v ", b.ports)
		}
		if rng.Intn(3) != 0 {
			svcs = append(svcs, own)
			desc += "own "
		}
		return desc, a043Registry(svcs)
	}

	bad := 0
	for i := 0; i < rounds; i++ {
		var trace []string
		var wg sync.WaitGroup
		nEvents, nRefresh := 1+rng.Intn(3), 1+rng.Intn(2)
		for j := 0; j < nEvents; j++ {
			ev := pick()
			trace = append(trace, ev.name)
			d := time.Duration(rng.Intn(60)) * 5 * time.Microsecond
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(d); ev.run() }()
		}
		for j := 0; j < nRefresh; j++ {
			desc, reg := registryOf()
			trace = append(trace, "refresh{"+desc+"}")
			d := time.Duration(rng.Intn(60)) * 5 * time.Microsecond
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(d)
				_ = c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg)
			}()
		}
		if rng.Intn(2) == 0 {
			trace = append(trace, "snapshot")
			d := time.Duration(rng.Intn(60)) * 5 * time.Microsecond
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(d); _ = c.generateSnapshot(ctx) }()
		}
		wg.Wait()
		settled, forced := s8Rebuilt(t, c)
		for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
			if settled[table] == nil || forced[table] == nil {
				continue
			}
			if !proto.Equal(settled[table], forced[table]) {
				bad++
				if bad <= 3 {
					t.Errorf("round %d (seed %d) %v: table %s as published differs from the same table with every stored virtual host rebuilt from the current inputs:\n    %v",
						i, seed, trace, table, r4TableDiff(settled[table], forced[table]))
				}
			}
		}
		r3NoDuplicateDomains(t, fmt.Sprintf("round %d %v", i, trace), forced)
	}
	t.Logf("seed %d, %d rounds, %d rounds with a stale stored virtual host: %d times", seed, rounds, bad, bad)
	require.Zero(t, bad)
}
