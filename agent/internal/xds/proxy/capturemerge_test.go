package proxy

import (
	"strconv"
	"testing"

	"aethermesh.dev/agent/test/xdseval"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The compiler of the published capture table (MergeCaptureRouteTables), on
// small hand-built per-port tables: what it does with each shape, and what it
// refuses. The cache's tests hold it to the agent's real tables.

func mergeTestTables() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
	const mesh = "a.ns.mesh"
	routed := func(p uint32) *routev3.VirtualHost {
		vh := BuildServiceVirtualHost(mesh+"|"+strconv.Itoa(int(p)), mesh+":"+strconv.Itoa(int(p)), []string{mesh, mesh + ":" + strconv.Itoa(int(p)), "a.ns", "a.ns:" + strconv.Itoa(int(p))}, nil)
		// A service-wide filter, enabled on the virtual host.
		cfg, _ := anypb.New(wrapperspb.String("filter-config"))
		vh.TypedPerFilterConfig = map[string]*anypb.Any{"test.filter": cfg}
		return vh
	}
	mismatch := func(p uint32) *routev3.VirtualHost {
		return BuildRefusalVirtualHost(mesh+"|mismatch", []string{mesh + ":*", "a.ns:*"}, RefusalPortMismatch, "dialled "+strconv.Itoa(int(p))+"\n")
	}
	table := func(p uint32, vhs ...*routev3.VirtualHost) *routev3.RouteConfiguration {
		return BuildCaptureRouteConfiguration(CaptureHTTPRouteName(p), vhs, BuildCaptureCatchAllVirtualHost("mesh", p, false, true), nil)
	}
	perPort := map[uint32]*routev3.RouteConfiguration{
		8080: table(8080, routed(8080), mismatch(8080)),
		9090: table(9090, routed(9090), mismatch(9090)),
		80: table(80, BuildRefusalVirtualHost(mesh+"|refused", []string{mesh, mesh + ":80", "a.ns", "a.ns:80"}, RefusalPortNotDeclared, "undeclared\n"),
			mismatch(80)),
	}
	other := BuildCaptureRouteConfiguration(CaptureHTTPOtherRouteName,
		[]*routev3.VirtualHost{BuildRefusalVirtualHost(mesh+"|undeclared", []string{mesh, mesh + ":*", "a.ns", "a.ns:*"}, RefusalPortNotDeclared, "undeclared\n")},
		BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
	return perPort, other
}

func TestMergeCaptureRouteTables_OneVirtualHostPerName(t *testing.T) {
	perPort, other := mergeTestTables()
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	require.Equal(t, CaptureHTTPChainName, merged.GetName())
	names := []string{}
	for _, vh := range merged.GetVirtualHosts() {
		names = append(names, vh.GetName())
	}
	assert.Equal(t, []string{"a.ns.mesh", "on_demand_catch_all"}, names, "one virtual host per name, and the catch-all")
	// Its domains are the spellings port-less and ":*": an exact ":<port>"
	// spelling is told apart by the matcher.
	assert.ElementsMatch(t, []string{"a.ns", "a.ns:*", "a.ns.mesh", "a.ns.mesh:*"}, merged.GetVirtualHosts()[0].GetDomains())
	assert.NotNil(t, merged.GetVirtualHosts()[0].GetMatcher())
	assert.Empty(t, merged.GetVirtualHosts()[0].GetTypedPerFilterConfig(), "the filter config moves onto the routes it was in force on")

	at := func(host string, dialled string) []*routev3.Route {
		fs := map[string]string{}
		if dialled != "" {
			fs[DialledPortFilterStateKey] = dialled
		}
		a, err := xdseval.Resolve(merged, xdseval.Request{Host: host, FilterState: fs})
		require.NoError(t, err)
		return a.Routes
	}
	for _, tc := range []struct {
		host, dialled, want string
		filter              bool
	}{
		{"a.ns.mesh", "8080", "a.ns.mesh:8080", true},
		{"A.NS.MESH:8080", "8080", "a.ns.mesh:8080", true},
		{"a.ns:9090", "9090", "a.ns.mesh:9090", true},
		{"a.ns.mesh:9090", "8080", RefusalPortMismatch, false},
		{"a.ns.mesh:8080:8080", "8080", RefusalPortMismatch, false},
		{"a.ns.mesh", "80", RefusalPortNotDeclared, false},
		{"a.ns.mesh:8080", "80", RefusalPortMismatch, false},
		{"a.ns.mesh", "7777", RefusalPortNotDeclared, false},
		{"a.ns.mesh:8080", "", RefusalPortNotDeclared, false},
	} {
		routes := at(tc.host, tc.dialled)
		require.NotEmpty(t, routes, "%s dialled %s", tc.host, tc.dialled)
		o := xdseval.OutcomeOf(routes[0])
		if o.Reason != "" {
			assert.Equal(t, tc.want, o.Reason, "%s dialled %s", tc.host, tc.dialled)
		} else {
			assert.Equal(t, tc.want, o.Cluster, "%s dialled %s", tc.host, tc.dialled)
		}
		_, has := routes[0].GetTypedPerFilterConfig()["test.filter"]
		assert.Equal(t, tc.filter, has, "%s dialled %s: the service's filter", tc.host, tc.dialled)
	}
	// The catch-all names a cluster on demand only for the dialled port.
	for _, d := range []string{"80", "8080", "9090", "7777"} {
		routes := at("x.y.mesh:8080", d)
		var onDemand bool
		for _, r := range routes {
			onDemand = onDemand || r.GetRoute().GetClusterHeader() != ""
		}
		assert.Equal(t, d != "7777", onDemand, "the catch-all dialled %s", d)
	}
}

func TestMergeCaptureRouteTables_RefusesWhatItDoesNotModel(t *testing.T) {
	perPort, other := mergeTestTables()
	mutated := func(f func(rc *routev3.RouteConfiguration)) (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
		p, o := mergeTestTables()
		f(p[8080])
		return p, o
	}
	for name, in := range map[string]func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration){
		"a suffix wildcard": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				rc.VirtualHosts[0].Domains = append(rc.VirtualHosts[0].Domains, "*.ns.mesh")
			})
		},
		"a virtual host's retry policy": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				rc.VirtualHosts[0].RetryPolicy = &routev3.RetryPolicy{NumRetries: wrapperspb.UInt32(1)}
			})
		},
		"a table-level field": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) {
				rc.ResponseHeadersToAdd = []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: "x", Value: "y"}}}
			})
		},
		"no catch-all": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return mutated(func(rc *routev3.RouteConfiguration) { rc.VirtualHosts = rc.VirtualHosts[:len(rc.VirtualHosts)-1] })
		},
		"a matcher with routes": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			p, o := mergeTestTables()
			short := shortNameVirtualHost("a", "", map[string]shortArm{"ns": {routes: p[8080].GetVirtualHosts()[0].GetRoutes()}}, nil)
			short.Routes = p[8080].GetVirtualHosts()[0].GetRoutes()
			p[8080].VirtualHosts = append([]*routev3.VirtualHost{short}, p[8080].VirtualHosts...)
			return p, o
		},
		"a missing table": func() (map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) {
			return perPort, nil
		},
	} {
		p, o := in()
		_, err := MergeCaptureRouteTables(CaptureHTTPChainName, p, o)
		assert.Error(t, err, name)
	}
	_, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err, "the unmutated tables compile")
}

// TestMergeCaptureRouteTables_ShortNameKeepsItsCallerMatcher: a short name's
// virtual host answers by the caller's namespace; compiled, the dialled port
// selects which of those matchers runs.
func TestMergeCaptureRouteTables_ShortNameKeepsItsCallerMatcher(t *testing.T) {
	perPort, other := mergeTestTables()
	for p, rc := range perPort {
		arm := []*routev3.Route{BuildRefusalVirtualHost("", nil, "arm-"+strconv.Itoa(int(p)), "x\n").GetRoutes()[0]}
		rc.VirtualHosts = append([]*routev3.VirtualHost{shortNameVirtualHost("a", "", map[string]shortArm{"ns": {routes: arm}}, nil)}, rc.VirtualHosts...)
	}
	merged, err := MergeCaptureRouteTables(CaptureHTTPChainName, perPort, other)
	require.NoError(t, err)
	for _, d := range []string{"80", "8080", "9090"} {
		a, err := xdseval.Resolve(merged, xdseval.Request{Host: "a", FilterState: map[string]string{DialledPortFilterStateKey: d, CallerNamespaceFilterStateKey: "ns"}})
		require.NoError(t, err)
		require.NotEmpty(t, a.Routes)
		assert.Equal(t, "arm-"+d, xdseval.OutcomeOf(a.Routes[0]).Reason)
		a, err = xdseval.Resolve(merged, xdseval.Request{Host: "a", FilterState: map[string]string{DialledPortFilterStateKey: d}})
		require.NoError(t, err)
		assert.Equal(t, RefusalCallerUnknown, xdseval.OutcomeOf(a.Routes[0]).Reason, "no caller namespace")
	}
}
