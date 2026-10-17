package cache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/xdseval"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// The mesh's HTTP proxy port answers a service's Kubernetes names exactly as
// its mesh name (proposal 043, "The HTTP proxy port"): on the loopback table
// (out_http) and on a captured dial to 18081 (the proxy port's block of
// cap_http), which are built from the same virtual hosts. Through chart 3.0.2
// "<svc>.<ns>.svc.cluster.local:8080" on 18081 got the floor (the passthrough
// on the captured path, 404 on the loopback) while the mesh name was routed.

// pkTables are the two tables that answer the proxy port, each with the
// request filter state that selects it.
func pkTables(t *testing.T, c *SnapshotCache) map[string]struct {
	tbl *xdseval.Table
	fs  map[string]string
} {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	rs := snap.GetResources(resourcev3.RouteType)
	out := map[string]struct {
		tbl *xdseval.Table
		fs  map[string]string
	}{}
	loop, ok := rs[proxy.OutboundHTTPRouteName].(*routev3.RouteConfiguration)
	require.True(t, ok, "no out_http")
	out["out_http"] = struct {
		tbl *xdseval.Table
		fs  map[string]string
	}{xdseval.NewTable(loop), map[string]string{}}
	capt, ok := rs[proxy.CaptureHTTPChainName].(*routev3.RouteConfiguration)
	require.True(t, ok, "no cap_http")
	out["cap_http dialled 18081"] = struct {
		tbl *xdseval.Table
		fs  map[string]string
	}{xdseval.NewTable(capt), map[string]string{proxy.DialledPortFilterStateKey: strconv.Itoa(meshconst.ProxyOutboundPort)}}
	return out
}

// pkFirst is the first route a request takes in tbl, with its virtual
// host's per-filter config (nil route: none).
func pkFirst(t *testing.T, tbl *xdseval.Table, req xdseval.Request) (*routev3.Route, map[string]*anypb.Any) {
	t.Helper()
	a, err := tbl.Resolve(req)
	require.NoError(t, err)
	var routes []*routev3.Route
	if !a.NoVhost {
		routes = a.Routes
	}
	r, err := xdseval.FirstRoute(routes, req)
	require.NoError(t, err)
	return r, a.VhostConfig
}

// pkOnDemand: the request is answered by no virtual host of its service's
// own but the floor's (the catch-all), or by a route that asks for the
// cluster its authority spells: the service's ports are not known.
func pkOnDemand(t *testing.T, tbl *xdseval.Table, req xdseval.Request) bool {
	t.Helper()
	a, err := tbl.Resolve(req)
	require.NoError(t, err)
	if a.NoVhost || a.Vhost == "on_demand_catch_all" {
		return true
	}
	r, _ := pkFirst(t, tbl, req)
	return xdseval.OutcomeOf(r).ClusterHeader != ""
}

// pkCluster is the cluster route r sends to: its own, or the h2 cluster an
// east-west HTTP/3 selection falls back to ("" for none).
func pkCluster(t *testing.T, r *routev3.Route) string {
	t.Helper()
	ra := r.GetRoute()
	switch {
	case ra == nil, ra.GetClusterHeader() != "", len(ra.GetWeightedClusters().GetClusters()) > 0:
		return ""
	case ra.GetCluster() != "":
		return ra.GetCluster()
	}
	return quicSelectionDefault(t, ra)
}

// pkSameAs reports how Host a and Host b are answered differently in tbl,
// "" when they are not.
func pkSameAs(t *testing.T, tbl *xdseval.Table, fs map[string]string, a, b, path string) string {
	t.Helper()
	ra, ca := pkFirst(t, tbl, xdseval.Request{Host: a, Path: path, FilterState: fs})
	rb, cb := pkFirst(t, tbl, xdseval.Request{Host: b, Path: path, FilterState: fs})
	if same, why := xdseval.SameTreatmentIn(ca, ra, cb, rb); !same {
		return why
	}
	return ""
}

// pkKubernetesNames are the Kubernetes names of svc in ns, one of them in
// mixed case.
func pkKubernetesNames(svc, ns string) []string {
	fqdn := svc + "." + ns + ".svc.cluster.local"
	return []string{fqdn, svc + "." + ns + ".svc", svc + "." + ns, strings.ToUpper(svc[:1]) + svc[1:] + "." + strings.ToUpper(ns) + ".SVC.Cluster.Local"}
}

// TestProxyPort_KubernetesNamesAnswerAsTheMeshName: for every service of the
// fixture (a generated mesh Service each, with rules and a service-wide
// filter on some), every Kubernetes name with every port suffix gets on the
// proxy port what the mesh name with that suffix gets, on both tables, for
// every path a rule tells apart. The answers are held to include a delivery
// and a refusal, so the check cannot pass on a fixture that never routes.
func TestProxyPort_KubernetesNamesAnswerAsTheMeshName(t *testing.T) {
	for name, c := range oneHCMFixtures(t) {
		t.Run(name, func(t *testing.T) {
			delivered, refused, onDemand := 0, 0, 0
			tables := pkTables(t, c)
			loopback := tables["out_http"].tbl
			for tname, tb := range tables {
				for _, s := range a043Fixture() {
					mesh := s.name + "." + s.ns + "." + a043Mesh
					// A service the node holds no endpoint of and knows no
					// ports of is parked for the cluster its mesh name spells
					// (an undeclared port is asked for on demand); a Kubernetes
					// name spells no cluster, and is left to the floor until
					// the ports are known. Judged on the loopback table, whose
					// virtual hosts the proxy port's block is compiled from.
					cold := pkOnDemand(t, loopback, xdseval.Request{Host: mesh + ":7777", Path: "/"})
					for _, suffix := range []string{"", ":80", ":8080", ":9090", ":9000", ":7070", ":18081", ":18082", ":7777"} {
						for _, path := range []string{"/", "/x"} {
							like := mesh + suffix
							if cold {
								like = "example.com" + suffix
								onDemand++
							}
							for _, k := range pkKubernetesNames(s.name, s.ns) {
								assert.Empty(t, pkSameAs(t, tb.tbl, tb.fs, like, k+suffix, path), "%s: Host %s path %s answered unlike %s", tname, k+suffix, path, like)
							}
							r, _ := pkFirst(t, tb.tbl, xdseval.Request{Host: s.name + "." + s.ns + ".svc.cluster.local" + suffix, Path: path, FilterState: tb.fs})
							switch o := xdseval.OutcomeOf(r); {
							case o.Reason != "":
								refused++
							case pkCluster(t, r) != "" && pkCluster(t, r) != proxy.PassthroughClusterName, len(o.Weighted) > 0:
								delivered++
							}
						}
					}
				}
			}
			if name != "no endpoint held, names only" {
				assert.Positive(t, refused, "a Kubernetes name on a port its service does not declare is refused")
				assert.Positive(t, delivered, "a Kubernetes name on a declared port is delivered")
				assert.Zero(t, onDemand, "every service's ports are known")
			}
		})
	}
}

// TestProxyPort_KubernetesNameDeliversItsPort pins the reported case: Host
// "<svc>.<ns>.svc.cluster.local:8080" on the proxy port reaches the service's
// port 8080, as the mesh name does, and ":9090" its port 9090.
func TestProxyPort_KubernetesNameDeliversItsPort(t *testing.T) {
	c := a043Cache(t, a043Fixture())
	require.NoError(t, c.generateSnapshot(context.Background()))
	for tname, tb := range pkTables(t, c) {
		for _, p := range []string{"8080", "9090"} {
			for _, host := range []string{"multi.demo.svc.cluster.local:" + p, "MULTI.demo.svc:" + p, "multi.demo:" + p} {
				r, _ := pkFirst(t, tb.tbl, xdseval.Request{Host: host, Path: "/", FilterState: tb.fs})
				require.NotNil(t, r, "%s: Host %s", tname, host)
				want := "multi.demo." + a043Mesh + ":" + p
				if p == "8080" {
					want = "multi.demo." + a043Mesh // the port the pods declare: the service's default cluster
				}
				assert.Equal(t, want, pkCluster(t, r), "%s: Host %s", tname, host)
			}
		}
		// The bare Kubernetes name is the bare mesh name: port 80, which
		// "multi" does not declare.
		r, _ := pkFirst(t, tb.tbl, xdseval.Request{Host: "multi.demo.svc.cluster.local", Path: "/", FilterState: tb.fs})
		require.NotNil(t, r)
		assert.Equal(t, uint32(421), xdseval.OutcomeOf(r).Status, "%s: the bare Kubernetes name is refused as the bare mesh name is", tname)
	}
}

// TestProxyPort_UserServiceAndMeshOnlyNames: a mesh service behind a user's
// Service. When a route names it, the agent has the Service's port map and
// its Kubernetes names are the mesh's: the port in Host is the SERVICE's
// port, translated through the map, exactly as on a dial ("own...:8000"
// reaches pod port 8080, "own...:8080" is refused, as the mesh name). When
// only a service-wide filter names it (meshOnly), its Kubernetes names stay
// the user's Service's: unanswered on the proxy port, as on a dial (the floor:
// a Host that is no mesh name gets the same).
func TestProxyPort_UserServiceAndMeshOnlyNames(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	for _, routed := range []bool{true, false} {
		for _, mapped := range []bool{true, false} {
			if routed && !mapped {
				continue // a route's parent Service always has its port map
			}
			t.Run(fmt.Sprintf("routed=%v mapped=%v", routed, mapped), func(t *testing.T) {
				c := a043Cache(t, []a043Service{multi})
				if mapped {
					c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080}})
				}
				c.SetStaticDependencies([]string{"demo/own", "demo/multi"})
				if routed {
					c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/own": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("local")}}})
				} else {
					c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
				}
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{own, multi})))
				require.NoError(t, c.generateSnapshot(ctx))
				for tname, tb := range pkTables(t, c) {
					for _, suffix := range []string{"", ":80", ":8000", ":8080", ":9090", ":18081"} {
						for _, k := range pkKubernetesNames("own", "demo") {
							if routed {
								assert.Empty(t, pkSameAs(t, tb.tbl, tb.fs, mesh+suffix, k+suffix, "/"), "%s: Host %s answered unlike the mesh name", tname, k+suffix)
							} else {
								assert.Empty(t, pkSameAs(t, tb.tbl, tb.fs, "example.com"+suffix, k+suffix, "/"), "%s: meshOnly Host %s is not the floor", tname, k+suffix)
							}
						}
					}
					if routed {
						r, _ := pkFirst(t, tb.tbl, xdseval.Request{Host: "own.demo.svc.cluster.local:8000", Path: "/", FilterState: tb.fs})
						require.NotNil(t, r)
						assert.Equal(t, "own.demo."+a043Mesh, pkCluster(t, r), "%s: the Service's port 8000 reaches pod port 8080 (the default cluster)", tname)
						r, _ = pkFirst(t, tb.tbl, xdseval.Request{Host: "own.demo.svc.cluster.local:8080", Path: "/", FilterState: tb.fs})
						require.NotNil(t, r)
						assert.Equal(t, uint32(421), xdseval.OutcomeOf(r).Status, "%s: 8080 is no port of the Service", tname)
					}
				}
			})
		}
	}
}

// TestProxyPort_PublishedTablesClaimEachDomainOnce: the proxy rejects a
// route table in which two virtual hosts claim one domain, or one claims it
// twice, and domains are compared case-insensitively. Every route table the
// agent publishes, for every fixture, claims each domain once, and no builder
// had a claim dropped to get there (oneClaimPerDomain keeps the table valid
// by dropping the later claim, which loses that virtual host's answer).
func TestProxyPort_PublishedTablesClaimEachDomainOnce(t *testing.T) {
	check := func(t *testing.T, c *SnapshotCache) {
		var dropped []string
		proxy.OnDuplicateDomain(func(table, domain, kept, lost string) {
			dropped = append(dropped, table+" "+domain+" kept by "+kept+", dropped from "+lost)
		})
		t.Cleanup(func() { proxy.OnDuplicateDomain(func(string, string, string, string) {}) })
		require.NoError(t, c.generateSnapshot(context.Background()))
		assert.Empty(t, dropped, "a builder claimed a domain another virtual host has")
		snap, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		for name, res := range snap.GetResources(resourcev3.RouteType) {
			rc, ok := res.(*routev3.RouteConfiguration)
			require.True(t, ok)
			seen := map[string]string{}
			for _, vh := range rc.GetVirtualHosts() {
				for _, d := range vh.GetDomains() {
					d = strings.ToLower(d)
					prev, dup := seen[d]
					assert.False(t, dup, "table %s: domain %s in %s and %s", name, d, prev, vh.GetName())
					seen[d] = vh.GetName()
				}
			}
		}
	}
	for name, c := range oneHCMFixtures(t) {
		t.Run(name, func(t *testing.T) { check(t, c) })
	}
	t.Run("route target ports", func(t *testing.T) {
		c := a043Cache(t, a043Fixture())
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("9090")}}})
		c.SetRouteTargetPorts(map[string][]uint32{"demo/multi": {8080, 9090}})
		require.NoError(t, c.generateSnapshot(context.Background()))
		check(t, c)
	})
}

// pkKubeNeverNamesClusterByAuthority: no Kubernetes name of svc reaches a
// route that asks for the cluster its own authority spells (no such cluster
// exists; the request would be parked until the on-demand timeout).
func pkKubeNeverNamesClusterByAuthority(t *testing.T, c *SnapshotCache, svc, ns, label string) {
	t.Helper()
	for tname, tb := range pkTables(t, c) {
		for _, k := range pkKubernetesNames(svc, ns) {
			for _, suffix := range []string{"", ":80", ":8080", ":9090", ":7777"} {
				r, _ := pkFirst(t, tb.tbl, xdseval.Request{Host: k + suffix, Path: "/", FilterState: tb.fs})
				if r == nil {
					continue
				}
				assert.Empty(t, xdseval.OutcomeOf(r).ClusterHeader, "%s %s: Host %s names its cluster by authority", label, tname, k+suffix)
			}
		}
	}
}

// TestProxyPort_ColdRoutedServiceThenWarm: a service with a GAMMA rule (local,
// with a service-wide filter; or imported), no generated Service, no port map
// and no endpoint on the node: its loopback virtual hosts ask for the cluster
// the authority spells. Its Kubernetes names are never parked for a cluster
// named after themselves (the floor instead); once the node holds an endpoint
// they answer as the mesh name, on out_http and on the proxy port's block.
// (Adopted from the adversarial review of the change: it fails with the cold
// guard of withKubernetesNames removed, and with kubernetesNames ignoring the
// routed services.)
func TestProxyPort_ColdRoutedServiceThenWarm(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	ghost := a043Service{name: "ghost", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.9"}
	for _, imported := range []bool{false, true} {
		label := "cold"
		if imported {
			label = "cold imported"
		}
		c := a043Cache(t, a043Fixture())
		c.SetStaticDependencies([]string{"demo/ghost"})
		rules := map[string][]proxy.GammaRoute{"demo/ghost": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("ghost")}}}
		if imported {
			c.SetImportedServiceRoutes(rules)
		} else {
			c.SetServiceRoutes(rules)
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/ghost": r3ChainCfg(t)})
		}
		require.NoError(t, c.generateSnapshot(ctx))
		loop := pkTables(t, c)["out_http"].tbl
		require.True(t, pkOnDemand(t, loop, xdseval.Request{Host: "ghost.demo." + a043Mesh + ":8080", Path: "/"}), "%s: the fixture is not cold", label)
		pkKubeNeverNamesClusterByAuthority(t, c, "ghost", "demo", label)

		// Warm: the node now holds an endpoint of ghost.
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(append(a043Fixture(), ghost))))
		require.NoError(t, c.generateSnapshot(ctx))
		for tname, tb := range pkTables(t, c) {
			for _, suffix := range []string{"", ":8080", ":9090", ":7777"} {
				for _, k := range pkKubernetesNames("ghost", "demo") {
					assert.Empty(t, pkSameAs(t, tb.tbl, tb.fs, "ghost.demo."+a043Mesh+suffix, k+suffix, "/"), "%s, warm, %s: Host %s unlike the mesh name", label, tname, k+suffix)
				}
			}
		}
		pkKubeNeverNamesClusterByAuthority(t, c, "ghost", "demo", label+", warm")
	}
}

// TestWithKubernetesNames_KeepsEveryFieldAndWritesNoVirtualHost: a virtual
// host that gains domains is a new one. One that sets only what a loopback
// virtual host sets shares its routes; one that sets anything else (here a
// retry policy) is a full copy, so nothing it sets is lost. Neither given
// virtual host is written to (the cluster entries' are reused across
// snapshots).
func TestWithKubernetesNames_KeepsEveryFieldAndWritesNoVirtualHost(t *testing.T) {
	const mesh = "a.ns." + a043Mesh
	kube := map[string][]string{mesh: {"a.ns.svc.cluster.local", "a.ns"}}
	plain := proxy.BuildOutboundClusterVirtualHost(mesh+":8080", []string{mesh + ":8080"})
	rich := proxy.BuildOutboundClusterVirtualHost(mesh+":9090", []string{mesh + ":9090"})
	rich.RetryPolicy = &routev3.RetryPolicy{RetryOn: "5xx"}
	other := proxy.BuildOutboundClusterVirtualHost("x.y."+a043Mesh+":8080", []string{"x.y." + a043Mesh + ":8080"})
	out := withKubernetesNames([]*routev3.VirtualHost{plain, rich, other}, kube)

	assert.Equal(t, []string{mesh + ":8080", "a.ns.svc.cluster.local:8080", "a.ns:8080"}, out[0].GetDomains())
	assert.NotSame(t, plain, out[0])
	assert.Same(t, plain.GetRoutes()[0], out[0].GetRoutes()[0], "the routes are shared")
	assert.Equal(t, []string{mesh + ":9090", "a.ns.svc.cluster.local:9090", "a.ns:9090"}, out[1].GetDomains())
	assert.Equal(t, "5xx", out[1].GetRetryPolicy().GetRetryOn(), "a field a loopback virtual host does not set is kept")
	assert.Same(t, other, out[2], "a name with no Kubernetes names is left as it is")
	assert.Equal(t, []string{mesh + ":8080"}, plain.GetDomains(), "the given virtual host is not written to")
	assert.Equal(t, []string{mesh + ":9090"}, rich.GetDomains())
}
