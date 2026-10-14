package cache

// Third adversarial pass on the proposal-043 stack (head 7f0f8299). Tests
// adopted from the review.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

const r3Filter = "envoy.filters.http.header_mutation"

func r3ChainCfg(t *testing.T) proxy.ExtensionFilter {
	t.Helper()
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	return proxy.ExtensionFilter{Name: r3Filter, Config: cfg}
}

// r3Sig is everything a client (and the destination's filter) can tell apart
// about how a table answers one request: the virtual host's routes as the
// caller's namespace sees them, in order.
type r3Sig struct {
	first string   // the first route that matches "GET /"
	all   []string // every route of the arm, in order
}

// r3Normal maps the cluster a route names to "<svc key>:<port>" so that the
// default cluster ("<fqdn>", the legacy target's) and the per-port cluster of
// the same port compare equal. legacy is the service's legacy HTTP port, by
// mesh name.
func r3Normal(cluster string, legacy map[string]uint32) string {
	if p, ok := legacy[cluster]; ok {
		return fmt.Sprintf("%s:%d", cluster, p)
	}
	return cluster
}

func r3Route(t *testing.T, vh *routev3.VirtualHost, r *routev3.Route, host string, legacy map[string]uint32) string {
	t.Helper()
	var b strings.Builder
	m := r.GetMatch()
	fmt.Fprintf(&b, "match[prefix=%q path=%q", m.GetPrefix(), m.GetPath())
	for _, h := range m.GetHeaders() {
		fmt.Fprintf(&b, " hdr=%s", h.GetName())
	}
	if f := m.GetRuntimeFraction(); f != nil {
		fmt.Fprintf(&b, " fraction=%d", f.GetDefaultValue().GetNumerator())
	}
	b.WriteString("] ")
	switch {
	case r.GetDirectResponse() != nil:
		reason := ""
		for _, h := range r.GetResponseHeadersToAdd() {
			if h.GetHeader().GetKey() == proxy.NoHTTPPortErrorHeader {
				reason = h.GetHeader().GetValue()
			}
		}
		fmt.Fprintf(&b, "direct %d %s", r.GetDirectResponse().GetStatus(), reason)
		return b.String()
	case r.GetRedirect() != nil:
		b.WriteString("redirect")
	case r.GetRoute() != nil:
		ra := r.GetRoute()
		switch {
		case ra.GetClusterHeader() != "":
			// The cluster asked for is the authority that was typed.
			fmt.Fprintf(&b, "on-demand(%s)", host)
		case ra.GetCluster() != "":
			fmt.Fprintf(&b, "cluster(%s)", r3Normal(ra.GetCluster(), legacy))
		case ra.GetWeightedClusters() != nil:
			var parts []string
			for _, wc := range ra.GetWeightedClusters().GetClusters() {
				parts = append(parts, fmt.Sprintf("%s=%d", r3Normal(wc.GetName(), legacy), wc.GetWeight().GetValue()))
			}
			fmt.Fprintf(&b, "weighted(%s)", strings.Join(parts, ","))
		default:
			_, noMatch, ok := proxy.QUICSelectionArms(r)
			require.True(t, ok)
			fmt.Fprintf(&b, "cluster(%s)", r3Normal(noMatch, legacy))
		}
	}
	// The HTTP filters in force on this route: the virtual host's and the
	// route's own.
	var filters []string
	for name := range vh.GetTypedPerFilterConfig() {
		filters = append(filters, "vhost:"+name)
	}
	for name := range r.GetTypedPerFilterConfig() {
		filters = append(filters, "route:"+name)
	}
	sort.Strings(filters)
	fmt.Fprintf(&b, " filters=%v", filters)
	for _, hd := range r.GetResponseHeadersToAdd() {
		fmt.Fprintf(&b, " +%s=%s", hd.GetHeader().GetKey(), hd.GetHeader().GetValue())
	}
	return b.String()
}

// r3Pick is the proxy's virtual-host choice: exact, longest "x*", then "*".
func r3Pick(t *testing.T, table *routev3.RouteConfiguration, host string) *routev3.VirtualHost {
	t.Helper()
	var exact, wildcard, catchAll *routev3.VirtualHost
	wl := -1
	for _, vh := range table.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			switch {
			case d == "*":
				catchAll = vh
			case strings.HasSuffix(d, "*"):
				if p := strings.TrimSuffix(d, "*"); strings.HasPrefix(host, p) && len(p) > wl {
					wildcard, wl = vh, len(p)
				}
			case d == host:
				exact = vh
			}
		}
	}
	switch {
	case exact != nil:
		return exact
	case wildcard != nil:
		return wildcard
	}
	require.NotNil(t, catchAll)
	return catchAll
}

func r3Resolve(t *testing.T, table *routev3.RouteConfiguration, ns, host string, legacy map[string]uint32) r3Sig {
	t.Helper()
	vh := r3Pick(t, table, host)
	routes := vh.GetRoutes()
	if vh.GetMatcher() != nil {
		routes = matcherArm(t, vh.GetMatcher(), ns)
	}
	var sig r3Sig
	for _, r := range routes {
		matched := true
		for _, h := range r.GetMatch().GetHeaders() {
			if h.GetName() == ":authority" {
				matched = matched && mustRegexp(t, h.GetStringMatch().GetSafeRegex().GetRegex()).MatchString(host)
			}
		}
		if p := r.GetMatch().GetPath(); p != "" && p != "/" {
			matched = false
		}
		s := r3Route(t, vh, r, host, legacy)
		if vh.GetName() == "on_demand_catch_all" {
			if matched && sig.first == "" {
				sig.first = "catch-all: " + s
			}
			continue
		}
		sig.all = append(sig.all, s)
		if matched && sig.first == "" && r.GetMatch().GetRuntimeFraction() == nil {
			sig.first = s
		}
	}
	return sig
}

func r3NoDuplicateDomains(t *testing.T, what string, tables map[string]*routev3.RouteConfiguration) {
	t.Helper()
	for name, table := range tables {
		seen := map[string]string{}
		for _, vh := range table.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				if prev, dup := seen[d]; dup {
					t.Errorf("%s: table %s: domain %q is claimed by both %q and %q; the proxy rejects the WHOLE table", what, name, d, prev, vh.GetName())
				}
				seen[d] = vh.GetName()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 1. A short name on the proxy port, for a service the node knows by name
// only (a generated Service without real ports: the agent saw it before the
// registrar wrote them, or a chart 2.6 registrar wrote it; or a user Service
// that owns the name) and that carries a service-wide filter or a route.
//
// The full name's stand-in virtual host routes by cluster_header: the cluster
// asked for is the authority that was typed. Its routes are COPIED into the
// short name's arm, so there the cluster asked for is "echo:8080", which is
// no cluster's name and which the agent's on-demand observer ignores.
// ---------------------------------------------------------------------------
func TestShortName_ColdArmNamesTheFullNamesCluster(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	c := a043Cache(t, nil)
	a043ProjectNamesOnly(c, []a043Service{svc})
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))

	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		table := a043Tables(t, c)[tname]
		require.NotNil(t, table, tname)
		callingFrom(t, "demo")
		full := a043Resolve(t, table, "echo.demo."+a043Mesh+":8080")
		short := a043Resolve(t, table, "echo:8080")
		t.Logf("%s: full  -> %+v", tname, full)
		t.Logf("%s: short -> %+v", tname, short)
		require.True(t, full.onDemand, "the full name is parked on the cluster its authority names")
		_, fullOK := proxy.ServiceFromClusterName(full.cluster, a043Mesh)
		require.True(t, fullOK)
		if short.onDemand {
			_, ok := proxy.ServiceFromClusterName(short.cluster, a043Mesh)
			assert.Truef(t, ok, "%s: Host echo:8080 from a caller in demo asks on demand for a cluster named %q: no cluster has that name and the agent's ODCDS observer drops it (\"outside the mesh domain\"), so the request waits out the on-demand timeout and fails, where echo.demo.%s:8080 is parked and delivered", tname, short.cluster, a043Mesh)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. A service-wide (CHAIN) filter and a route's own ExtensionRef to the SAME
// filter. Under the full name the service-wide config sits on the virtual
// host and the route's on the route: the proxy has both levels. In the short
// name's arm the copy keeps the route's entry and DROPS the service-wide one.
// For a filter that applies every level (header_mutation) or merges them
// (ext_authz's context extensions), the short name runs without the
// service-wide configuration. (Live: perport_third_live_test.go.)
// ---------------------------------------------------------------------------
func TestShortName_ArmCarriesBothLevelsOfAFilter(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	stamp := func(key string) proxy.ExtensionFilter {
		cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
			ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: key, Value: "ran"}}}}},
		}})
		require.NoError(t, err)
		return proxy.ExtensionFilter{Name: r3Filter, Config: cfg}
	}
	stamps := func(a *anypb.Any) []string {
		var m header_mutationv3.HeaderMutationPerRoute
		require.NoError(t, a.UnmarshalTo(&m))
		var out []string
		for _, r := range m.GetMutations().GetResponseMutations() {
			out = append(out, r.GetAppend().GetHeader().GetKey())
		}
		return out
	}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{svc})
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": stamp("x-service-wide")})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {
		{Matches: []proxy.GammaMatch{{Prefix: "/"}}, ExtensionFilters: []proxy.ExtensionFilter{stamp("x-route-own")}},
	}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{svc})))

	checked := 0
	for tname, table := range a043Tables(t, c) {
		fullVh := r3Pick(t, table, "echo.demo."+a043Mesh+":8080")
		shortVh := r3Pick(t, table, "echo:8080")
		if fullVh.GetName() == "on_demand_catch_all" || len(fullVh.GetTypedPerFilterConfig()) == 0 {
			continue
		}
		require.NotNil(t, shortVh.GetMatcher(), tname)
		// Under the full name the proxy has the two levels.
		require.Equal(t, []string{"x-service-wide"}, stamps(fullVh.GetTypedPerFilterConfig()[r3Filter]), tname)
		// The rule's route is the first that takes "GET /"; the virtual
		// host's own last route follows it and has the service's level only.
		var routes []*routev3.Route
		for _, r := range matcherArm(t, shortVh.GetMatcher(), "demo") {
			if r.GetRoute() != nil {
				routes = append(routes, r)
			}
		}
		require.NotEmpty(t, routes, tname)
		for i, r := range routes {
			got := r.GetTypedPerFilterConfig()[r3Filter]
			require.NotNilf(t, got, "table %s: a route of the arm without the service-wide filter", tname)
			want := []string{"x-service-wide"}
			if i == 0 {
				// The short arm has ONE place, the route: it must hold the
				// service-wide configuration as well as the route's own, in
				// the order the proxy applies the two levels.
				want = []string{"x-route-own", "x-service-wide"}
			}
			assert.Equalf(t, want, stamps(got), "table %s, Host echo:8080 from demo, route %d of the arm: the levels of %s", tname, i, r3Filter)
		}
		checked++
	}
	require.GreaterOrEqual(t, checked, 3, "the loopback table, the proxy port's and cap_http_8080")
}

// ---------------------------------------------------------------------------
// 3. A user Service that owns the name maps 8000 -> 8080 (and 80 -> 8080).
// What does each path answer for the Service port and for the pod port?
// ---------------------------------------------------------------------------
func TestPerPort_UserServicePortMapOnBothPaths(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	for _, held := range []bool{true, false} {
		c := a043Cache(t, nil)
		// A user Service owns the name: the registrar generates none.
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/echo": {8000: 8080}})
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("r")}}})
		c.SetRouteTargetPorts(map[string][]uint32{})
		var listed []a043Service
		if held {
			listed = []a043Service{svc}
		}
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(listed)))
		tables := a043Tables(t, c)
		legacy := map[string]uint32{"echo.demo." + a043Mesh: 8080}
		full := "echo.demo." + a043Mesh
		get := func(table, host string) string {
			tb := tables[table]
			if tb == nil {
				return "(no table " + table + ")"
			}
			return r3Resolve(t, tb, "demo", host, legacy).first
		}
		capSvcPort := get("cap_http_8000", full+":8000")
		capPodPort := get("cap_http_8080", full+":8080")
		loopSvcPort := get(proxy.OutboundHTTPRouteName, full+":8000")
		loopPodPort := get(proxy.OutboundHTTPRouteName, full+":8080")
		t.Logf("held=%v captured dial :8000 (the Service port) -> %s", held, capSvcPort)
		t.Logf("held=%v captured dial :8080 (the pod port)     -> %s", held, capPodPort)
		t.Logf("held=%v proxy port, Host :8000 (Service port)  -> %s", held, loopSvcPort)
		t.Logf("held=%v proxy port, Host :8080 (pod port)      -> %s", held, loopPodPort)
		if held {
			// "A client addresses a Service port; the mesh resolves it to a pod
			// port" (the model, 2), and the proxy port "names the target, port
			// included": the two paths must agree on what a port of this name is.
			assert.Equalf(t, strings.Contains(capSvcPort, "cluster("), strings.Contains(loopSvcPort, "cluster("),
				"held=%v: Service port 8000 of a user Service: a captured dial gets %q, the proxy port gets %q", held, capSvcPort, loopSvcPort)
			assert.Equalf(t, strings.Contains(capPodPort, "cluster("), strings.Contains(loopPodPort, "cluster("),
				"held=%v: pod port 8080 behind a user Service that has no port 8080: a captured dial gets %q, the proxy port gets %q", held, capPodPort, loopPodPort)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Cold and warm must not differ. Random mesh states, built twice: once
// with the node holding no endpoint (it knows the ports from the generated
// Services), once holding them all. Every table, every name form, every port
// spelling, from every namespace: the answer must be the same, and no table
// may claim a domain twice.
// ---------------------------------------------------------------------------
func r3Seed(t *testing.T) int64 {
	if s := os.Getenv("COLD_WARM_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		require.NoError(t, err)
		return n
	}
	return 43
}

type r3State struct {
	services []a043Service
	filters  map[string]proxy.ExtensionFilter
	routes   map[string][]proxy.GammaRoute
}

func r3RandomState(t *testing.T, rng *rand.Rand) r3State {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	st := r3State{filters: map[string]proxy.ExtensionFilter{}, routes: map[string][]proxy.GammaRoute{}}
	httpPool := []uint32{80, 8080, 9090}
	i := 0
	for _, ns := range []string{"demo", "other"} {
		for _, name := range []string{"echo", "solo"} {
			if rng.Intn(4) == 0 {
				continue
			}
			i++
			s := a043Service{name: name, ns: ns, ports: map[uint32]registryv1.PortProtocol{}, ip: fmt.Sprintf("10.0.8.%d", i), vip: fmt.Sprintf("10.96.8.%d", i)}
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
			var nums []uint32
			for p := range s.ports {
				nums = append(nums, p)
			}
			slices.Sort(nums)
			s.legacy = nums[rng.Intn(len(nums))]
			st.services = append(st.services, s)
			if rng.Intn(2) == 0 {
				st.filters[s.key()] = r3ChainCfg(t)
			}
			if rng.Intn(2) == 0 {
				st.routes[s.key()] = append(st.routes[s.key()], proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("every:" + s.key())})
			}
			if rng.Intn(3) == 0 {
				pp := httpPool[rng.Intn(len(httpPool))]
				st.routes[s.key()] = append(st.routes[s.key()], proxy.GammaRoute{ParentPort: pp, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark(fmt.Sprintf("port%d:%s", pp, s.key()))})
			}
		}
	}
	// A rule with a backendRef to another service's port, valid or not.
	if len(st.services) >= 2 && rng.Intn(2) == 0 {
		from, to := st.services[0], st.services[len(st.services)-1]
		bp := httpPool[rng.Intn(len(httpPool))]
		st.routes[from.key()] = append(st.routes[from.key()], proxy.GammaRoute{
			Matches:  []proxy.GammaMatch{{Prefix: "/b"}},
			Backends: []proxy.GammaBackend{{Service: to.key(), Cluster: to.name + "." + to.ns + "." + a043Mesh, Port: bp, Weight: 1}},
		})
	}
	return st
}

func r3Build(t *testing.T, st r3State, held bool) *SnapshotCache {
	t.Helper()
	c := a043Cache(t, nil)
	a043Project(c, st.services)
	c.SetServiceChainFilters(st.filters)
	c.SetServiceRoutes(st.routes)
	var listed []a043Service
	if held {
		listed = st.services
	}
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(listed)))
	return c
}

func TestPerPort_ColdAndWarmAnswerAlike(t *testing.T) {
	seed := r3Seed(t)
	rng := rand.New(rand.NewSource(seed))
	// 60 rounds by default; COLD_WARM_ROUNDS widens a run by hand.
	rounds := 60
	if s := os.Getenv("COLD_WARM_ROUNDS"); s != "" {
		rounds, _ = strconv.Atoi(s)
	}
	diffs := map[string]int{}
	shown := 0
	compared := 0
	for round := 0; round < rounds; round++ {
		st := r3RandomState(t, rng)
		cold, warm := r3Build(t, st, false), r3Build(t, st, true)
		ct, wt := a043Tables(t, cold), a043Tables(t, warm)
		r3NoDuplicateDomains(t, fmt.Sprintf("seed %d round %d cold", seed, round), ct)
		r3NoDuplicateDomains(t, fmt.Sprintf("seed %d round %d warm", seed, round), wt)
		legacy := map[string]uint32{}
		for _, s := range st.services {
			if s.ports[s.legacy] == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
				legacy[s.name+"."+s.ns+"."+a043Mesh] = s.legacy
			}
		}
		var names []string
		for n := range wt {
			if _, ok := ct[n]; !ok {
				diffs["table only when warm: "+n]++
			}
			names = append(names, n)
		}
		for n := range ct {
			if _, ok := wt[n]; !ok {
				diffs["table only when cold: "+n]++
			}
		}
		sort.Strings(names)
		for _, tname := range names {
			if ct[tname] == nil {
				continue
			}
			for _, s := range st.services {
				forms := []string{s.name, s.name + "." + s.ns + "." + a043Mesh, s.name + "." + s.ns, s.name + "." + s.ns + ".svc", s.name + "." + s.ns + ".svc.cluster.local"}
				for fi, form := range forms {
					for _, suffix := range []string{"", ":80", ":8080", ":9090", ":9000", ":7777", ":18081", ":18082"} {
						for _, ns := range []string{"demo", "other", "third"} {
							host := form + suffix
							cs, ws := r3Resolve(t, ct[tname], ns, host, legacy), r3Resolve(t, wt[tname], ns, host, legacy)
							compared++
							if cs.first == ws.first && slices.Equal(cs.all, ws.all) {
								continue
							}
							kind := fmt.Sprintf("form=%d suffix=%s", fi, suffix)
							if cs.first == ws.first {
								kind += " (first route equal, arm differs)"
							}
							diffs[kind]++
							if shown < 25 {
								shown++
								t.Logf("seed %d round %d table %s caller %s Host %s (service %s ports %v legacy %d)\n   cold: %s\n   warm: %s\n   cold arm: %v\n   warm arm: %v",
									seed, round, tname, ns, host, s.key(), s.ports, s.legacy, cs.first, ws.first, cs.all, ws.all)
							}
						}
					}
				}
			}
		}
	}
	t.Logf("compared %d (table, name, port, caller) answers", compared)
	var kinds []string
	for k := range diffs {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		t.Errorf("cold != warm: %s: %d times", k, diffs[k])
	}
}

// ---------------------------------------------------------------------------
// 5. A generated Service whose pods declare the mesh's raw-TCP port (18082)
// as HTTP. The registrar writes it as a real HTTP port (p18082-tcp,
// appProtocol http); the agent's own derivation refuses to serve it.
// ---------------------------------------------------------------------------
func TestPerPort_DeclaredReservedPortColdAndWarmAlike(t *testing.T) {
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 18082: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	st := r3State{services: []a043Service{svc}}
	cold, warm := r3Build(t, st, false), r3Build(t, st, true)
	legacy := map[string]uint32{"echo.demo." + a043Mesh: 8080}
	host := "echo.demo." + a043Mesh + ":18082"
	cs := r3Resolve(t, a043Tables(t, cold)[proxy.OutboundHTTPRouteName], "demo", host, legacy)
	ws := r3Resolve(t, a043Tables(t, warm)[proxy.OutboundHTTPRouteName], "demo", host, legacy)
	t.Logf("cold: %s", cs.first)
	t.Logf("warm: %s", ws.first)
	assert.Equal(t, ws.first, cs.first, "proxy port, Host %s: cold and warm differ", host)
}

// ---------------------------------------------------------------------------
// 6. A Service that carries one port number as HTTP (TCP) and as UDP: the
// declared ports put it in both classes.
// ---------------------------------------------------------------------------
func TestPerPort_DeclaredPortInTwoClassesClaimsOnce(t *testing.T) {
	c := a043Cache(t, nil)
	c.SetCaptureAuthorities(map[string]string{"demo/echo": "echo.demo.svc.cluster.local"})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "demo/echo", ClusterIP: "10.96.9.1", DeclaredHTTP: []uint32{8080}, DeclaredOther: []uint32{8080}}})
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(nil)))
	r3NoDuplicateDomains(t, "http 8080 + udp 8080 declared", a043Tables(t, c))
}

// ---------------------------------------------------------------------------
// 7. #1694: refreshEntryVhosts takes the cluster WRITE lock inside snapshot
// generation. Rule events, registry refreshes, pod events and snapshot builds
// at once; run under --config=race. A deadlock shows as the timeout.
// ---------------------------------------------------------------------------
func TestPerPort_RuleEventsRaceSnapshotsAndRefreshes(t *testing.T) {
	ctx := context.Background()
	st := r3RandomState(t, rand.New(rand.NewSource(7)))
	c := r3Build(t, st, true)
	reg := a043Registry(st.services)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					f(i)
				}
			}
		}()
	}
	run(func(i int) {
		routes := map[string][]proxy.GammaRoute{}
		for _, s := range st.services {
			routes[s.key()] = []proxy.GammaRoute{{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark(strconv.Itoa(i))}}
		}
		c.SetServiceRoutes(routes)
	})
	run(func(i int) {
		f := map[string]proxy.ExtensionFilter{}
		if i%2 == 0 {
			for _, s := range st.services {
				f[s.key()] = r3ChainCfg(t)
			}
		}
		c.SetServiceChainFilters(f)
	})
	run(func(i int) {
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/usersvc": {80: uint32(8080 + i%2)}})
	})
	run(func(int) { _ = c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg) })
	run(func(int) { _ = c.generateSnapshot(ctx) })
	run(func(int) { _ = c.generateSnapshot(ctx) })
	run(func(i int) {
		if i%2 == 0 {
			a043Project(c, st.services)
		} else {
			a043ProjectNamesOnly(c, st.services)
		}
	})
	done := make(chan struct{})
	go func() {
		time.Sleep(3 * time.Second)
		close(stop)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock: the workers did not finish")
	}
	// Quiesce and check the tables agree with each other after the storm: the
	// loopback table and the capture tables carry the same filter decision.
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{st.services[0].key(): r3ChainCfg(t)})
	a043Project(c, st.services)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	tables := a043Tables(t, c)
	r3NoDuplicateDomains(t, "after the storm", tables)
	s := st.services[0]
	for tname, table := range tables {
		for _, vh := range table.GetVirtualHosts() {
			claims := false
			for _, d := range vh.GetDomains() {
				claims = claims || strings.HasPrefix(d, s.name+"."+s.ns+"."+a043Mesh)
			}
			if !claims || vh.GetMatcher() != nil {
				continue
			}
			for _, r := range vh.GetRoutes() {
				if r.GetRoute() == nil {
					continue
				}
				_, onVh := vh.GetTypedPerFilterConfig()[r3Filter]
				assert.Truef(t, onVh, "after the storm: table %s, virtual host %s delivers %s without its service-wide filter", tname, vh.GetName(), s.key())
			}
		}
	}
}
