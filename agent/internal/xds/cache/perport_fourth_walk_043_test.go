package cache

// Fourth adversarial pass on the proposal-043 stack (head 7b3a177a). Tests
// adopted from the review.

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// r4Svc is one service of a random mesh state.
type r4Svc struct {
	a043Service
	// mode: 0 generated Service with real ports, 1 generated Service, name
	// only, 2 a user's Service owns the name (port map), 3 no Service at all.
	mode    int
	portMap map[uint32]uint32
	held    bool
}

type r4State struct {
	svcs     []r4Svc
	filters  map[string]proxy.ExtensionFilter
	routes   map[string][]proxy.GammaRoute
	imported map[string][]proxy.GammaRoute
}

func r4Env(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		n, _ := strconv.Atoi(s)
		return n
	}
	return def
}

// r4Random draws a state. shape, when not nil, fixes which services exist,
// their mode and whether the node holds them (so that a walk never REMOVES a
// service from the registry: the grace of #1635 is then never in play, and
// the incremental cache must equal a fresh one).
func r4Random(t *testing.T, rng *rand.Rand, shape *r4State) r4State {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	st := r4State{filters: map[string]proxy.ExtensionFilter{}, routes: map[string][]proxy.GammaRoute{}, imported: map[string][]proxy.GammaRoute{}}
	httpPool := []uint32{80, 8080, 9090}
	i := 0
	for _, ns := range []string{"demo", "other"} {
		for _, name := range []string{"echo", "solo"} {
			i++
			var s r4Svc
			if shape != nil {
				found := false
				for _, p := range shape.svcs {
					if p.name == name && p.ns == ns {
						s.mode, s.held, found = p.mode, p.held, true
					}
				}
				if !found {
					continue
				}
			} else {
				if rng.Intn(4) == 0 {
					continue
				}
				s.mode = rng.Intn(4)
				s.held = rng.Intn(2) == 0
			}
			s.a043Service = a043Service{name: name, ns: ns, ports: map[uint32]registryv1.PortProtocol{}, ip: fmt.Sprintf("10.0.8.%d", i), vip: fmt.Sprintf("10.96.8.%d", i)}
			for _, p := range httpPool {
				if rng.Intn(2) == 0 {
					s.ports[p] = h
				}
			}
			if rng.Intn(3) == 0 {
				s.ports[9000] = tcp
			}
			if len(s.ports) == 0 {
				s.ports[8080] = h
			}
			nums := slices.Sorted(maps.Keys(s.ports))
			s.legacy = nums[rng.Intn(len(nums))]
			if s.mode == 2 {
				s.portMap = map[uint32]uint32{}
				for _, sp := range []uint32{80, 8000, 8080, 9000} {
					if rng.Intn(2) == 0 {
						s.portMap[sp] = []uint32{8080, 9090, 80, 9000, 7000}[rng.Intn(5)]
					}
				}
				if len(s.portMap) == 0 {
					s.portMap[8000] = 8080
				}
			}
			st.svcs = append(st.svcs, s)
			if rng.Intn(2) == 0 {
				st.filters[s.key()] = r3ChainCfg(t)
			}
			if rng.Intn(2) == 0 {
				st.routes[s.key()] = append(st.routes[s.key()], proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("every:" + s.key())})
			}
			if rng.Intn(3) == 0 {
				pp := []uint32{80, 8080, 9090, 8000}[rng.Intn(4)]
				st.routes[s.key()] = append(st.routes[s.key()], proxy.GammaRoute{ParentPort: pp, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark(fmt.Sprintf("port%d:%s", pp, s.key()))})
			}
			if rng.Intn(4) == 0 {
				st.routes[s.key()] = append(st.routes[s.key()], proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/x"}}, ExtensionFilters: []proxy.ExtensionFilter{r3ChainCfg(t)}})
			}
			if rng.Intn(4) == 0 {
				st.imported[s.key()] = append(st.imported[s.key()], proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("imported:" + s.key())})
			}
		}
	}
	if len(st.svcs) >= 2 && rng.Intn(2) == 0 {
		from, to := st.svcs[rng.Intn(len(st.svcs))], st.svcs[rng.Intn(len(st.svcs))]
		bp := []uint32{80, 8080, 9090, 8000, 9000}[rng.Intn(5)]
		st.routes[from.key()] = append(st.routes[from.key()], proxy.GammaRoute{
			Matches:  []proxy.GammaMatch{{Prefix: "/b"}},
			Backends: []proxy.GammaBackend{{Service: to.key(), Cluster: to.name + "." + to.ns + "." + a043Mesh, Port: bp, Weight: 1}},
		})
	}
	return st
}

// r4Ops returns what each reconciler pushes for st, one closure each.
func r4Ops(c *SnapshotCache, st r4State) map[string]func() {
	ctx := context.Background()
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	portMaps := map[string]map[uint32]uint32{}
	targets := map[string][]uint32{}
	var listed []a043Service
	for _, s := range st.svcs {
		switch s.mode {
		case 0, 1:
			authorities[s.key()] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
			e := capture.CaptureTCPService{ServiceName: s.key(), ClusterIP: s.vip}
			if s.mode == 0 {
				for port, class := range s.ports {
					if class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
						e.DeclaredHTTP = append(e.DeclaredHTTP, port)
					} else {
						e.DeclaredOther = append(e.DeclaredOther, port)
					}
				}
				slices.Sort(e.DeclaredHTTP)
				slices.Sort(e.DeclaredOther)
			}
			vips = append(vips, e)
		case 2:
			portMaps[s.key()] = s.portMap
		}
		if s.held {
			listed = append(listed, s.a043Service)
		}
	}
	for svc, rules := range st.routes {
		set := map[uint32]struct{}{}
		for _, r := range rules {
			if r.ParentPort != 0 {
				set[r.ParentPort] = struct{}{}
			}
		}
		if len(set) > 0 {
			targets[svc] = slices.Sorted(maps.Keys(set))
		}
	}
	reg := a043Registry(listed)
	return map[string]func(){
		"authorities": func() { c.SetCaptureAuthorities(authorities) },
		"services":    func() { c.SetCaptureTCPServices(vips) },
		"portmaps":    func() { c.SetServicePortMaps(portMaps) },
		"routes":      func() { c.SetServiceRoutes(st.routes) },
		"imported":    func() { c.SetImportedServiceRoutes(st.imported) },
		"filters":     func() { c.SetServiceChainFilters(st.filters) },
		"targets":     func() { c.SetRouteTargetPorts(targets) },
		"load":        func() { _ = c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg) },
	}
}

var r4Order = []string{"authorities", "services", "portmaps", "routes", "imported", "filters", "targets"}

func r4Fresh(t *testing.T, st r4State) *SnapshotCache {
	t.Helper()
	c := a043Cache(t, nil)
	ops := r4Ops(c, st)
	for _, k := range r4Order {
		ops[k]()
	}
	ops["load"]()
	require.NoError(t, c.generateSnapshot(context.Background()))
	return c
}

func r4Describe(st r4State) string {
	var b strings.Builder
	for _, s := range st.svcs {
		fmt.Fprintf(&b, "\n    %s mode=%d held=%v ports=%v legacy=%d portMap=%v filter=%v routes=%d imported=%d", s.key(), s.mode, s.held, s.ports, s.legacy, s.portMap, st.filters[s.key()].Name != "", len(st.routes[s.key()]), len(st.imported[s.key()]))
		for _, r := range st.routes[s.key()] {
			fmt.Fprintf(&b, "\n        rule parentPort=%d matches=%v backends=%v ext=%d", r.ParentPort, r.Matches, r.Backends, len(r.ExtensionFilters))
		}
	}
	return b.String()
}

// r4TableDiff says which virtual hosts of two tables differ.
func r4TableDiff(a, b *routev3.RouteConfiguration) []string {
	idx := func(rc *routev3.RouteConfiguration) map[string]*routev3.VirtualHost {
		m := map[string]*routev3.VirtualHost{}
		for _, vh := range rc.GetVirtualHosts() {
			m[vh.GetName()+" "+strings.Join(vh.GetDomains(), ",")] = vh
		}
		return m
	}
	ma, mb := idx(a), idx(b)
	var out []string
	for k, va := range ma {
		vb, ok := mb[k]
		switch {
		case !ok:
			out = append(out, "only incremental: "+k)
		case !proto.Equal(va, vb):
			out = append(out, "differs: "+k+"\n      incremental: "+protojson.Format(va)[:min(700, len(protojson.Format(va)))]+"\n      fresh:       "+protojson.Format(vb)[:min(700, len(protojson.Format(vb)))])
		}
	}
	for k := range mb {
		if _, ok := ma[k]; !ok {
			out = append(out, "only fresh: "+k)
		}
	}
	sort.Strings(out)
	return out
}

// r4FilterInvariant: no table delivers a request addressed to a service that
// carries a service-wide filter on a route that runs without it. Returns what
// it found, by kind.
func r4FilterInvariant(t *testing.T, tables map[string]*routev3.RouteConfiguration, st r4State, found map[string]int, show func(string)) {
	t.Helper()
	for _, s := range st.svcs {
		if _, ok := st.filters[s.key()]; !ok {
			continue
		}
		forms := []string{s.name, s.name + "." + s.ns + "." + a043Mesh, s.name + "." + s.ns, s.name + "." + s.ns + ".svc", s.name + "." + s.ns + ".svc.cluster.local"}
		for tname, table := range tables {
			if !strings.HasPrefix(tname, "cap_http") && tname != proxy.OutboundHTTPRouteName {
				continue
			}
			for fi, form := range forms {
				for _, suffix := range []string{"", ":80", ":8000", ":8080", ":9090", ":9000", ":7777", ":18081", ":18082"} {
					host := form + suffix
					vh := r3Pick(t, table, host)
					routes := vh.GetRoutes()
					if vh.GetMatcher() != nil {
						routes = matcherArm(t, vh.GetMatcher(), s.ns) // the caller is in the service's namespace
					}
					for _, path := range []string{"/", "/x", "/b"} {
						for _, r := range routes {
							m := r.GetMatch()
							ok := true
							for _, hm := range m.GetHeaders() {
								if hm.GetName() == ":authority" {
									ok = ok && mustRegexp(t, hm.GetStringMatch().GetSafeRegex().GetRegex()).MatchString(host)
								} else {
									ok = false
								}
							}
							switch {
							case m.GetPath() != "":
								ok = ok && m.GetPath() == path
							case m.GetPrefix() != "":
								ok = ok && strings.HasPrefix(path, m.GetPrefix())
							case m.GetPathSeparatedPrefix() != "":
								ok = ok && (path == m.GetPathSeparatedPrefix() || strings.HasPrefix(path, m.GetPathSeparatedPrefix()+"/"))
							}
							if !ok {
								continue
							}
							if m.GetRuntimeFraction() != nil {
								continue // a share of the requests; the next route takes the rest
							}
							if r.GetRoute() == nil {
								break // answered by the proxy
							}
							if r.GetRoute().GetCluster() == proxy.PassthroughClusterName {
								tk := tname
								if strings.HasPrefix(tname, "cap_http_") && tname != "cap_http_18081" && tname != "cap_http_other" {
									tk = "cap_http_<real>"
									if _, isPort := s.ports[r4PortOf(tname)]; isPort {
										tk = "cap_http_<a port of the service>"
									}
								}
								kind := fmt.Sprintf("PASSTHROUGH mode=%d held=%v form=%d table=%s", s.mode, s.held, fi, tk)
								if found[kind] == 0 && os.Getenv("R4_SHOW_PASS") != "" {
									show(fmt.Sprintf("%s: e.g. table %s Host %s (service %s ports %v portMap %v)", kind, tname, host, s.key(), s.ports, s.portMap))
								}
								found[kind]++
								break
							}
							_, onVh := vh.GetTypedPerFilterConfig()[r3Filter]
							_, onRoute := r.GetTypedPerFilterConfig()[r3Filter]
							if !onVh && !onRoute {
								kind := fmt.Sprintf("DELIVERED WITHOUT THE FILTER mode=%d held=%v form=%d table=%s", s.mode, s.held, fi, map[bool]string{true: "loopback", false: "capture"}[tname == proxy.OutboundHTTPRouteName])
								if found[kind] == 0 {
									show(fmt.Sprintf("%s: table %s Host %s path %s (service %s) -> vhost %q route %v", kind, tname, host, path, s.key(), vh.GetName(), r.GetAction()))
								}
								found[kind]++
							}
							break
						}
					}
				}
			}
		}
	}
}

// A walk over mesh states in which no service ever leaves the registry (so
// the grace of #1635 is never in play). After every step the reconcilers'
// pushes and the registry refresh land in a RANDOM order, and a snapshot is
// built after each of them. Two things are held:
//
//  1. in every snapshot, the settled one and the ones in between: no domain
//     claimed twice, and no request to a filtered service on a route without
//     its filter;
//  2. once the step has settled (a registry refresh ran after the last push,
//     as signalDependencyChange arranges): the tables are exactly the ones a
//     cache that was started in that state builds. Anything else is state that
//     survived a transition.
func TestPerPort_WalkIsHistoryIndependent(t *testing.T) {
	seed := int64(r4Env("R4_SEED", 44))
	walks, steps := r4Env("R4_WALKS", 12), r4Env("R4_STEPS", 8)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	diffs := map[string]int{}
	found := map[string]int{}
	shown := 0
	show := func(s string) {
		if shown < 30 {
			shown++
			t.Log(s)
		}
	}
	for w := 0; w < walks; w++ {
		first := r4Random(t, rng, nil)
		c := r4Fresh(t, first)
		prev := first
		for step := 0; step < steps; step++ {
			st := r4Random(t, rng, &first)
			ops := r4Ops(c, st)
			order := slices.Clone(r4Order)
			order = append(order, "load")
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			for _, k := range order {
				ops[k]()
				require.NoError(t, c.generateSnapshot(ctx))
				tables := a043Tables(t, c)
				r3NoDuplicateDomains(t, fmt.Sprintf("seed %d walk %d step %d after %s", seed, w, step, k), tables)
			}
			// Settle: the refresh that follows the last push.
			ops["load"]()
			require.NoError(t, c.generateSnapshot(ctx))
			got := a043Tables(t, c)
			r4FilterInvariant(t, got, st, found, func(s string) { show(fmt.Sprintf("seed %d walk %d step %d: %s", seed, w, step, s)) })
			want := a043Tables(t, r4Fresh(t, st))
			for name, wt := range want {
				gt, ok := got[name]
				if !ok {
					diffs["table missing from the incremental cache: "+name]++
					continue
				}
				if proto.Equal(gt, wt) {
					continue
				}
				kind := "table differs: " + name
				if strings.HasPrefix(name, "cap_http_") {
					kind = "table differs: cap_http_*"
				}
				if diffs[kind] == 0 {
					show(fmt.Sprintf("seed %d walk %d step %d order %v: %s\n  %s\n  PREVIOUS state:%s\n  THIS state:%s", seed, w, step, order, name, strings.Join(r4TableDiff(gt, wt), "\n  "), r4Describe(prev), r4Describe(st)))
				}
				diffs[kind]++
			}
			for name := range got {
				if _, ok := want[name]; !ok {
					diffs["table only in the incremental cache"]++
				}
			}
			prev = st
		}
	}
	for _, k := range slices.Sorted(maps.Keys(found)) {
		if strings.HasPrefix(k, "DELIVERED") {
			t.Errorf("%s: %d times", k, found[k])
		} else {
			t.Logf("%s: %d times", k, found[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(diffs)) {
		t.Errorf("incremental != fresh: %s: %d times", k, diffs[k])
	}
}

func r4PortOf(table string) uint32 {
	n, _ := strconv.Atoi(strings.TrimPrefix(table, "cap_http_"))
	return uint32(n)
}
