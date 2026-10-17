package xdseval

import (
	"errors"
	"testing"

	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func ipInput(t *testing.T, m proto.Message) *xdscorev3.TypedExtensionConfig {
	return &xdscorev3.TypedExtensionConfig{Name: "in", TypedConfig: anyOf(t, m)}
}

// portOnly: a nested matcher that matches only destination port p, to chain.
// On any other port it has no match, so the IP matcher moves on.
func portOnly(t *testing.T, p, chainName string) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: &xdsmatcherv3.Matcher{
		MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
			Input:    ipInput(t, &network_inputsv3.DestinationPortInput{}),
			TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: map[string]*xdsmatcherv3.Matcher_OnMatch{p: chain(t, chainName)}}},
		}},
	}}}
}

func ipListener(m *xdsmatcherv3.Matcher, chains ...string) *listenerv3.Listener {
	l := &listenerv3.Listener{DefaultFilterChain: &listenerv3.FilterChain{Name: "default"}, FilterChainMatcher: m}
	for _, c := range chains {
		l.FilterChains = append(l.FilterChains, &listenerv3.FilterChain{Name: c})
	}
	return l
}

// The model runs an IPMatcher tree as the pinned proxy's IpRangeMatcher does
// (source/extensions/common/matcher/ip_range_matcher.h at v1.40.0): the
// longest prefix first, then declaration order; a nested no-match moves on
// to the next candidate; an exclusive candidate only when it is the first;
// then the tree's on_no_match.
func TestSelectChain_IPMatcher(t *testing.T) {
	dst := ipInput(t, &network_inputsv3.DestinationIPInput{})
	m := IPRangeTree(dst, []IPRange{
		// Declared first and the shortest: declaration order alone would
		// pick it for every address below.
		{Prefixes: []string{"10.96.0.0/12", "192.168.0.0/16"}, OnMatch: chain(t, "svc")},
		// A /32 inside the /12.
		{Prefixes: []string{"10.96.0.5/32"}, OnMatch: chain(t, "pinned")},
		// A /32 inside the /12 whose nested matcher holds one port only.
		{Prefixes: []string{"10.96.0.6/32"}, OnMatch: portOnly(t, "9000", "pinned-9000")},
		// An exclusive /24 inside the /12, and a /32 inside it whose nested
		// matcher holds one port only.
		{Prefixes: []string{"10.96.7.0/24"}, OnMatch: chain(t, "excl"), Exclusive: true},
		{Prefixes: []string{"10.96.7.7/32"}, OnMatch: portOnly(t, "9000", "pinned-7")},
		// An exclusive /20 whose nested matcher holds one port only: first,
		// it is tried, and its no-match moves on.
		{Prefixes: []string{"10.97.0.0/20"}, OnMatch: portOnly(t, "9000", "excl-9000"), Exclusive: true},
		// Same length as the first range matcher's /12, declared later.
		{Prefixes: []string{"10.96.0.0/12"}, OnMatch: chain(t, "svc-later")},
	}, chain(t, "outside"))
	l := ipListener(m, "svc", "pinned", "pinned-9000", "excl", "pinned-7", "excl-9000", "svc-later", "outside")
	for _, tc := range []struct {
		ip   string
		port uint32
		want string
		why  string
	}{
		{"10.96.0.5", 80, "pinned", "a /32 inside the /12 wins: longest prefix first"},
		{"10.96.0.4", 80, "svc", "the /12 alone"},
		{"10.100.3.4", 80, "svc", "equal prefixes: the one declared first"},
		{"10.96.0.6", 9000, "pinned-9000", "the /32's nested matcher matches"},
		{"10.96.0.6", 80, "svc", "the /32's nested matcher has no match: on to the /12"},
		{"10.96.7.1", 80, "excl", "an exclusive candidate that comes first is tried"},
		{"10.96.7.7", 9000, "pinned-7", "the /32 first"},
		{"10.96.7.7", 80, "svc", "the /32 has no match, and the exclusive /24 after it is skipped"},
		{"10.97.0.1", 9000, "excl-9000", "the exclusive /20 first, and it matches"},
		{"10.97.0.1", 80, "svc", "the exclusive /20 first, no match, then the /12"},
		{"192.168.3.3", 80, "svc", "the range matcher's second range"},
		{"10.95.255.255", 80, "outside", "below the /12: on_no_match"},
		{"10.112.0.0", 80, "outside", "above the /12: on_no_match"},
		{"8.8.8.8", 80, "outside", "outside every range"},
		{"not-an-address", 80, "outside", "not an address: on_no_match"},
	} {
		got, err := SelectChain(l, Conn{IP: tc.ip, Port: tc.port})
		require.NoError(t, err, tc.ip)
		assert.Equal(t, tc.want, got, "%s:%d: %s", tc.ip, tc.port, tc.why)
	}

	// Without an on_no_match, an address outside every range, or one whose
	// candidates all have no match, takes the listener's default chain.
	bare := IPRangeTree(dst, []IPRange{{Prefixes: []string{"10.96.0.6/32"}, OnMatch: portOnly(t, "9000", "pinned")}}, nil)
	for _, c := range []Conn{{IP: "8.8.8.8", Port: 80}, {IP: "10.96.0.6", Port: 80}} {
		got, err := SelectChain(ipListener(bare, "pinned"), c)
		require.NoError(t, err)
		assert.Equal(t, "default", got, "%+v", c)
	}
}

// A range's address is truncated to its prefix length, as CidrRange::create
// does; an IPv4 range does not hold an IPv6 address.
func TestSelectChain_IPMatcherRanges(t *testing.T) {
	dst := ipInput(t, &network_inputsv3.DestinationIPInput{})
	raw := func(prefix string, l uint32, name string) *xdsmatcherv3.Matcher {
		m := IPRangeTree(dst, nil, nil)
		ipm := &xdsmatcherv3.IPMatcher{RangeMatchers: []*xdsmatcherv3.IPMatcher_IPRangeMatcher{{
			Ranges:  []*xdscorev3.CidrRange{{AddressPrefix: prefix, PrefixLen: wrapperspb.UInt32(l)}},
			OnMatch: chain(t, name),
		}}}
		m.GetMatcherTree().GetCustomMatch().TypedConfig = anyOf(t, ipm)
		return m
	}
	got, err := SelectChain(ipListener(raw("10.96.0.1", 12, "svc"), "svc"), Conn{IP: "10.100.0.1"})
	require.NoError(t, err)
	assert.Equal(t, "svc", got, "10.96.0.1/12 is 10.96.0.0/12")
	got, err = SelectChain(ipListener(raw("0.0.0.0", 0, "any4"), "any4"), Conn{IP: "fd00::1"})
	require.NoError(t, err)
	assert.Equal(t, "default", got, "0.0.0.0/0 holds no IPv6 address")

	_, err = SelectChain(ipListener(raw("10.96.0.300", 12, "svc"), "svc"), Conn{IP: "10.96.0.1"})
	require.Error(t, err, "a malformed range is a configuration the proxy refuses")
	_, err = SelectChain(ipListener(raw("10.96.0.0", 129, "svc"), "svc"), Conn{IP: "10.96.0.1"})
	require.Error(t, err, "a prefix_len over 128 is refused")
}

// A custom_match that is not an IPMatcher stays unmodelled, and so does a
// custom_match predicate.
func TestSelectChain_OtherCustomMatchIsUnsupported(t *testing.T) {
	m := IPRangeTree(ipInput(t, &network_inputsv3.DestinationIPInput{}), nil, nil)
	m.GetMatcherTree().GetCustomMatch().TypedConfig = anyOf(t, &xdsmatcherv3.ServerNameMatcher{})
	_, err := SelectChain(ipListener(m), Conn{IP: "10.0.0.1"})
	var u Unsupported
	require.True(t, errors.As(err, &u), "got %v", err)

	pred := &xdsmatcherv3.Matcher{MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{{
		Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
			Input:   ipInput(t, &network_inputsv3.DestinationIPInput{}),
			Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_CustomMatch{CustomMatch: &xdscorev3.TypedExtensionConfig{Name: "ip", TypedConfig: anyOf(t, &xdsmatcherv3.IPMatcher{})}},
		}}},
		OnMatch: chain(t, "x"),
	}}}}}
	_, err = SelectChain(ipListener(pred, "x"), Conn{IP: "10.0.0.1"})
	require.True(t, errors.As(err, &u), "got %v", err)
}

func TestMatcherActionNames_WalksIPMatcher(t *testing.T) {
	dst := ipInput(t, &network_inputsv3.DestinationIPInput{})
	m := IPRangeTree(dst, []IPRange{
		{Prefixes: []string{"10.96.0.0/12"}, OnMatch: chain(t, "svc")},
		{Prefixes: []string{"10.96.0.6/32"}, OnMatch: portOnly(t, "9000", "pinned")},
	}, chain(t, "outside"))
	assert.Equal(t, []string{"outside", "pinned", "svc"}, MatcherActionNames(m))
}
