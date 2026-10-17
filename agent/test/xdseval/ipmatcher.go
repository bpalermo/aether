package xdseval

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"

	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// IPRangeMatcherName is the extension name of the pinned proxy's IP-range
// custom matcher, whose typed config is xds.type.matcher.v3.IPMatcher.
const IPRangeMatcherName = "envoy.matching.custom_matchers.ip_range_matcher"

// evalIPMatcher is a matcher tree whose custom_match is an IPMatcher, as the
// pinned proxy (v1.40.0) runs it: IpRangeMatcher<DataType>::match in
// source/extensions/common/matcher/ip_range_matcher.h (:74-111), built by
// IpRangeMatcherFactoryBase (:122-157).
//
//   - The input's value is parsed as an IP address; a value that is absent or
//     not an address takes the tree's on_no_match (:80-88). Here that is the
//     nil this returns, which EvalMatcher hands to on_no_match.
//   - Every range holding the address is a candidate, one per range (a range
//     matcher with two ranges that both hold it is two candidates), sorted by
//     the range's prefix_len longest first, then by the range matcher's
//     declaration order (IpRangeNodeComparator, :46-56; the sort at :92).
//   - A candidate's on_match is tried in that order; a match ends it, a
//     no-match moves on to the next candidate (:93-109).
//   - An exclusive candidate is tried only when it is the first one (:95-97):
//     once a candidate has not matched, every later exclusive one is skipped.
//   - When none matches, the tree's on_no_match (:110).
//
// The prefix_len the sort uses is the configured value; the range itself is
// the address truncated to it, a length past the family's width meaning the
// whole address (CidrRange::create, source/common/network/cidr_range.cc
// :102-114, :150-176). A range the proxy would refuse is an error here.
func evalIPMatcher(t *xdsmatcherv3.Matcher_MatcherTree, in InputFunc) (*xdscorev3.TypedExtensionConfig, error) {
	ipm, err := ipMatcherOf(t.GetCustomMatch())
	if err != nil {
		return nil, err
	}
	v, ok, err := in(t.GetInput())
	if err != nil || !ok {
		return nil, err
	}
	addr, perr := netip.ParseAddr(v)
	if perr != nil {
		return nil, nil
	}
	cands, err := ipCandidates(ipm, addr)
	if err != nil {
		return nil, err
	}
	first := true
	for _, c := range cands {
		if !first && c.exclusive {
			continue
		}
		a, err := evalOnMatch(c.onMatch, in)
		if err != nil || a != nil {
			return a, err
		}
		first = false
	}
	return nil, nil
}

// ipCandidate is one range holding the address: its range matcher's index,
// the range's configured prefix_len, and the range matcher's exclusive flag
// and on_match (the proxy's IpRangeNode).
type ipCandidate struct {
	index     int
	prefixLen uint32
	exclusive bool
	onMatch   *xdsmatcherv3.Matcher_OnMatch
}

// ipCandidates returns the ranges of ipm holding addr, in the order the
// proxy tries them: the longest prefix_len first, then the range matcher
// declared first.
func ipCandidates(ipm *xdsmatcherv3.IPMatcher, addr netip.Addr) ([]ipCandidate, error) {
	var cands []ipCandidate
	for i, rm := range ipm.GetRangeMatchers() {
		for _, r := range rm.GetRanges() {
			p, err := ipRange(r)
			if err != nil {
				return nil, err
			}
			if p.Contains(addr) {
				cands = append(cands, ipCandidate{index: i, prefixLen: r.GetPrefixLen().GetValue(), exclusive: rm.GetExclusive(), onMatch: rm.GetOnMatch()})
			}
		}
	}
	slices.SortStableFunc(cands, func(a, b ipCandidate) int {
		if c := cmp.Compare(b.prefixLen, a.prefixLen); c != 0 {
			return c
		}
		return cmp.Compare(a.index, b.index)
	})
	return cands, nil
}

// ipMatcherOnMatches returns the on_match of every range matcher of a
// custom_match tree, and false when it is not a decodable IPMatcher.
func ipMatcherOnMatches(cm *xdscorev3.TypedExtensionConfig) ([]*xdsmatcherv3.Matcher_OnMatch, bool) {
	ipm, err := ipMatcherOf(cm)
	if err != nil {
		return nil, false
	}
	out := make([]*xdsmatcherv3.Matcher_OnMatch, 0, len(ipm.GetRangeMatchers()))
	for _, rm := range ipm.GetRangeMatchers() {
		out = append(out, rm.GetOnMatch())
	}
	return out, true
}

// ipMatcherOf decodes a tree's custom_match, which must be an IPMatcher.
func ipMatcherOf(cm *xdscorev3.TypedExtensionConfig) (*xdsmatcherv3.IPMatcher, error) {
	ipm := &xdsmatcherv3.IPMatcher{}
	if !cm.GetTypedConfig().MessageIs(ipm) {
		return nil, Unsupported{"a custom_match tree of " + cm.GetTypedConfig().GetTypeUrl()}
	}
	if err := cm.GetTypedConfig().UnmarshalTo(ipm); err != nil {
		return nil, fmt.Errorf("custom_match %q: %w", cm.GetName(), err)
	}
	return ipm, nil
}

// ipRange is the range a CidrRange holds, as the proxy truncates it.
func ipRange(r *xdscorev3.CidrRange) (netip.Prefix, error) {
	a, err := netip.ParseAddr(r.GetAddressPrefix())
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("xdseval: IPMatcher range %q: malformed IP address", r.GetAddressPrefix())
	}
	if r.GetPrefixLen().GetValue() > 128 {
		return netip.Prefix{}, fmt.Errorf("xdseval: IPMatcher range %s/%d: prefix_len over 128", r.GetAddressPrefix(), r.GetPrefixLen().GetValue())
	}
	return netip.PrefixFrom(a, min(int(r.GetPrefixLen().GetValue()), a.BitLen())).Masked(), nil
}

// IPRange is one range matcher of an IPMatcher: its prefixes ("10.96.0.0/12"),
// what a match of them does, and whether it is exclusive.
type IPRange struct {
	Prefixes  []string
	OnMatch   *xdsmatcherv3.Matcher_OnMatch
	Exclusive bool
}

// IPRangeTree builds a matcher whose tree is an IPMatcher over input, the
// range matchers in the order given, with onNoMatch (which may be nil). It
// panics on a prefix that does not parse: it builds test fixtures.
func IPRangeTree(input *xdscorev3.TypedExtensionConfig, ranges []IPRange, onNoMatch *xdsmatcherv3.Matcher_OnMatch) *xdsmatcherv3.Matcher {
	ipm := &xdsmatcherv3.IPMatcher{}
	for _, r := range ranges {
		rm := &xdsmatcherv3.IPMatcher_IPRangeMatcher{OnMatch: r.OnMatch, Exclusive: r.Exclusive}
		for _, s := range r.Prefixes {
			p := netip.MustParsePrefix(s)
			rm.Ranges = append(rm.Ranges, &xdscorev3.CidrRange{AddressPrefix: p.Addr().String(), PrefixLen: wrapperspb.UInt32(uint32(p.Bits()))})
		}
		ipm.RangeMatchers = append(ipm.RangeMatchers, rm)
	}
	cfg, err := anypb.New(ipm)
	if err != nil {
		panic(err)
	}
	return &xdsmatcherv3.Matcher{
		MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
			Input:    input,
			TreeType: &xdsmatcherv3.Matcher_MatcherTree_CustomMatch{CustomMatch: &xdscorev3.TypedExtensionConfig{Name: IPRangeMatcherName, TypedConfig: cfg}},
		}},
		OnNoMatch: onNoMatch,
	}
}
