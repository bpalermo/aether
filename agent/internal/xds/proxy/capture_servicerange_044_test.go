package proxy

import (
	"net/netip"
	"testing"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/test/xdseval"
	meshconst "aethermesh.dev/common/constants/mesh"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Proposal 044, Phase 0: the capture listener's HTTP branch hands what it
// does not take to an IP-range matcher over the cluster's Service ranges,
// whose one chain, cap_passthrough_svc, is the passthrough under its own stat
// prefix. These tests hold where the branch sits (after the HTTP branch, so
// a mesh address that serves HTTP only keeps its HTTP), that every address
// outside the ranges still takes the default chain, and that no range means
// exactly the listener built before the branch existed.

// srRanges is a Service CIDR and a single address outside it (a ClusterSetIP
// of another cluster, say).
var srRanges = []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12"), netip.MustParsePrefix("100.64.5.5/32")}

// srServices are a raw-TCP service and a service with a TLSRoute and HTTP
// ports, both inside the range. A service that serves HTTP only has no
// capture service at all: 10.96.1.1 below is one.
func srServices() []CaptureTCPService {
	return []CaptureTCPService{
		{ClusterName: TCPClusterName("demo/tcponly", "aether.internal"), ClusterIP: "10.96.0.7", LegacyTCPPort: 9000},
		{
			ClusterName: TCPClusterName("demo/tlsr", "aether.internal"), ClusterIP: "10.96.0.8", HTTPPorts: []uint32{8080},
			TLSRouteRules: []L4ServiceRoute{{
				SNIHostnames: []string{"tls.example.com"},
				Backends:     []L4Backend{{Service: "demo/tls-a", Cluster: TCPClusterName("demo/tls-a", "aether.internal"), Weight: 1}},
			}},
		},
	}
}

func srListener(t *testing.T, svcs []CaptureTCPService, withPassthrough bool, opts ...CaptureListenerOption) *listenerv3.Listener {
	t.Helper()
	l, err := GenerateCaptureListener(perPortPod(), perPortSource, meshconst.ProxyCapturePort, "aether.internal", false, svcs, withPassthrough, nil, opts...)
	require.NoError(t, err)
	requireMatcherNamesChains(t, l)
	return l
}

// TestCaptureServiceRange_NoRangeIsTodaysListener: without a range (no
// option, a nil one, an empty one) the listener is the one built before the
// branch existed, byte for byte; so is a scoped listener given a range, which
// has no passthrough to split.
func TestCaptureServiceRange_NoRangeIsTodaysListener(t *testing.T) {
	for _, redirectAll := range []bool{true, false} {
		for _, svcs := range [][]CaptureTCPService{nil, srServices()} {
			base := srListener(t, svcs, redirectAll)
			for _, opt := range []CaptureListenerOption{WithServiceRanges(nil), WithServiceRanges([]netip.Prefix{})} {
				assert.True(t, proto.Equal(base, srListener(t, svcs, redirectAll, opt)), "redirectAll=%v: an empty range changed the listener", redirectAll)
			}
			assert.NotContains(t, chainNamesOf(base), CapturePassthroughServiceChainName)
			if redirectAll {
				assert.Nil(t, captureHTTPBranchOf(base).GetOnNoMatch(), "no range: the HTTP branch has no on_no_match")
			}
		}
		if !redirectAll {
			assert.True(t, proto.Equal(srListener(t, srServices(), false), srListener(t, srServices(), false, WithServiceRanges(srRanges))),
				"scoped capture has no passthrough: a range must not change it")
		}
	}
}

// TestCaptureServiceRange_Selection is the hazard table of the branch.
func TestCaptureServiceRange_Selection(t *testing.T) {
	l := srListener(t, srServices(), true, WithServiceRanges(srRanges))
	without := srListener(t, srServices(), true)
	const svc = CapturePassthroughServiceChainName
	tcpOwn := "cap_tcp_" + TCPClusterName("demo/tcponly", "aether.internal") + "_9000"
	for _, tc := range []struct {
		c          xdseval.Conn
		want, then string
		why        string
	}{
		{httpConn("10.96.1.1", 8080), CaptureHTTPChainName, CaptureHTTPChainName, "a mesh address that serves HTTP only, over HTTP: cap_http, not the range"},
		{httpConn("10.96.1.1", meshconst.ProxyOutboundPort), CaptureHTTPChainName, CaptureHTTPChainName, "the same on the proxy port"},
		{rawConn("10.96.1.1", 8080), svc, "cap_passthrough", "the same address, not HTTP: the range"},
		{tlsConn("10.96.1.1", 443, "example.com"), svc, "cap_passthrough", "the same address, TLS: the range"},
		{rawConn("10.96.0.7", 9000), tcpOwn, tcpOwn, "a raw-TCP mesh address on its port: its own chain"},
		{httpConn("10.96.0.7", 9000), tcpOwn, tcpOwn, "the same, whatever the bytes look like"},
		{rawConn("10.96.0.7", 5555), svc, "cap_passthrough", "a raw-TCP mesh address on a port it does not declare: back from its branch, then the range"},
		{tlsConn("10.96.0.8", 443, "tls.example.com"), "", "", "a TLSRoute keeps its chain (checked below)"},
		{tlsConn("10.96.0.8", 8080, "tls.example.com"), svc, "cap_passthrough", "TLS to the service's own HTTP port: passed through, now counted as the range"},
		{rawConn("10.96.200.1", 5432), svc, "cap_passthrough", "a non-mesh address in the range"},
		{httpConn("10.96.200.1", 80), CaptureHTTPChainName, CaptureHTTPChainName, "HTTP to a non-mesh address in the range: still cap_http"},
		{rawConn("100.64.5.5", 5432), svc, "cap_passthrough", "a /32 outside the CIDR: the range"},
		{rawConn("100.64.5.6", 5432), "cap_passthrough", "cap_passthrough", "the address next to that /32: outside"},
		{rawConn("192.0.2.1", 443), "cap_passthrough", "cap_passthrough", "an external address: the passthrough"},
		{tlsConn("192.0.2.1", 443, "example.com"), "cap_passthrough", "cap_passthrough", "external TLS: the passthrough"},
		{rawConn("10.112.0.1", 80), "cap_passthrough", "cap_passthrough", "the first address past the /12"},
		{rawConn("10.95.255.255", 80), "cap_passthrough", "cap_passthrough", "the last address before the /12"},
	} {
		got := sel(t, l, tc.c)
		was := sel(t, without, tc.c)
		if tc.want == "" {
			assert.Equal(t, was, got, "%s", tc.why)
			assert.Contains(t, got, "cap_tls_", "%s", tc.why)
			continue
		}
		assert.Equal(t, tc.want, got, "%+v: %s", tc.c, tc.why)
		assert.Equal(t, tc.then, was, "%+v without the range: %s", tc.c, tc.why)
	}
}

// TestCaptureServiceRange_Chain: cap_passthrough_svc is the passthrough under
// its own name and stat prefix, the same cluster, and the default chain is
// unchanged.
func TestCaptureServiceRange_Chain(t *testing.T) {
	l := srListener(t, srServices(), true, WithServiceRanges(srRanges))
	fc := chainByName(l.GetFilterChains(), CapturePassthroughServiceChainName)
	require.NotNil(t, fc)
	require.Len(t, fc.GetFilters(), 1)
	tp := chainTCPProxy(fc)
	require.NotNil(t, tp)
	assert.Equal(t, PassthroughClusterName, tp.GetCluster())
	assert.Equal(t, CapturePassthroughServiceChainName, tp.GetStatPrefix())

	want := BuildCapturePassthroughFilterChain()
	assert.True(t, proto.Equal(want, l.GetDefaultFilterChain()), "the default chain is the passthrough, unchanged")
	twin := proto.Clone(want).(*listenerv3.FilterChain)
	twin.Name = CapturePassthroughServiceChainName
	twinTP := chainTCPProxy(twin)
	twinTP.StatPrefix = CapturePassthroughServiceChainName
	twin.Filters[0].ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(twinTP)}
	assert.True(t, proto.Equal(twin, fc), "cap_passthrough_svc is cap_passthrough with its own name and stat prefix")

	// One range matcher, every prefix, in the order given.
	rb := captureHTTPBranchOf(l).GetOnNoMatch().GetMatcher()
	require.NotNil(t, rb)
	assert.Equal(t, captureIPRangeMatcherName, rb.GetMatcherTree().GetCustomMatch().GetName())
	assert.Equal(t, captureMatchDestinationIP, rb.GetMatcherTree().GetInput().GetName())
	assert.Nil(t, rb.GetOnNoMatch(), "an address in no range goes back to the default chain")
	assert.Equal(t, []string{CapturePassthroughServiceChainName}, xdseval.MatcherActionNames(rb))
}

// TestCaptureServiceRange_ListenerSize: what one range costs each pod's
// capture listener. The branch is one matcher and one chain whatever the
// number of Services, so the cost is a constant per range; the bound is
// generous and only catches a branch that grows with something else.
func TestCaptureServiceRange_ListenerSize(t *testing.T) {
	one := []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}
	for _, svcs := range [][]CaptureTCPService{nil, srServices()} {
		before := proto.Size(srListener(t, svcs, true))
		after := proto.Size(srListener(t, svcs, true, WithServiceRanges(one)))
		t.Logf("capture listener, %d capture services: %d bytes, %d with one /12 range (+%d)", len(svcs), before, after, after-before)
		assert.Less(t, after-before, 1024)
	}
}

func chainNamesOf(l *listenerv3.Listener) []string {
	out := make([]string, 0, len(l.GetFilterChains()))
	for _, fc := range l.GetFilterChains() {
		out = append(out, fc.GetName())
	}
	return out
}

// captureHTTPBranchOf returns the listener's HTTP branch: the root's
// on_no_match when the root branches on the destination address, else the
// root itself (no address has a branch).
func captureHTTPBranchOf(l *listenerv3.Listener) *xdsmatcherv3.Matcher {
	root := l.GetFilterChainMatcher()
	if root.GetMatcherTree().GetInput().GetName() == captureMatchDestinationIP {
		return root.GetOnNoMatch().GetMatcher()
	}
	return root
}
