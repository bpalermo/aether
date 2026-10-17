package proxy

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/test/xdseval"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestMergeCaptureRouteTables_APathlessRequestNeverTakesTheOtherPortsBlock:
// the dialled port's table answers "a.ns.mesh" and "a.ns.mesh:8080" with
// "/"-prefixed routes only, and cap_http_other answers the name (any port)
// with a regex rule. A CONNECT with no path dialled to 8080 gets no route
// from its own table; compiled, it falls out of the 8080 block into the other
// ports' block, which matches no dialled port, and took the regex rule there
// (adversarial review of the change). Dialled to a port with no block of its
// own, it still takes it, and a request with a path is answered as before.
func TestMergeCaptureRouteTables_APathlessRequestNeverTakesTheOtherPortsBlock(t *testing.T) {
	const mesh = "a.ns.mesh"
	rule := &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_SafeRegex{SafeRegex: &typematcherv3.RegexMatcher{Regex: ".*"}}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "other-by-regex"}}},
	}
	routed := BuildServiceVirtualHost(mesh+"|8080", mesh+":8080", []string{mesh, mesh + ":8080"}, nil)
	perPort := map[uint32]*routev3.RouteConfiguration{
		8080: BuildCaptureRouteConfiguration(CaptureHTTPRouteName(8080), []*routev3.VirtualHost{routed}, BuildCaptureCatchAllVirtualHost("mesh", 8080, false, true), nil),
	}
	otherVH := BuildServiceVirtualHost(mesh+"|other", mesh+":9090", []string{mesh, mesh + ":*"}, nil)
	otherVH.Routes = append([]*routev3.Route{proto.Clone(rule).(*routev3.Route)}, otherVH.Routes...)
	other := BuildCaptureRouteConfiguration(CaptureHTTPOtherRouteName, []*routev3.VirtualHost{otherVH}, BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)

	connect := xdseval.Request{Host: mesh + ":8080", NoPath: true, FilterState: map[string]string{DialledPortFilterStateKey: "8080"}}
	own, err := xdseval.Resolve(perPort[8080], connect)
	require.NoError(t, err)
	ownRoute, err := xdseval.FirstRoute(own.Routes, connect)
	require.NoError(t, err)
	require.Nil(t, ownRoute, "anti-vacuity: its own table gives it no route")
	a, err := xdseval.Resolve(merged, connect)
	require.NoError(t, err)
	r, err := xdseval.FirstRoute(a.Routes, connect)
	require.NoError(t, err)
	assert.Nil(t, r, "a CONNECT dialled to 8080 with no path takes cap_http_other's route %v", r.GetRoute().GetCluster())

	hosts := []string{mesh, mesh + ":8080", mesh + ":9090", "A.NS.MESH:8080"}
	// Every dialled port but none: a connection with no dialled port gets no
	// route for a request with no path, where cap_http_other's table gives
	// the regex rule (closeToPathlessOfPorts says why), so "" is held with a
	// path only.
	assert.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, []string{"8080", "80", "808", "80800", "9090", "1"}, []string{""}, "/", "/x", ""))
	assert.Empty(t, mergeMismatches(t, merged, perPort, other, hosts, []string{""}, []string{""}, "/", "/x"))
}

// TestComplementPatterns: the regexes match every string that is none of the
// values, each by exactly one of them, and no value.
func TestComplementPatterns(t *testing.T) {
	for _, values := range [][]string{{"8080"}, {"80", "8080", "9090", "18081"}, {"1", "10", "100"}, {"1000", "2373", "3746", "5119"}} {
		var res []*regexp.Regexp
		for _, p := range complementPatterns(values) {
			res = append(res, regexp.MustCompile(p))
		}
		in := map[string]bool{}
		for _, v := range values {
			in[v] = true
		}
		var probe []string
		for i := range 100000 {
			probe = append(probe, strconv.Itoa(i))
		}
		probe = append(probe, "", "x", "8080x", "08080", "-1")
		for _, s := range probe {
			n := 0
			for _, re := range res {
				if re.MatchString(s) {
					n++
				}
			}
			want := 1
			if in[s] {
				want = 0
			}
			if n != want {
				t.Fatalf("values %v: %q is matched by %d patterns, want %d", values, s, n, want)
			}
		}
	}
}

// oldPick is mergeTable.pick as it was until chart 3.0.3: a scan of every
// wildcard for the longest prefix of host.
func (t *mergeTable) oldPick(host string) *routev3.VirtualHost {
	if vh, ok := t.exact[host]; ok {
		return vh
	}
	var best *routev3.VirtualHost
	bestLen := -1
	for prefix, vh := range t.wild {
		if len(host) > len(prefix) && strings.HasPrefix(host, prefix) && len(prefix) > bestLen {
			best, bestLen = vh, len(prefix)
		}
	}
	if best != nil {
		return best
	}
	return t.star
}

// TestMergeTable_PickIsTheScanItReplaced: a randomized differential of the
// one-lookup pick against the scan it replaced, over every table
// indexMergeTable accepts from a small alphabet that collides on purpose
// (adopted from the adversarial review of the change).
func TestMergeTable_PickIsTheScanItReplaced(t *testing.T) {
	rng := rand.New(rand.NewSource(1710))
	names := []string{"", "a", "A", "a.b", "a.B", "ab", "b", "a.", ".a", "a-b"}
	sufs := []string{"", ":*", ":80", ":8080", ":0", ":", "*", ":8*", "::*"}
	alpha := "aAb.:*80-"
	tables := 0
	for range 20000 {
		rc := &routev3.RouteConfiguration{Name: "t"}
		for v := range 1 + rng.Intn(5) {
			vh := &routev3.VirtualHost{Name: "v" + strconv.Itoa(v)}
			for d := rng.Intn(4); d >= 0; d-- {
				vh.Domains = append(vh.Domains, names[rng.Intn(len(names))]+sufs[rng.Intn(len(sufs))])
			}
			rc.VirtualHosts = append(rc.VirtualHosts, vh)
		}
		rc.VirtualHosts = append(rc.VirtualHosts, &routev3.VirtualHost{Name: "star", Domains: []string{"*"}})
		mt, err := indexMergeTable(rc)
		if err != nil {
			continue
		}
		tables++
		var hosts []string
		for d := range mt.exact {
			hosts = append(hosts, d, d+"0", d+":", d+":1")
		}
		for p := range mt.wild {
			hosts = append(hosts, p, p+"1", p+"8080", p+"1:2", strings.TrimSuffix(p, ":"), p+":")
		}
		for range 50 {
			b := make([]byte, rng.Intn(9))
			for i := range b {
				b[i] = alpha[rng.Intn(len(alpha))]
			}
			hosts = append(hosts, strings.ToLower(string(b)))
		}
		for _, h := range hosts {
			if o, n := mt.oldPick(h), mt.pick(h); o != n {
				t.Fatalf("table %v host %q: the scan picks %s, pick %s", rc.GetVirtualHosts(), h, o.GetName(), n.GetName())
			}
		}
	}
	require.Greater(t, tables, 1000)
}
