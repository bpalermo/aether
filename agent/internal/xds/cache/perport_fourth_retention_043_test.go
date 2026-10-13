package cache

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// r4AllRouteLists returns every route list of a virtual host: its own, and
// every arm and no-match list of its matcher.
func r4AllRouteLists(vh *routev3.VirtualHost) [][]*routev3.Route {
	out := [][]*routev3.Route{vh.GetRoutes()}
	var walk func(m *xdsmatcherv3.Matcher)
	onMatch := func(on *xdsmatcherv3.Matcher_OnMatch) {
		if on == nil {
			return
		}
		if on.GetMatcher() != nil {
			walk(on.GetMatcher())
		}
		if a := on.GetAction(); a != nil {
			var rl routev3.RouteList
			if a.GetTypedConfig().UnmarshalTo(&rl) == nil {
				out = append(out, rl.GetRoutes())
			}
		}
	}
	walk = func(m *xdsmatcherv3.Matcher) {
		if m == nil {
			return
		}
		onMatch(m.GetOnNoMatch())
		for _, fm := range m.GetMatcherList().GetMatchers() {
			onMatch(fm.GetOnMatch())
		}
		for _, on := range m.GetMatcherTree().GetExactMatchMap().GetMap() {
			onMatch(on)
		}
	}
	walk(vh.GetMatcher())
	return out
}

func r4RouteClusters(r *routev3.Route) []string {
	ra := r.GetRoute()
	if ra == nil {
		return nil
	}
	var out []string
	if ra.GetCluster() != "" {
		out = append(out, ra.GetCluster())
	}
	for _, wc := range ra.GetWeightedClusters().GetClusters() {
		out = append(out, wc.GetName())
	}
	if arms, noMatch, ok := proxy.QUICSelectionArms(r); ok {
		out = append(out, noMatch)
		for _, c := range arms {
			out = append(out, c)
		}
	}
	return out
}

// r4NoHTTPToANonHTTPPort: ruling 7. "Never an HTTP request to an endpoint whose
// row does not declare that port as HTTP", grace or not: every cluster an HTTP
// route table names, that the snapshot carries with endpoints, holds only pods
// whose CURRENT row declares the cluster's port as HTTP.
func r4NoHTTPToANonHTTPPort(t *testing.T, c *SnapshotCache, st r4State, found map[string]int, show func(string)) {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	byIP := map[string]r4Svc{}
	byMesh := map[string]r4Svc{}
	for _, s := range st.svcs {
		byIP[s.ip] = s
		byMesh[s.name+"."+s.ns+"."+a043Mesh] = s
	}
	clas := map[string]*endpointv3.ClusterLoadAssignment{}
	for name, res := range snap.GetResources(resourcev3.EndpointType) {
		clas[name] = res.(*endpointv3.ClusterLoadAssignment)
	}
	members := func(cluster string) []string {
		res, ok := snap.GetResources(resourcev3.ClusterType)[cluster]
		if !ok {
			return nil
		}
		cl := res.(*clusterv3.Cluster)
		eds := cl.GetEdsClusterConfig().GetServiceName()
		if eds == "" {
			eds = cluster
		}
		var ips []string
		for _, l := range clas[eds].GetEndpoints() {
			for _, e := range l.GetLbEndpoints() {
				ips = append(ips, e.GetEndpoint().GetAddress().GetSocketAddress().GetAddress())
			}
		}
		return ips
	}
	for tname, table := range a043Tables(t, c) {
		if !strings.HasPrefix(tname, "cap_http") && tname != proxy.OutboundHTTPRouteName {
			continue
		}
		for _, vh := range table.GetVirtualHosts() {
			for _, routes := range r4AllRouteLists(vh) {
				for _, r := range routes {
					for _, cluster := range r4RouteClusters(r) {
						name := strings.TrimPrefix(cluster, "quic:")
						var port uint32
						fq := name
						if i := strings.LastIndexByte(name, ':'); i >= 0 {
							fmt.Sscanf(name[i+1:], "%d", &port)
							fq = name[:i]
						}
						owner, isMesh := byMesh[fq]
						for _, ip := range members(cluster) {
							s, known := byIP[ip]
							what := ""
							switch {
							case !known || !s.held:
								what = "a pod that is in no listing now"
							case isMesh && s.key() != owner.key():
								what = "a pod of ANOTHER service (" + s.key() + ")"
							case port == 0 && s.ports[s.legacy] != registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
								what = fmt.Sprintf("a pod whose declared port is not HTTP (the default cluster; legacy %d, ports %v)", s.legacy, s.ports)
							case port != 0 && s.ports[port] != registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
								what = fmt.Sprintf("a pod that does not declare the port as HTTP (port %d, ports %v)", port, s.ports)
							}
							if what == "" {
								continue
							}
							kind := "HTTP ROUTE REACHES " + strings.SplitN(what, " (", 2)[0]
							if found[kind] == 0 {
								show(fmt.Sprintf("%s: table %s vhost %q domains %v -> cluster %s holds %s: %s", kind, tname, vh.GetName(), vh.GetDomains(), cluster, ip, what))
							}
							found[kind]++
						}
					}
				}
			}
		}
	}
}

// A walk in which everything may change between steps: a service leaves the
// registry or comes back, changes its ports and their classes, its Service is
// generated, a user's, or gone. The grace of #1635 is in play, so the tables
// are not compared with a fresh cache's; three things are held in EVERY
// snapshot, settled or not:
//
//   - no domain claimed twice (the proxy rejects the whole table);
//   - no request to a filtered service on a route without the filter;
//   - no HTTP route to a cluster holding a pod that does not declare the
//     cluster's port as HTTP now (ruling 7).
func TestPerPort_WalkWithRemovalsAndReplacements(t *testing.T) {
	seed := int64(r4Env("R4_SEED", 45))
	walks, steps := r4Env("R4_WALKS", 12), r4Env("R4_STEPS", 10)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	found := map[string]int{}
	shown := 0
	show := func(s string) {
		if shown < 30 {
			shown++
			t.Log(s)
		}
	}
	for w := 0; w < walks; w++ {
		st := r4Random(t, rng, nil)
		c := r4Fresh(t, st)
		prev := st
		for step := 0; step < steps; step++ {
			st = r4Random(t, rng, nil)
			ops := r4Ops(c, st)
			order := append(slices.Clone(r4Order), "load")
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			loaded := false
			for _, k := range append(slices.Clone(order), "load") {
				ops[k]()
				loaded = loaded || k == "load"
				require.NoError(t, c.generateSnapshot(ctx))
				where := fmt.Sprintf("seed %d walk %d step %d after %s (order %v)", seed, w, step, k, order)
				r3NoDuplicateDomains(t, where, a043Tables(t, c))
				for _, gap := range loopbackGaps(t, c) {
					if found["LOOPBACK NOT EXHAUSTIVE"] == 0 {
						t.Logf("%s: %s", where, gap)
					}
					found["LOOPBACK NOT EXHAUSTIVE"]++
				}
				if tr := os.Getenv("R4_TRACE"); tr != "" && w == r4Env("R4_TRACE_WALK", 0) {
					var names []string
					for n2, e2 := range c.clusters {
						if e2.service == tr {
							names = append(names, fmt.Sprintf("%s(absent=%v,vhost=%v)", n2, !e2.absentSince.IsZero(), e2.vhost != nil))
						}
					}
					slices.Sort(names)
					var now string
					for _, s := range st.svcs {
						if s.key() == tr {
							now = fmt.Sprintf("mode=%d held=%v ports=%v legacy=%d", s.mode, s.held, s.ports, s.legacy)
						}
					}
					p, has := c.servicePorts[tr]
					t.Logf("TRACE step %d after %-11s state[%s] servicePorts(has=%v)=%+v declared=%+v entries=%v", step, k, now, has, p, c.declaredByService()[tr], names)
				}
				for name, e := range c.clusters {
					if _, ok := c.servicePorts[e.service]; !ok && found["ENTRY WITHOUT PORTS"] == 0 {
						found["ENTRY WITHOUT PORTS"]++
						var b strings.Builder
						for n2, e2 := range c.clusters {
							if e2.service == e.service {
								fmt.Fprintf(&b, "\n      entry %s absent=%v vhost=%v l4Floor=%v", n2, !e2.absentSince.IsZero(), e2.vhost != nil, e2.l4Floor)
							}
						}
						t.Logf("%s: entry %s of %s, which servicePorts does not hold; declared=%v%s\n  PREVIOUS:%s\n  NOW:%s", where, name, e.service, c.declaredByService()[e.service], b.String(), r4Describe(prev), r4Describe(st))
					}
				}
				if !loaded {
					continue // the registry this cache last read is the previous step's
				}
				tell := func(s string) {
					show(where + ": " + s + "\n  PREVIOUS:" + r4Describe(prev) + "\n  NOW:" + r4Describe(st))
				}
				r4NoHTTPToANonHTTPPort(t, c, st, found, tell)
			}
			r4FilterInvariant(t, a043Tables(t, c), st, found, func(s string) {
				var b strings.Builder
				for _, vh := range a043Tables(t, c)[proxy.OutboundHTTPRouteName].GetVirtualHosts() {
					fmt.Fprintf(&b, "\n      out_http vhost %q domains %v filter=%v routes=%d", vh.GetName(), vh.GetDomains(), len(vh.GetTypedPerFilterConfig()) > 0, len(vh.GetRoutes()))
				}
				fmt.Fprintf(&b, "\n    servicePorts:")
				for svc, p := range c.servicePorts {
					fmt.Fprintf(&b, "\n      %s http=%v tcp=%v udp=%v legacy=%d reservedOnly=%v", svc, p.http, p.tcp, p.udp, p.legacyPort, p.reservedOnly)
				}
				fmt.Fprintf(&b, "\n    declared: %v\n    entries:", c.declaredByService())
				for name, e := range c.clusters {
					fmt.Fprintf(&b, "\n      %s service=%s absentSince=%v vhost=%v", name, e.service, !e.absentSince.IsZero(), e.vhost != nil)
				}
				show(fmt.Sprintf("seed %d walk %d step %d settled (order %v): %s\n  PREVIOUS:%s\n  NOW:%s\n  TABLE:%s", seed, w, step, order, s[:min(len(s), 330)], r4Describe(prev), r4Describe(st), b.String()))
			})
			prev = st
		}
	}
	for _, k := range slices.Sorted(maps.Keys(found)) {
		if strings.HasPrefix(k, "PASSTHROUGH") {
			continue
		}
		t.Errorf("%s: %d times", k, found[k])
	}
}
