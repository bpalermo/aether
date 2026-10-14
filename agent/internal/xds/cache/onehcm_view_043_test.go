package cache

// The one-HCM capture listener publishes ONE route table, cap_http, compiled
// from the per-port tables (cap_http_<P>, cap_http_other) that state the
// addressing rule of proposal 043 port by port (proxy.MergeCaptureRouteTables).
//
// The tests of this package were written against the per-port tables, which
// are still built: they read them through perPortView, which also holds the
// published table to the same answers for every request (gate (b) of the
// change, compiledMismatches). A test that asserts a per-port table therefore
// asserts what the proxy does with the published one.

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/xdseval"
	meshconst "aethermesh.dev/common/constants/mesh"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// otherDialled is a port number no fixture declares: what cap_http_other
// answers.
const otherDialled = 17777

// compiledMismatches returns every request for which the compiled table and
// the per-port table of its dialled port answer differently, at most limit of
// them (0: all).
//
// The requests: every Host spelling that selects a domain of any per-port
// table (the domain itself, upper-cased, with each dialled port and another
// port after it, twice over, and with no port), a Host no domain names, each
// dialled port of the tables plus one no table is for, and no dialled port at
// all (which must be cap_http_other's answer: refused, never delivered); and,
// for a name without a namespace, each caller namespace the domains name, one
// they do not, and none.
func compiledMismatches(merged *routev3.RouteConfiguration, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration, limit int, allPaths bool) []string {
	ports := slices.Sorted(maps.Keys(perPort))
	tables := map[uint32]*xdseval.Table{}
	for p, rc := range perPort {
		tables[p] = xdseval.NewTable(rc)
	}
	otherT, mergedT := xdseval.NewTable(other), xdseval.NewTable(merged)

	hosts := map[string]struct{}{"example.com": {}, "example.com:8080": {}, "": {}}
	namespaces := map[string]struct{}{}
	suffixes := []string{"", ":" + strconv.Itoa(otherDialled), ":0"}
	for _, p := range ports {
		suffixes = append(suffixes, ":"+strconv.Itoa(int(p)), fmt.Sprintf(":%d:%d", p, p))
	}
	addName := func(name string) {
		for _, s := range suffixes {
			hosts[name+s] = struct{}{}
		}
		hosts[strings.ToUpper(name)+":"+strconv.Itoa(int(firstOr(ports, 80)))] = struct{}{}
		hosts[strings.ToUpper(name)] = struct{}{}
		if parts := strings.Split(name, "."); len(parts) >= 2 {
			namespaces[parts[1]] = struct{}{}
		}
		// A mesh-shaped name no table claims, under the same domain: what
		// the catch-all's on-demand route answers.
		if parts := strings.Split(name, "."); len(parts) >= 3 {
			unknown := "zz-unknown.zz-unknown." + strings.Join(parts[2:], ".")
			for _, s := range suffixes {
				hosts[unknown+s] = struct{}{}
			}
		}
	}
	for _, rc := range append(slices.Collect(maps.Values(perPort)), other) {
		for _, vh := range rc.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				if d == "*" {
					continue
				}
				name, _, _ := strings.Cut(d, ":")
				addName(name)
				hosts[d] = struct{}{}
			}
		}
	}
	callers := []string{"", "no-such-namespace"}
	for ns := range namespaces {
		callers = append(callers, ns)
	}
	slices.Sort(callers)
	dialled := append(slices.Clone(ports), otherDialled, 0)

	probes := []routeProbe{{path: "/"}}
	if allPaths {
		probes = routeProbes(append(slices.Collect(maps.Values(perPort)), other))
	}
	var out []string
	for _, host := range slices.Sorted(maps.Keys(hosts)) {
		forCallers := []string{"default"}
		if !strings.Contains(strings.Split(host, ":")[0], ".") {
			forCallers = callers
		}
		for _, d := range dialled {
			for _, ns := range forCallers {
				fs := map[string]string{}
				if d != 0 {
					fs[proxy.DialledPortFilterStateKey] = strconv.Itoa(int(d))
				}
				if ns != "" {
					fs[proxy.CallerNamespaceFilterStateKey] = ns
				}
				want := otherT
				if t, ok := tables[d]; ok {
					want = t
				}
				for _, pr := range probes {
					req := xdseval.Request{Host: host, Path: pr.path, Headers: pr.headers, FilterState: fs}
					why := sameFirstRoute(want, mergedT, req)
					if why == "" {
						continue
					}
					out = append(out, fmt.Sprintf("host %q dialled %d caller %q path %s headers %v: %s", host, d, ns, pr.path, pr.headers, why))
					if limit > 0 && len(out) >= limit {
						return out
					}
				}
			}
		}
	}
	return out
}

// routeProbe is a path and request headers to ask a table about.
type routeProbe struct {
	path    string
	headers map[string]string
}

// routeProbes returns "/", the path of every route of the tables (a GAMMA
// rule's prefix or exact path, the liveness path), and "/" with every header a
// route matches exactly: each route of the tables is the first match of one of
// them where it can be.
func routeProbes(tables []*routev3.RouteConfiguration) []routeProbe {
	paths := map[string]bool{"/": true}
	headers := map[string]string{}
	for _, rc := range tables {
		for _, vh := range rc.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				if p := r.GetMatch().GetPrefix() + r.GetMatch().GetPath(); p != "" {
					paths[p] = true
				}
				for _, h := range r.GetMatch().GetHeaders() {
					if e := h.GetStringMatch().GetExact(); e != "" && !strings.HasPrefix(h.GetName(), ":") {
						headers[h.GetName()] = e
					}
				}
			}
		}
	}
	var out []routeProbe
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		out = append(out, routeProbe{path: p})
	}
	if len(headers) > 0 {
		out = append(out, routeProbe{path: "/", headers: headers})
	}
	return out
}

// sameFirstRoute is "" when the per-port table and the compiled one give req
// the same first route (SameTreatment: action, filters, rules, status and
// x-aether-error; not the match, not the body), and why not otherwise.
func sameFirstRoute(want, got *xdseval.Table, req xdseval.Request) string {
	a, errA := want.Resolve(req)
	b, errB := got.Resolve(req)
	if errA != nil || errB != nil {
		return fmt.Sprintf("%v / %v", errA, errB)
	}
	var ra []*routev3.Route
	if !a.NoVhost {
		ra = a.Routes
	}
	fa, errA := xdseval.FirstRoute(ra, req)
	var rb []*routev3.Route
	if !b.NoVhost {
		rb = b.Routes
	}
	fb, errB := xdseval.FirstRoute(rb, req)
	if errA != nil || errB != nil {
		return fmt.Sprintf("%v / %v", errA, errB)
	}
	if same, why := xdseval.SameTreatmentIn(a.VhostConfig, fa, b.VhostConfig, fb); !same {
		return fmt.Sprintf("per-port vhost %s, compiled vhost %s:%s", a.Vhost, b.Vhost, why)
	}
	return ""
}

func firstOr(ports []uint32, def uint32) uint32 {
	if len(ports) == 0 {
		return def
	}
	return ports[0]
}

// requireCompiledEquivalent is gate (b) as every per-port read of this
// package's tests runs it: every request with path "/". The dedicated tests
// (onehcm_equivalence_043_test.go) also ask every path and header a route of
// the tables matches (requireCompiledEquivalentAllPaths).
func requireCompiledEquivalent(t testing.TB, merged *routev3.RouteConfiguration, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration) {
	t.Helper()
	// The cache returns the SAME table object while its inputs are unchanged
	// (captureRouteTables' memo), and the walks read it again on every step:
	// a table checked once against the per-port tables of its inputs is not
	// checked again.
	if _, done := checkedCompiled.Load(merged); done {
		return
	}
	requireCompiled(t, merged, perPort, other, false)
	checkedCompiled.Store(merged, true)
}

// checkedCompiled holds the published tables requireCompiledEquivalent has
// checked.
var checkedCompiled sync.Map

// requireCompiledEquivalentAllPaths is gate (b) over every probe.
func requireCompiledEquivalentAllPaths(t testing.TB, merged *routev3.RouteConfiguration, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration) {
	t.Helper()
	requireCompiled(t, merged, perPort, other, true)
}

func requireCompiled(t testing.TB, merged *routev3.RouteConfiguration, perPort map[uint32]*routev3.RouteConfiguration, other *routev3.RouteConfiguration, allPaths bool) {
	t.Helper()
	require.NotNil(t, merged, "no compiled table")
	bad := compiledMismatches(merged, perPort, other, 20, allPaths)
	require.Empty(t, bad, "the compiled cap_http answers differently from the per-port tables:\n%s", strings.Join(bad, "\n"))
}

// perPortView returns the per-port tables the cache compiles cap_http from
// for ports (cap_http_<P> for each port but the proxy port, and
// cap_http_other), sorted by name, after checking that what captureRouteTables
// returns for the same ports is their compilation and answers as they do.
func perPortView(t testing.TB, c *SnapshotCache, ports []uint32) []types.Resource {
	t.Helper()
	published := c.captureRouteTables(ports)
	require.Len(t, published, 1, "one capture route table")
	merged := published[0].(*routev3.RouteConfiguration)
	require.Equal(t, proxy.CaptureHTTPChainName, merged.GetName())
	perPort, other := c.capturePerPortTables(c.captureTableInputs(), ports)
	requireCompiledEquivalent(t, merged, perPort, other)
	out := make([]types.Resource, 0, len(perPort)+1)
	for _, p := range slices.Sorted(maps.Keys(perPort)) {
		out = append(out, perPort[p])
	}
	return append(out, other)
}

// withPerPortView replaces, in a snapshot's route tables by name, cap_http
// with the per-port tables it was compiled from, after checking it answers as
// they do. The snapshot's own tables win on a name. (The tests of this package
// walk every table of the map by its per-port meaning; the published table is
// read with a043Published.)
func withPerPortView(t testing.TB, c *SnapshotCache, snapTables map[string]*routev3.RouteConfiguration) map[string]*routev3.RouteConfiguration {
	t.Helper()
	merged, ok := snapTables[proxy.CaptureHTTPChainName]
	if !ok {
		return snapTables
	}
	ports := c.captureHTTPPorts()
	perPort, other := c.capturePerPortTables(c.captureTableInputs(), ports)
	requireCompiledEquivalent(t, merged, perPort, other)
	delete(snapTables, proxy.CaptureHTTPChainName)
	for _, rc := range append(slices.Collect(maps.Values(perPort)), other) {
		if _, taken := snapTables[rc.GetName()]; !taken {
			snapTables[rc.GetName()] = rc
		}
	}
	return snapTables
}

// a043Published returns the cap_http table of node-1's snapshot.
func a043Published(t testing.TB, c *SnapshotCache) *routev3.RouteConfiguration {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	rc, ok := snap.GetResources(resourcev3.RouteType)[proxy.CaptureHTTPChainName].(*routev3.RouteConfiguration)
	require.True(t, ok, "the snapshot carries no cap_http")
	return rc
}

// perPortName is the per-port table of dialled port d (0: cap_http_other).
func perPortName(d uint32) string {
	if d == 0 {
		return proxy.CaptureHTTPOtherRouteName
	}
	return proxy.CaptureHTTPRouteName(d)
}

// capturePortOf is the port a per-port table's name stands for, and false
// for any other name (cap_http_other and cap_http included).
func capturePortOf(name string) (uint32, bool) {
	rest, ok := strings.CutPrefix(name, proxy.CaptureHTTPRoutePrefix)
	if !ok {
		return 0, false
	}
	p, err := strconv.ParseUint(rest, 10, 16)
	if err != nil || p == 0 {
		return 0, false
	}
	return uint32(p), true
}

var _ = meshconst.ProxyOutboundPort
