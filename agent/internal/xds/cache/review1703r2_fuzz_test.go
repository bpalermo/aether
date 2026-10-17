package cache

// Review of #1703, second pass: random cache inputs, and for each the
// published cap_http must (1) compile (no fail-closed refusal table) and (2)
// answer every probe as the per-port table of its dialled port does.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

func r2Fixture(rng *rand.Rand) []a043Service {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	pool := []uint32{80, 8080, 9090, 9000, 7070}
	var out []a043Service
	seen := map[string]bool{}
	n := 2 + rng.Intn(5)
	for i := 0; len(out) < n && i < 50; i++ {
		name := []string{"a", "b", "multi", "web"}[rng.Intn(4)]
		ns := []string{"demo", "other"}[rng.Intn(2)]
		if seen[ns+"/"+name] {
			continue
		}
		seen[ns+"/"+name] = true
		ports := map[uint32]registryv1.PortProtocol{}
		for _, p := range pool {
			if rng.Intn(3) == 0 {
				c := h
				if rng.Intn(4) == 0 {
					c = tcp
				}
				ports[p] = c
			}
		}
		if len(ports) == 0 {
			ports[pool[rng.Intn(len(pool))]] = h
		}
		keys := make([]uint32, 0, len(ports))
		for p := range ports {
			keys = append(keys, p)
		}
		slices.Sort(keys)
		k := len(out)
		out = append(out, a043Service{
			name: name, ns: ns, legacy: keys[rng.Intn(len(keys))], ports: ports,
			ip: fmt.Sprintf("10.0.3.%d", k+1), vip: fmt.Sprintf("10.96.3.%d", k+1),
		})
	}
	return out
}

var r2MaxRegex int

func TestReview1703R2_FuzzCompiledTable(t *testing.T) {
	seeds := 60
	if s := os.Getenv("R2_SEEDS"); s != "" {
		seeds, _ = strconv.Atoi(s)
	}
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	const filter = "envoy.filters.http.header_mutation"
	for seed := 0; seed < seeds; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(seed)))
			services := r2Fixture(rng)
			var c *SnapshotCache
			if rng.Intn(4) == 0 {
				c = newTestCache("node-1")
				c.SetCaptureEnabled(true)
				c.SetCaptureRedirectAll(rng.Intn(2) == 0)
				a043ProjectWith(c, services, rng.Intn(2) == 0)
			} else {
				c = a043Cache(t, services)
				c.SetCaptureRedirectAll(rng.Intn(3) != 0)
			}
			routes := map[string][]proxy.GammaRoute{}
			imported := map[string][]proxy.GammaRoute{}
			filters := map[string]proxy.ExtensionFilter{}
			portMaps := map[string]map[uint32]uint32{}
			targets := map[string][]uint32{}
			for _, s := range services {
				if rng.Intn(2) == 0 {
					var rs []proxy.GammaRoute
					for j := 0; j < 1+rng.Intn(3); j++ {
						r := proxy.GammaRoute{HeaderMutation: rtpMark(fmt.Sprintf("%s-%d", s.key(), j))}
						switch rng.Intn(4) {
						case 0:
							r.Matches = []proxy.GammaMatch{{Prefix: "/x"}}
						case 1:
							r.Matches = []proxy.GammaMatch{{Exact: "/e"}}
						case 2:
							r.Matches = []proxy.GammaMatch{{Prefix: "/", Headers: []proxy.GammaHeaderMatch{{Name: "x-h", Value: "v"}}}}
						default:
							r.Matches = []proxy.GammaMatch{{Prefix: "/"}}
						}
						switch rng.Intn(3) {
						case 0:
							for p := range s.ports {
								r.ParentPort = p
								break
							}
						case 1:
							r.ParentPort = []uint32{80, 8080, 9090, 9000, 7070}[rng.Intn(5)]
						}
						if rng.Intn(3) == 0 {
							o := services[rng.Intn(len(services))]
							bp := []uint32{80, 8080, 9090, 9000, 7070}[rng.Intn(5)]
							r.Backends = []proxy.GammaBackend{{Service: o.key(), Cluster: o.name + "." + o.ns + "." + a043Mesh, Weight: 1, Port: bp}}
						}
						rs = append(rs, r)
					}
					routes[s.key()] = rs
					tp := []uint32{}
					for p := range s.ports {
						tp = append(tp, p)
					}
					slices.Sort(tp)
					targets[s.key()] = tp
				}
				if rng.Intn(3) == 0 {
					filters[s.key()] = proxy.ExtensionFilter{Name: filter, Config: cfg}
				}
				if rng.Intn(4) == 0 {
					imported[s.key()] = []proxy.GammaRoute{{Imported: true, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("imported")}}
				}
				if rng.Intn(5) == 0 {
					m := map[uint32]uint32{}
					for p := range s.ports {
						m[[]uint32{80, 8000, p}[rng.Intn(3)]] = p
					}
					if rng.Intn(2) == 0 {
						m[8001] = 0
					}
					portMaps[s.key()] = m
				}
			}
			c.SetServiceRoutes(routes)
			c.SetImportedServiceRoutes(imported)
			c.SetServiceChainFilters(filters)
			c.SetServicePortMaps(portMaps)
			c.SetRouteTargetPorts(targets)
			require.NoError(t, c.generateSnapshot(context.Background()))
			merged := a043Published(t, c)
			for _, vh := range merged.GetVirtualHosts() {
				require.NotEqual(t, "capture_table_unbuilt", vh.GetName(), "seed %d: the compile failed and the node refuses every mesh name: services %+v routes %+v portMaps %v", seed, services, routes, portMaps)
			}
			for _, vh := range merged.GetVirtualHosts() {
				for _, r := range vh.GetRoutes() {
					for _, h := range r.GetMatch().GetHeaders() {
						if re := h.GetStringMatch().GetSafeRegex().GetRegex(); h.GetName() == ":authority" && len(re) > r2MaxRegex {
							r2MaxRegex = len(re)
							t.Logf("longest :authority regex so far: %q (vhost %s, %d routes in table)", re, vh.GetName(), len(vh.GetRoutes()))
						}
					}
				}
			}
			perPort, other := c.capturePerPortTables(c.captureTableInputs(), c.captureHTTPPorts(), c.proxyPortTableNow(c.captureHTTPPorts()))
			bad := compiledMismatches(merged, perPort, other, 5, true)
			require.Empty(t, bad, "seed %d: services %+v", seed, services)
		})
	}
}
