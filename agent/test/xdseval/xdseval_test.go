package xdseval

import (
	"testing"

	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func anyOf(t *testing.T, m proto.Message) *anypb.Any {
	t.Helper()
	a, err := anypb.New(m)
	require.NoError(t, err)
	return a
}

func chain(t *testing.T, name string) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{Name: name, TypedConfig: anyOf(t, wrapperspb.String(name))}}}
}

// The model backtracks as Envoy's matching tree does: a matched key whose
// child does not match falls to the map's on_no_match, and a list moves on.
func TestSelectChain_MatcherBacktracks(t *testing.T) {
	in := func(m proto.Message) *xdscorev3.TypedExtensionConfig {
		return &xdscorev3.TypedExtensionConfig{Name: "in", TypedConfig: anyOf(t, m)}
	}
	sni := &xdsmatcherv3.Matcher{MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{{
		Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
			Input:   in(&network_inputsv3.ServerNameInput{}),
			Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Suffix{Suffix: ".example"}, IgnoreCase: true}},
		}}},
		OnMatch: chain(t, "tls"),
	}}}}}
	l := &listenerv3.Listener{
		FilterChains:       []*listenerv3.FilterChain{{Name: "tls"}, {Name: "http"}},
		DefaultFilterChain: &listenerv3.FilterChain{Name: "default"},
		FilterChainMatcher: &xdsmatcherv3.Matcher{
			MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
				Input: in(&network_inputsv3.DestinationIPInput{}),
				TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: map[string]*xdsmatcherv3.Matcher_OnMatch{
					"10.0.0.1": {OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: sni}},
				}}},
			}},
			OnNoMatch: &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: &xdsmatcherv3.Matcher{
				MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
					Input: in(&network_inputsv3.ApplicationProtocolInput{}),
					TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: map[string]*xdsmatcherv3.Matcher_OnMatch{
						"'http/1.1'": chain(t, "http"),
						"'gone'":     chain(t, "gone"),
					}}},
				}},
			}}},
		},
	}
	for _, tc := range []struct {
		c    Conn
		want string
	}{
		{Conn{IP: "10.0.0.1", SNI: "A.EXAMPLE", Transport: "tls"}, "tls"},
		{Conn{IP: "10.0.0.1", App: []string{"http/1.1"}}, "http"}, // backtracked
		{Conn{IP: "10.0.0.2", App: []string{"http/1.1"}}, "http"},
		{Conn{IP: "10.0.0.2"}, "default"},
		{Conn{IP: "10.0.0.2", App: []string{"gone"}}, ""}, // names no chain: closed
		{Conn{IP: "10.0.0.1", SNI: "other.org", Transport: "tls"}, "default"},
	} {
		got, err := SelectChain(l, tc.c)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%+v", tc.c)
	}
}

// Without a matcher, the legacy filter_chain_match does not backtrack.
func TestSelectChain_LegacyDoesNotBacktrack(t *testing.T) {
	l := &listenerv3.Listener{
		DefaultFilterChain: &listenerv3.FilterChain{Name: "default"},
		FilterChains: []*listenerv3.FilterChain{
			{Name: "tls", FilterChainMatch: &listenerv3.FilterChainMatch{PrefixRanges: []*corev3.CidrRange{{AddressPrefix: "10.0.0.1", PrefixLen: wrapperspb.UInt32(32)}}, ServerNames: []string{"a.example"}}},
			{Name: "tcp", FilterChainMatch: &listenerv3.FilterChainMatch{PrefixRanges: []*corev3.CidrRange{{AddressPrefix: "10.0.0.1", PrefixLen: wrapperspb.UInt32(32)}}, DestinationPort: wrapperspb.UInt32(9000)}},
			{Name: "http", FilterChainMatch: &listenerv3.FilterChainMatch{TransportProtocol: "raw_buffer", ApplicationProtocols: []string{"http/1.1"}}},
		},
	}
	for _, tc := range []struct {
		c    Conn
		want string
	}{
		{Conn{IP: "10.0.0.1", Port: 8080, Transport: "raw_buffer", App: []string{"http/1.1"}}, "default"}, // the trap
		{Conn{IP: "10.0.0.2", Port: 8080, Transport: "raw_buffer", App: []string{"http/1.1"}}, "http"},
		{Conn{IP: "10.0.0.1", Port: 9000, Transport: "tls", SNI: "a.example"}, "tcp"}, // #911
		{Conn{IP: "10.0.0.1", Port: 8080, Transport: "tls", SNI: "a.example"}, "tls"},
		{Conn{IP: "10.0.0.2", Port: 9000, Transport: "raw_buffer", App: []string{"http/1.1"}}, "default"},
	} {
		got, err := SelectChain(l, tc.c)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%+v", tc.c)
	}
}

func TestResolve_PicksAndMatches(t *testing.T) {
	route := func(cluster string) *routev3.Route {
		return &routev3.Route{Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}}, Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}}}}
	}
	list := func(c string) *xdsmatcherv3.Matcher_OnMatch {
		return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{Name: "routes", TypedConfig: anyOf(t, &routev3.RouteList{Routes: []*routev3.Route{route(c)}})}}}
	}
	rc := &routev3.RouteConfiguration{VirtualHosts: []*routev3.VirtualHost{
		{Name: "exact", Domains: []string{"a.b"}, Routes: []*routev3.Route{route("exact")}},
		{Name: "prefix", Domains: []string{"a.b:*"}, Matcher: &xdsmatcherv3.Matcher{
			MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{{
				Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
					Input:   &xdscorev3.TypedExtensionConfig{Name: "h", TypedConfig: anyOf(t, &typematcherv3.HttpRequestHeaderMatchInput{HeaderName: ":authority"})},
					Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: "a.b:1"}, IgnoreCase: true}},
				}}},
				OnMatch: list("one"),
			}}}},
			OnNoMatch: list("other"),
		}},
		{Name: "suffix", Domains: []string{"*.b"}, Routes: []*routev3.Route{route("suffix")}},
		{Name: "star", Domains: []string{"*"}, Routes: []*routev3.Route{route("star")}},
	}}
	for host, want := range map[string]string{"A.B": "exact", "a.b:1": "one", "A.B:1": "one", "a.b:2": "other", "x.b": "suffix", "b": "star", "a.b:": "star"} {
		a, err := Resolve(rc, Request{Host: host})
		require.NoError(t, err)
		first, err := FirstRoute(a.Routes, Request{Host: host})
		require.NoError(t, err)
		assert.Equal(t, want, OutcomeOf(first).Cluster, host)
	}
	same, _, err := SameAnswer(Answer{Routes: []*routev3.Route{route("x")}}, Answer{Routes: []*routev3.Route{route("x")}})
	require.NoError(t, err)
	assert.True(t, same)
	same, why, err := SameAnswer(Answer{Routes: []*routev3.Route{route("x")}}, Answer{Routes: []*routev3.Route{route("y")}})
	require.NoError(t, err)
	assert.False(t, same, why)
	same, _, err = SameAnswer(Answer{NoRoutes: true}, Answer{Routes: []*routev3.Route{route("y")}})
	require.NoError(t, err)
	assert.False(t, same)
	assert.Equal(t, uint32(404), OutcomeOf(nil).Status)
}

// TestFirstRoute_APathlessRequestTakesOnlyAConnectRoute pins the proxy's
// behaviour: with no :path, VirtualHostImpl::getRouteFromRoutes
// (source/common/router/config_impl.cc) skips every route whose
// supportsPathlessHeaders() is false, and only ConnectRouteEntryImpl's is
// true (config_impl.h). A regex ".*", an empty prefix and "/" take a request
// with a path and not one without; a CONNECT matcher takes the CONNECT. Seen
// on the pinned proxy: on such a virtual host "GET /x" took the regex route
// and "CONNECT a.ns.mesh:8080" got 404 no route.
func TestFirstRoute_APathlessRequestTakesOnlyAConnectRoute(t *testing.T) {
	route := func(name string, m *routev3.RouteMatch) *routev3.Route {
		return &routev3.Route{Name: name, Match: m}
	}
	regex := route("regex", &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_SafeRegex{SafeRegex: &typematcherv3.RegexMatcher{Regex: ".*"}}})
	empty := route("empty-prefix", &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: ""}})
	slash := route("slash", &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}})
	connect := route("connect", &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_ConnectMatcher_{ConnectMatcher: &routev3.RouteMatch_ConnectMatcher{}}})
	first := func(routes []*routev3.Route, r Request) string {
		t.Helper()
		got, err := FirstRoute(routes, r)
		require.NoError(t, err)
		return got.GetName()
	}
	plain := []*routev3.Route{regex, empty, slash}
	assert.Equal(t, "regex", first(plain, Request{Host: "a.ns.mesh:8080", Path: "/x"}))
	assert.Empty(t, first(plain, Request{Host: "a.ns.mesh:8080", NoPath: true}), "no route takes a request with no path")
	withConnect := []*routev3.Route{regex, empty, slash, connect}
	assert.Equal(t, "connect", first(withConnect, Request{Host: "a.ns.mesh:8080", NoPath: true}))
	assert.Equal(t, "regex", first(withConnect, Request{Host: "a.ns.mesh:8080", Path: "/x"}), "a CONNECT route takes no other request")
	assert.Equal(t, "connect", first([]*routev3.Route{connect, slash}, Request{Host: "a.ns.mesh:8080", Path: "/x", Connect: true}), "an extended CONNECT with a path")
}

func TestMatchString(t *testing.T) {
	for _, tc := range []struct {
		sm   *xdsmatcherv3.StringMatcher
		v    string
		want bool
	}{
		{&xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: "A"}}, "a", false},
		{&xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: "A"}, IgnoreCase: true}, "a", true},
		{&xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Prefix{Prefix: "a:"}}, "a:1", true},
		{&xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Contains{Contains: ":"}}, "a", false},
		{&xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_SafeRegex{SafeRegex: &xdsmatcherv3.RegexMatcher{Regex: ".+"}}}, "x", true},
		{&xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_SafeRegex{SafeRegex: &xdsmatcherv3.RegexMatcher{Regex: ".+"}}}, "", false},
	} {
		got, err := MatchString(tc.sm, tc.v)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%v on %q", tc.sm, tc.v)
	}
	_, err := MatchString(&xdsmatcherv3.StringMatcher{}, "x")
	assert.Error(t, err, "an empty pattern is not modelled")
}
