package cache

// Fifth adversarial pass on the proposal-043 stack (head 3990f748). Tests
// adopted from the review.

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
)

// r5Svc is one service of a mesh state. Unlike the fourth pass's r4Svc, what
// its generated Service DECLARES (decl) is drawn apart from what its rows say
// (ports): the registrar derives one from the other on a ticker, and the node
// reads the two on different watches.
type r5Svc struct {
	a043Service
	// mode: 0 generated Service, 2 a user's Service owns the name (port map),
	// 3 no Service at all.
	mode    int
	decl    map[uint32]registryv1.PortProtocol // mode 0: nil = name only
	portMap map[uint32]uint32
	held    bool
	filter  bool
	every   bool   // a port-less local rule, marked
	portRul uint32 // a rule for one parent port, 0 none
	ext     bool   // a rule with an ExtensionRef filter on /x
	imp     bool   // an imported rule
	backend string // key of the service a /b rule names, "" none
	bport   uint32
}

type r5State struct{ svcs map[string]*r5Svc }

func (st r5State) clone() r5State {
	out := r5State{svcs: map[string]*r5Svc{}}
	for k, s := range st.svcs {
		c := *s
		c.ports = maps.Clone(s.ports)
		c.decl = maps.Clone(s.decl)
		c.portMap = maps.Clone(s.portMap)
		out.svcs[k] = &c
	}
	return out
}

var (
	r5Names = [][2]string{{"demo", "echo"}, {"demo", "solo"}, {"other", "echo"}}
	r5H     = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	r5T     = registryv1.PortProtocol_PORT_PROTOCOL_TCP
)

func r5Ports(rng *rand.Rand) map[uint32]registryv1.PortProtocol {
	out := map[uint32]registryv1.PortProtocol{}
	for _, p := range []uint32{80, 8080, 9090} {
		switch rng.Intn(5) {
		case 0, 1:
			out[p] = r5H
		case 2:
			if p != 80 {
				out[p] = r5T
			}
		}
	}
	if rng.Intn(3) == 0 {
		out[9000] = r5T
	}
	if len(out) == 0 {
		out[8080] = r5H
	}
	return out
}

func r5PortMap(rng *rand.Rand) map[uint32]uint32 {
	out := map[uint32]uint32{}
	for _, sp := range []uint32{80, 8000, 8080, 9000} {
		if rng.Intn(2) == 0 {
			// 0: a targetPort that is a name the Service's EndpointSlices do
			// not resolve. The port is refused until it does.
			out[sp] = []uint32{8080, 9090, 80, 9000, 7000, 0}[rng.Intn(6)]
		}
	}
	if len(out) == 0 {
		out[8000] = 8080
	}
	return out
}

func r5Legacy(rng *rand.Rand, ports map[uint32]registryv1.PortProtocol) uint32 {
	nums := slices.Sorted(maps.Keys(ports))
	return nums[rng.Intn(len(nums))]
}

func r5NewSvc(rng *rand.Rand, i int, ns, name string) *r5Svc {
	s := &r5Svc{}
	s.a043Service = a043Service{name: name, ns: ns, ip: fmt.Sprintf("10.0.8.%d", i+1), vip: fmt.Sprintf("10.96.8.%d", i+1)}
	s.ports = r5Ports(rng)
	s.legacy = r5Legacy(rng, s.ports)
	s.mode = []int{0, 0, 2, 3}[rng.Intn(4)]
	s.held = rng.Intn(2) == 0
	switch rng.Intn(4) {
	case 0:
		s.decl = nil
	case 1:
		s.decl = r5Ports(rng)
	default:
		s.decl = maps.Clone(s.ports)
	}
	s.portMap = r5PortMap(rng)
	s.filter = rng.Intn(2) == 0
	s.every = rng.Intn(2) == 0
	return s
}

func r5Random(rng *rand.Rand) r5State {
	st := r5State{svcs: map[string]*r5Svc{}}
	for i, n := range r5Names {
		if rng.Intn(5) == 0 {
			continue
		}
		s := r5NewSvc(rng, i, n[0], n[1])
		st.svcs[s.key()] = s
	}
	return st
}

// r5Mutate changes a few things of a state, as the world does between two
// reconciles, and says what.
func r5Mutate(rng *rand.Rand, st r5State) (r5State, []string) {
	out := st.clone()
	var what []string
	for n := 1 + rng.Intn(3); n > 0; n-- {
		i := rng.Intn(len(r5Names))
		key := r5Names[i][0] + "/" + r5Names[i][1]
		s, ok := out.svcs[key]
		if !ok {
			s = r5NewSvc(rng, i, r5Names[i][0], r5Names[i][1])
			out.svcs[key] = s
			what = append(what, key+": appears")
			continue
		}
		switch rng.Intn(14) {
		case 0, 1:
			s.held = !s.held
			what = append(what, fmt.Sprintf("%s: held=%v", key, s.held))
		case 2:
			s.ports = r5Ports(rng)
			s.legacy = r5Legacy(rng, s.ports)
			what = append(what, fmt.Sprintf("%s: rows now %v legacy %d", key, s.ports, s.legacy))
		case 3:
			s.decl = r5Ports(rng)
			what = append(what, fmt.Sprintf("%s: Service declares %v", key, s.decl))
		case 4:
			s.decl = maps.Clone(s.ports)
			what = append(what, fmt.Sprintf("%s: Service declares the rows' ports %v", key, s.decl))
		case 5:
			s.decl = nil
			what = append(what, key+": Service declares no port")
		case 6:
			s.mode = []int{0, 2, 3}[rng.Intn(3)]
			what = append(what, fmt.Sprintf("%s: mode=%d", key, s.mode))
		case 7:
			s.portMap = r5PortMap(rng)
			what = append(what, fmt.Sprintf("%s: port map %v", key, s.portMap))
		case 8:
			s.filter = !s.filter
			what = append(what, fmt.Sprintf("%s: filter=%v", key, s.filter))
		case 9:
			s.every = !s.every
			what = append(what, fmt.Sprintf("%s: every-rule=%v", key, s.every))
		case 10:
			s.portRul = []uint32{0, 80, 8080, 9090, 8000}[rng.Intn(5)]
			what = append(what, fmt.Sprintf("%s: port rule %d", key, s.portRul))
		case 11:
			s.ext = !s.ext
			s.imp = rng.Intn(3) == 0
			what = append(what, fmt.Sprintf("%s: ext=%v imported=%v", key, s.ext, s.imp))
		case 12:
			to := r5Names[rng.Intn(len(r5Names))]
			s.backend, s.bport = to[0]+"/"+to[1], []uint32{80, 8080, 9090, 8000, 9000}[rng.Intn(5)]
			if rng.Intn(2) == 0 {
				s.backend = ""
			}
			what = append(what, fmt.Sprintf("%s: backend %q:%d", key, s.backend, s.bport))
		case 13:
			delete(out.svcs, key)
			what = append(what, key+": gone entirely")
		}
	}
	return out, what
}

func (st r5State) describe() string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(st.svcs)) {
		s := st.svcs[k]
		fmt.Fprintf(&b, "\n    %s mode=%d held=%v rows=%v legacy=%d decl=%v portMap=%v filter=%v every=%v portRule=%d ext=%v imp=%v backend=%q:%d",
			k, s.mode, s.held, s.ports, s.legacy, s.decl, s.portMap, s.filter, s.every, s.portRul, s.ext, s.imp, s.backend, s.bport)
	}
	return b.String()
}

// r5Pushes returns what each reconciler pushes for st, one closure per input,
// each taking the cache it is pushed to.
func r5Pushes(t *testing.T, st r5State) map[string]func(*SnapshotCache) {
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	portMaps := map[string]map[uint32]uint32{}
	targets := map[string][]uint32{}
	routes := map[string][]proxy.GammaRoute{}
	imported := map[string][]proxy.GammaRoute{}
	filters := map[string]proxy.ExtensionFilter{}
	var listed []a043Service
	for _, k := range slices.Sorted(maps.Keys(st.svcs)) {
		s := st.svcs[k]
		switch s.mode {
		case 0:
			authorities[k] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
			e := capture.CaptureTCPService{ServiceName: k, ClusterIP: s.vip}
			for port, class := range s.decl {
				if class == r5H {
					e.DeclaredHTTP = append(e.DeclaredHTTP, port)
				} else {
					e.DeclaredOther = append(e.DeclaredOther, port)
				}
			}
			slices.Sort(e.DeclaredHTTP)
			slices.Sort(e.DeclaredOther)
			vips = append(vips, e)
		case 2:
			portMaps[k] = s.portMap
		}
		if s.held {
			listed = append(listed, s.a043Service)
		}
		if s.filter {
			filters[k] = r3ChainCfg(t)
		}
		if s.every {
			routes[k] = append(routes[k], proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("every:" + k)})
		}
		if s.portRul != 0 {
			routes[k] = append(routes[k], proxy.GammaRoute{ParentPort: s.portRul, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark(fmt.Sprintf("port%d:%s", s.portRul, k))})
			targets[k] = []uint32{s.portRul}
		}
		if s.ext {
			routes[k] = append(routes[k], proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/x"}}, ExtensionFilters: []proxy.ExtensionFilter{r3ChainCfg(t)}})
		}
		if s.imp {
			imported[k] = append(imported[k], proxy.GammaRoute{Imported: true, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("imported:" + k)})
		}
		if to, ok := st.svcs[s.backend]; ok && s.backend != "" {
			routes[k] = append(routes[k], proxy.GammaRoute{
				Matches:  []proxy.GammaMatch{{Prefix: "/b"}},
				Backends: []proxy.GammaBackend{{Service: to.key(), Cluster: to.name + "." + to.ns + "." + a043Mesh, Port: s.bport, Weight: 1}},
			})
		}
	}
	return map[string]func(*SnapshotCache){
		"authorities": func(c *SnapshotCache) { c.SetCaptureAuthorities(authorities) },
		"services":    func(c *SnapshotCache) { c.SetCaptureTCPServices(vips) },
		"portmaps":    func(c *SnapshotCache) { c.SetServicePortMaps(portMaps) },
		"routes":      func(c *SnapshotCache) { c.SetServiceRoutes(routes) },
		"imported":    func(c *SnapshotCache) { c.SetImportedServiceRoutes(imported) },
		"filters":     func(c *SnapshotCache) { c.SetServiceChainFilters(filters) },
		"targets":     func(c *SnapshotCache) { c.SetRouteTargetPorts(targets) },
		"load": func(c *SnapshotCache) {
			// The agent's registry client holds the rows of the services in
			// its watch filter, which the refresher sets to the dependency
			// set right before it reloads (AssertWatchFilter). R5_UNSCOPED=1
			// lists every service instead: a registry without watch scoping,
			// and the client's RPC fallback while its watch is not ready.
			rows := listed
			if !r5Unscoped {
				deps := c.DependencySet()
				rows = nil
				for _, s := range listed {
					if _, ok := deps[s.key()]; ok {
						rows = append(rows, s)
					}
				}
			}
			_ = c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(rows))
		},
	}
}

var r5Order = []string{"authorities", "services", "portmaps", "routes", "imported", "filters", "targets"}

// r5Applied is what a cache has been told so far: per input, the state whose
// push it last received.
type r5Applied map[string]r5State

func r5Fresh(t *testing.T, applied r5Applied) *SnapshotCache {
	t.Helper()
	c := a043Cache(t, nil)
	for _, k := range append(slices.Clone(r5Order), "load") {
		r5Pushes(t, applied[k])[k](c)
	}
	require.NoError(t, c.generateSnapshot(context.Background()))
	return c
}

// r5Answer is what a table does with one request from a caller's namespace.
type r5Answer struct {
	sig       string // everything a client can tell apart
	delivered bool   // routed to a cluster that is not the passthrough
	pass      bool
	filter    bool
	marks     []string
	clusters  []string
	vhost     string
	onDemand  bool
}

func r5Resolve(t *testing.T, table *routev3.RouteConfiguration, ns, host, path string) r5Answer {
	t.Helper()
	vh := r3Pick(t, table, host)
	routes := vh.GetRoutes()
	if vh.GetMatcher() != nil {
		routes = matcherArm(t, vh.GetMatcher(), ns)
	}
	ans := r5Answer{vhost: vh.GetName()}
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
		if !ok || m.GetRuntimeFraction() != nil {
			continue
		}
		if dr := r.GetDirectResponse(); dr != nil {
			reason := ""
			for _, h := range r.GetResponseHeadersToAdd() {
				if h.GetHeader().GetKey() == proxy.NoHTTPPortErrorHeader {
					reason = h.GetHeader().GetValue()
				}
			}
			ans.sig = fmt.Sprintf("direct %d %s", dr.GetStatus(), reason)
			return ans
		}
		ra := r.GetRoute()
		if ra == nil {
			ans.sig = "other action"
			return ans
		}
		if ra.GetCluster() == proxy.PassthroughClusterName {
			ans.pass, ans.sig = true, "passthrough"
			return ans
		}
		ans.delivered = true
		_, onVh := vh.GetTypedPerFilterConfig()[r3Filter]
		_, onRoute := r.GetTypedPerFilterConfig()[r3Filter]
		ans.filter = onVh || onRoute
		for _, h := range r.GetResponseHeadersToAdd() {
			if h.GetHeader().GetKey() == "x-rv" {
				ans.marks = append(ans.marks, h.GetHeader().GetValue())
			}
		}
		if ra.GetClusterHeader() != "" {
			ans.onDemand = true
			ans.clusters = []string{"on-demand(" + host + ")"}
		} else {
			ans.clusters = r4RouteClusters(r)
			sort.Strings(ans.clusters)
		}
		ans.sig = fmt.Sprintf("clusters=%v filter=%v marks=%v", ans.clusters, ans.filter, ans.marks)
		return ans
	}
	ans.sig = "no route matches"
	return ans
}

var (
	r5Suffixes = []string{"", ":80", ":8000", ":8080", ":9090", ":9000", ":7777", ":18081", ":18082"}
	r5Callers  = []string{"demo", "other", "third"}
)

func r5Forms(ns, name string) []string {
	return []string{name, name + "." + ns + "." + a043Mesh, name + "." + ns, name + "." + ns + ".svc", name + "." + ns + ".svc.cluster.local"}
}

func r5HTTPTable(name string) bool {
	return strings.HasPrefix(name, "cap_http") || name == proxy.OutboundHTTPRouteName
}

// r5Invariants: what must hold in EVERY snapshot, whatever was pushed in what
// order. rules is the state whose filters and routes the cache was last told
// (they come from two pushes, so two states).
func r5Invariants(t *testing.T, tables map[string]*routev3.RouteConfiguration, filters, routes, imported r5State, found map[string]int, show func(kind, detail string)) {
	t.Helper()
	for _, n := range r5Names {
		ns, name := n[0], n[1]
		key := ns + "/" + name
		fs, hasF := filters.svcs[key]
		filtered := hasF && fs.filter
		rs, hasR := routes.svcs[key]
		every := hasR && rs.every
		for tname, table := range tables {
			if !r5HTTPTable(tname) {
				continue
			}
			where := "capture"
			if tname == proxy.OutboundHTTPRouteName || tname == "cap_http_18081" {
				where = "loopback"
			}
			for fi, form := range r5Forms(ns, name) {
				for _, suffix := range r5Suffixes {
					host := form + suffix
					callers := []string{ns}
					if fi == 0 {
						callers = r5Callers
					}
					for _, caller := range callers {
						for _, path := range []string{"/", "/x", "/b"} {
							a := r5Resolve(t, table, caller, host, path)
							if !a.delivered {
								continue
							}
							// Ruling 5: a short name is the caller's namespace's.
							if fi == 0 && path == "/" {
								for _, cl := range a.clusters {
									want := name + "." + caller + "." + a043Mesh
									bare := strings.TrimPrefix(strings.TrimPrefix(cl, "quic:"), "on-demand(")
									if !strings.HasPrefix(bare, want) {
										show(fmt.Sprintf("SHORT NAME CROSSES A NAMESPACE table=%s", where),
											fmt.Sprintf("table %s caller %s Host %s -> %v (vhost %q)", tname, caller, host, a.clusters, a.vhost))
									}
								}
							}
							if fi == 0 && caller != ns {
								continue // another namespace's service of the name, judged in its own turn
							}
							if filtered && !a.filter {
								show(fmt.Sprintf("DELIVERED WITHOUT THE FILTER form=%d table=%s", fi, where),
									fmt.Sprintf("table %s caller %s Host %s path %s -> %s (vhost %q)", tname, caller, host, path, a.sig, a.vhost))
							}
							if every && path == "/" && !slices.Contains(a.marks, "every:"+key) {
								show(fmt.Sprintf("DELIVERED WITHOUT THE RULE form=%d table=%s", fi, where),
									fmt.Sprintf("table %s caller %s Host %s path %s -> %s (vhost %q)", tname, caller, host, path, a.sig, a.vhost))
							}
							// Ruling 4: the bare cluster is never asked for by
							// what the client typed.
							if a.onDemand && suffix == "" {
								show(fmt.Sprintf("ON DEMAND BY A PORT-LESS AUTHORITY form=%d table=%s", fi, where),
									fmt.Sprintf("table %s caller %s Host %s -> %s (vhost %q)", tname, caller, host, a.sig, a.vhost))
							}
						}
					}
				}
			}
		}
	}
	_ = imported
	_ = found
}

// r5Retained returns the services the cache holds a retained entry of.
func r5Retained(c *SnapshotCache) map[string]struct{} {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	out := map[string]struct{}{}
	for _, e := range c.clusters {
		if !e.absentSince.IsZero() {
			out[e.service] = struct{}{}
		}
	}
	return out
}

func r5Dump(c *SnapshotCache) string {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	var b strings.Builder
	b.WriteString("\n    servicePorts:")
	for _, svc := range slices.Sorted(maps.Keys(c.servicePorts)) {
		p := c.servicePorts[svc]
		fmt.Fprintf(&b, "\n      %s http=%v tcp=%v udp=%v legacy=%d", svc, p.http, p.tcp, p.udp, p.legacyPort)
	}
	b.WriteString("\n    entries:")
	for _, name := range slices.Sorted(maps.Keys(c.clusters)) {
		e := c.clusters[name]
		fmt.Fprintf(&b, "\n      %s service=%s retained=%v vhost=%v floor=%v", name, e.service, !e.absentSince.IsZero(), e.vhost.GetDomains(), e.l4Floor)
	}
	return b.String()
}

// TestReview5_Walk: states change a little at a time (and sometimes a lot),
// the pushes and the registry refresh land in a random order, a snapshot is
// built after each, the grace sometimes runs out. Held in every snapshot:
// r5Invariants and no duplicate domain. Held after every refresh, for every
// service the cache keeps no retained entry of: the answers are the ones of a
// cache that starts with exactly what this one has been told.
// r5Unscoped: the registry of the walk lists EVERY service, not only the
// node's dependencies (a registry without watch scoping, and the registry
// client's RPC fallback while its watch is not ready).
var r5Unscoped bool

// TestPerPort_WalkOverEveryInput: a walk in which every input of the cache
// changes between steps, in every order, with the grace in play, on a registry
// that lists the node's dependencies. After every push: no domain claimed
// twice, no filtered service on a route without its filter, no HTTP to a pod
// that does not declare the port as HTTP, every service the node claims has a
// virtual host of its own on the loopback table; and, settled, the answers of
// a cache that starts in that state wherever no entry is retained.
func TestPerPort_WalkOverEveryInput(t *testing.T) { r5Walk(t, false) }

// TestPerPort_WalkOverEveryInput_UnscopedListing is the same on a registry
// that lists every service of the mesh.
func TestPerPort_WalkOverEveryInput_UnscopedListing(t *testing.T) { r5Walk(t, true) }

func r5Walk(t *testing.T, unscoped bool) {
	prev := r5Unscoped
	r5Unscoped = unscoped
	t.Cleanup(func() { r5Unscoped = prev })
	seed := int64(r4Env("R5_SEED", 1))
	walks, steps := r4Env("R5_WALKS", 6), r4Env("R5_STEPS", 14)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	found := map[string]int{}
	shown := 0
	var history []string
	show := func(kind, detail string) {
		if found[kind] == 0 && shown < r4Env("R5_SHOW", 12) {
			shown++
			t.Logf("%s: %s\n  HISTORY (latest last):\n  %s", kind, detail, strings.Join(history[max(0, len(history)-r4Env("R5_HIST", 6)):], "\n  "))
		}
		found[kind]++
	}
	for w := 0; w < walks; w++ {
		st := r5Random(rng)
		applied := r5Applied{}
		for _, k := range append(slices.Clone(r5Order), "load") {
			applied[k] = st
		}
		c := r5Fresh(t, applied)
		history = []string{fmt.Sprintf("seed %d walk %d START:%s", seed, w, st.describe())}
		for step := 0; step < steps; step++ {
			var what []string
			if rng.Intn(6) == 0 {
				st, what = r5Random(rng), []string{"a new state altogether"}
			} else {
				st, what = r5Mutate(rng, st)
			}
			pushes := r5Pushes(t, st)
			order := append(slices.Clone(r5Order), "load")
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			order = append(order, "load")
			expire := rng.Intn(5) == 0
			history = append(history, fmt.Sprintf("step %d: %s; order %v expire=%v:%s", step, strings.Join(what, "; "), order, expire, st.describe()))
			for i, k := range order {
				if expire && i == len(order)-1 {
					c.clusterMu.Lock()
					for name, e := range c.clusters {
						if !e.absentSince.IsZero() {
							e.absentSince = time.Now().Add(-2 * c.retentionGrace())
							c.clusters[name] = e
						}
					}
					c.clusterMu.Unlock()
				}
				pushes[k](c)
				applied[k] = st
				require.NoError(t, c.generateSnapshot(ctx))
				tables := a043Tables(t, c)
				where := fmt.Sprintf("seed %d walk %d step %d after push #%d %s", seed, w, step, i, k)
				r3NoDuplicateDomains(t, where, tables)
				for _, gap := range loopbackGaps(t, c) {
					show("LOOPBACK NOT EXHAUSTIVE", where+": "+gap+r5Dump(c))
				}
				r5Invariants(t, tables, applied["filters"], applied["routes"], applied["imported"], found, func(kind, detail string) {
					show(kind, where+": "+detail+r5Dump(c))
				})
				// Ruling 7, in every snapshot and not only the ones built
				// right after a refresh: judged by the rows the cache last
				// read.
				{
					last := r4State{}
					for _, s := range applied["load"].svcs {
						last.svcs = append(last.svcs, r4Svc{a043Service: s.a043Service, mode: s.mode, portMap: s.portMap, held: s.held})
					}
					r7 := map[string]int{}
					r4NoHTTPToANonHTTPPort(t, c, last, r7, func(detail string) {
						show("RULING 7: "+strings.SplitN(detail, ":", 2)[0], where+": "+detail+r5Dump(c))
					})
				}
				if k != "load" {
					continue
				}
				// Settled with respect to what it has been told: compare with
				// a cache that starts there.
				retained := r5Retained(c)
				fresh := r5Fresh(t, applied)
				want := a043Tables(t, fresh)
				for tname, wt := range want {
					if !r5HTTPTable(tname) {
						continue
					}
					gt, ok := tables[tname]
					if !ok {
						if len(retained) == 0 {
							show("TABLE ONLY ON A FRESH NODE", where+": "+tname)
						}
						continue
					}
					for _, n := range r5Names {
						key := n[0] + "/" + n[1]
						if _, kept := retained[key]; kept {
							continue
						}
						for fi, form := range r5Forms(n[0], n[1]) {
							for _, suffix := range r5Suffixes {
								host := form + suffix
								callers := []string{n[0]}
								if fi == 0 {
									callers = r5Callers
								}
								for _, caller := range callers {
									if fi == 0 {
										// A short name's virtual host is shared by
										// every namespace's service of the name,
										// and which suffixes it has depends on all
										// of them: a retained one anywhere makes
										// the comparison one between two different
										// states.
										skip := false
										for _, ns := range r5Callers {
											if _, kept := retained[ns+"/"+n[1]]; kept {
												skip = true
											}
										}
										if skip {
											continue
										}
									}
									a, b := r5Resolve(t, gt, caller, host, "/"), r5Resolve(t, wt, caller, host, "/")
									if a.sig != b.sig {
										tk := tname
										if strings.HasPrefix(tname, "cap_http_") && tname != "cap_http_18081" && tname != "cap_http_other" {
											tk = "cap_http_<real>"
										}
										show(fmt.Sprintf("INCREMENTAL != FRESH form=%d table=%s", fi, tk),
											fmt.Sprintf("%s: table %s caller %s Host %s\n    incremental: %s (vhost %q)\n    fresh:       %s (vhost %q)%s\n  FRESH:%s",
												where, tname, caller, host, a.sig, a.vhost, b.sig, b.vhost, r5Dump(c), r5Dump(fresh)))
									}
								}
							}
						}
					}
				}
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(found)) {
		t.Errorf("%s: %d times", k, found[k])
	}
}
