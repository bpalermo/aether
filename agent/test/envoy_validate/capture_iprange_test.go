package envoy_validate

import (
	"net/netip"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/xdseval"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Proposal 044, Phase 0: the capture listener's HTTP branch gains an
// on_no_match that is an IP-range matcher
// (envoy.matching.custom_matchers.ip_range_matcher, xds.type.matcher.v3.IPMatcher)
// on the destination address over the cluster's Service ranges, to a chain
// of its own; anything else still falls to the default chain. Nothing
// agent generates that branch itself now (WithServiceRanges): the first test
// below adds it by hand to a listener built without a range, the second
// takes the agent's own. Both hold that the pinned proxy accepts it and that
// xdseval reads it as intended.

const ipRangeSvcChain = "cap_passthrough_svc"

// withServiceRangeBranch adds the range branch to the agent's capture
// listener in data, with ranges, and a copy of the default chain named
// ipRangeSvcChain for it.
func withServiceRangeBranch(t *testing.T, data []byte, ranges ...string) ([]byte, *listenerv3.Listener) {
	t.Helper()
	bs := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(data, bs))
	var capture *listenerv3.Listener
	for _, l := range bs.GetStaticResources().GetListeners() {
		if strings.HasPrefix(l.GetName(), "capture_") {
			require.Nil(t, capture, "one capture listener in the fixture")
			capture = l
		}
	}
	require.NotNil(t, capture)
	root := capture.GetFilterChainMatcher()
	dst := root.GetMatcherTree().GetInput()
	require.Equal(t, "envoy.matching.inputs.destination_ip", dst.GetName(), "the root branches on the destination address")
	httpBranch := root.GetOnNoMatch().GetMatcher()
	require.NotNil(t, httpBranch, "the root's on_no_match is the HTTP branch")
	require.Nil(t, httpBranch.GetOnNoMatch(), "the HTTP branch has no on_no_match today")
	httpBranch.OnNoMatch = &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: xdseval.IPRangeTree(dst, []xdseval.IPRange{{
		Prefixes: ranges,
		OnMatch: &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{
			Name: ipRangeSvcChain, TypedConfig: mustAny(wrapperspb.String(ipRangeSvcChain)),
		}}},
	}}, nil)}}

	svc := proto.Clone(capture.GetDefaultFilterChain()).(*listenerv3.FilterChain)
	require.Equal(t, proxy.PassthroughClusterName, tcpProxyOf(t, svc).GetCluster(), "the default chain is the passthrough")
	svc.Name = ipRangeSvcChain
	tp := tcpProxyOf(t, svc)
	tp.StatPrefix = ipRangeSvcChain
	svc.GetFilters()[len(svc.GetFilters())-1].ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(tp)}
	capture.FilterChains = append(capture.FilterChains, svc)

	out, err := marshalBootstrap(bs)
	require.NoError(t, err)
	return out, capture
}

func tcpProxyOf(t *testing.T, fc *listenerv3.FilterChain) *tcp_proxyv3.TcpProxy {
	t.Helper()
	require.NotEmpty(t, fc.GetFilters())
	tp := &tcp_proxyv3.TcpProxy{}
	require.NoError(t, fc.GetFilters()[len(fc.GetFilters())-1].GetTypedConfig().UnmarshalTo(tp), "chain %s ends in tcp_proxy", fc.GetName())
	return tp
}

func TestCaptureListenerServiceRangeBranchValidates(t *testing.T) {
	base := agentCaptureBootstrap(t)
	data, l := withServiceRangeBranch(t, base, "10.96.0.0/12")
	out, err := runValidate(t, data)
	t.Logf("envoy --mode validate:\n%s", out)
	require.NoError(t, err, "the pinned proxy refuses the capture listener with the Service-range branch")

	before := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(base, before))
	var unchanged *listenerv3.Listener
	for _, bl := range before.GetStaticResources().GetListeners() {
		if bl.GetName() == l.GetName() {
			unchanged = bl
		}
	}
	require.NotNil(t, unchanged)

	assert.Contains(t, xdseval.MatcherActionNames(l.GetFilterChainMatcher()), ipRangeSvcChain)
	raw := func(ip string, port uint32) xdseval.Conn {
		return xdseval.Conn{IP: ip, Port: port, Transport: "raw_buffer"}
	}
	http := func(ip string, port uint32) xdseval.Conn {
		return xdseval.Conn{IP: ip, Port: port, Transport: "raw_buffer", App: []string{"http/1.1"}}
	}
	for _, tc := range []struct {
		c          xdseval.Conn
		want, then string
	}{
		// A non-mesh address in the range: the new chain, not the passthrough.
		{raw("10.96.200.1", 5432), ipRangeSvcChain, "cap_passthrough"},
		// A mesh service with HTTP only (multi, 10.96.1.1) is in the range
		// and has no entry in the root: HTTP still reaches cap_http, the
		// hazard the branch's place after the HTTP branch avoids.
		{http("10.96.1.1", 8080), proxy.CaptureHTTPChainName, proxy.CaptureHTTPChainName},
		{raw("10.96.1.1", 8080), ipRangeSvcChain, "cap_passthrough"},
		// Outside the range: the passthrough, as before.
		{raw("192.0.2.1", 443), "cap_passthrough", "cap_passthrough"},
		{http("192.0.2.1", 80), proxy.CaptureHTTPChainName, proxy.CaptureHTTPChainName},
	} {
		got, err := xdseval.SelectChain(l, tc.c)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%+v", tc.c)
		was, err := xdseval.SelectChain(unchanged, tc.c)
		require.NoError(t, err)
		assert.Equal(t, tc.then, was, "%+v without the branch", tc.c)
	}
	// A mesh service with a raw-TCP port (mixed, 10.96.1.3:9000) keeps its
	// own chain: the root's /32 entry comes first.
	got, err := xdseval.SelectChain(l, raw("10.96.1.3", 9000))
	require.NoError(t, err)
	was, err := xdseval.SelectChain(unchanged, raw("10.96.1.3", 9000))
	require.NoError(t, err)
	assert.Equal(t, was, got)
	assert.NotEqual(t, ipRangeSvcChain, got)
	assert.NotEqual(t, "cap_passthrough", got)

	// Control: the proxy does read the matcher. A range it cannot parse is
	// refused.
	bad, _ := withServiceRangeBranch(t, base, "10.96.0.0/12")
	require.Equal(t, 1, strings.Count(string(bad), `"10.96.0.0"`), "the range is in the bootstrap once")
	bad = []byte(strings.Replace(string(bad), `"10.96.0.0"`, `"10.96.0.300"`, 1))
	out, err = runValidate(t, bad)
	require.Error(t, err, "the proxy accepted a malformed range: it did not parse the matcher:\n%s", out)
	assert.Contains(t, out, "10.96.0.300")
}

// TestCaptureListenerServiceRangeGenerated: the capture listener the agent
// builds with the Service ranges set (a CIDR and a /32 outside it, a
// ClusterSetIP say) passes the pinned proxy's validation, and its matching
// tree keeps the hazards of the branch's place.
func TestCaptureListenerServiceRangeGenerated(t *testing.T) {
	data := agentCaptureBootstrap(t, netip.MustParsePrefix("10.96.0.0/12"), netip.MustParsePrefix("100.64.5.5/32"))
	out, err := runValidate(t, data)
	t.Logf("envoy --mode validate:\n%s", out)
	require.NoError(t, err, "the pinned proxy refuses the agent's capture listener with the Service-range branch")

	bs := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(data, bs))
	var l *listenerv3.Listener
	for _, bl := range bs.GetStaticResources().GetListeners() {
		if strings.HasPrefix(bl.GetName(), "capture_") {
			l = bl
		}
	}
	require.NotNil(t, l)
	svc := proxy.CapturePassthroughServiceChainName
	var fc *listenerv3.FilterChain
	for _, c := range l.GetFilterChains() {
		if c.GetName() == svc {
			fc = c
		}
	}
	require.NotNil(t, fc, "the listener carries %s", svc)
	tp := tcpProxyOf(t, fc)
	assert.Equal(t, proxy.PassthroughClusterName, tp.GetCluster(), "the same cluster as the default chain")
	assert.Equal(t, svc, tp.GetStatPrefix())

	raw := func(ip string, port uint32) xdseval.Conn {
		return xdseval.Conn{IP: ip, Port: port, Transport: "raw_buffer"}
	}
	http := func(ip string, port uint32) xdseval.Conn {
		return xdseval.Conn{IP: ip, Port: port, Transport: "raw_buffer", App: []string{"http/1.1"}}
	}
	for _, tc := range []struct {
		c    xdseval.Conn
		want string
		why  string
	}{
		{http("10.96.1.1", 8080), proxy.CaptureHTTPChainName, "a mesh address that serves HTTP only (multi), over cleartext HTTP"},
		{raw("10.96.1.1", 8080), svc, "the same address, not HTTP"},
		{xdseval.Conn{IP: "10.96.1.1", Port: 443, Transport: "tls", SNI: "example.com"}, svc, "the same address, TLS"},
		{raw("10.96.200.1", 5432), svc, "a non-mesh address in the range"},
		{raw("100.64.5.5", 5432), svc, "a /32 outside the CIDR"},
		{raw("100.64.5.6", 5432), "cap_passthrough", "next to the /32: outside every range"},
		{raw("192.0.2.1", 443), "cap_passthrough", "an external address"},
		{http("192.0.2.1", 80), proxy.CaptureHTTPChainName, "HTTP to an external address"},
	} {
		got, err := xdseval.SelectChain(l, tc.c)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%+v: %s", tc.c, tc.why)
	}
	// A raw-TCP mesh address (mixed, 10.96.1.3:9000) keeps its own chain.
	got, err := xdseval.SelectChain(l, raw("10.96.1.3", 9000))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "cap_tcp_"), "mixed:9000 selected %q", got)
}
