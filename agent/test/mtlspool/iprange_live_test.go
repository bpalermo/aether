package mtlspool

// Proposal 044, Phase 0: xdseval's model of the IP-range matcher
// (envoy.matching.custom_matchers.ip_range_matcher, xds.type.matcher.v3.IPMatcher)
// against the pinned proxy, live. A listener with the shape the capture
// listener takes once its HTTP branch has a Service-range on_no_match:
//
//	root: exact map on the destination address (the /32s of the mesh
//	      addresses with a raw-TCP port)
//	  on_no_match: the HTTP branch (transport raw_buffer, then an HTTP
//	               application protocol) -> cap_http
//	    on_no_match: an IPMatcher on the destination address -> svc, ...
//	default chain: passthrough
//
// The range matcher also carries the cases the model must get right beyond
// the plain branch: a /32 inside the range whose nested matcher has no match
// for the dialled port (the proxy moves on to the shorter range), an
// exclusive /24 (tried only as the first candidate), and a second range.
//
// Every chain is a direct_response that writes its own name, and the
// listener binds one port on a distinct loopback address per case, so the
// local address of an accepted socket is the dialled one (no netns, no
// original_dst). For every case, the chain the proxy answers from must be
// the chain xdseval.SelectChain names for the same listener.

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/test/envoybin"
	"aethermesh.dev/agent/test/xdseval"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	tls_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	direct_responsev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/direct_response/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// irCase is one connection: where it goes, whether it speaks HTTP, and the
// chain it must reach.
type irCase struct {
	ip string
	// other: dial the other port, the one the /32s' nested matchers hold.
	other bool
	http  bool
	want  string
	why   string
}

func irCases() []irCase {
	return []irCase{
		{"127.96.0.10", false, false, "mesh_tcp", "a mesh address with a raw-TCP port: the root's /32"},
		{"127.96.0.20", false, true, "cap_http", "a mesh address with HTTP only, over HTTP: the HTTP branch, before the range"},
		{"127.96.0.20", false, false, "svc", "the same address, not HTTP: the range"},
		{"127.96.0.30", false, false, "svc", "a non-mesh address in the range"},
		{"127.96.0.40", true, false, "pinned", "a /32 in the range, declared after it: the longest prefix first"},
		{"127.96.0.40", false, false, "svc", "a /32 in the range whose nested matcher has no match: on to the range"},
		{"127.96.1.5", false, false, "excl", "an exclusive /24 in the range, the first candidate"},
		{"127.96.1.9", true, false, "pinned2", "a /32 inside the exclusive /24: the longest prefix first"},
		{"127.96.1.9", false, false, "svc", "a /32 with no match, then the exclusive /24 skipped, then the range"},
		{"127.200.0.7", false, false, "svc", "the range matcher's second range"},
		{"127.50.0.1", false, false, "passthrough", "outside every range: the default chain"},
	}
}

func (c irCase) port(port, otherPort uint32) uint32 {
	if c.other {
		return otherPort
	}
	return port
}

func irAction(t *testing.T, name string) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{
		Name: name, TypedConfig: fsAny(t, wrapperspb.String(name)),
	}}}
}

func irInput(t *testing.T, name string, m proto.Message) *xdscorev3.TypedExtensionConfig {
	return &xdscorev3.TypedExtensionConfig{Name: name, TypedConfig: fsAny(t, m)}
}

func irExact(t *testing.T, input *xdscorev3.TypedExtensionConfig, m map[string]*xdsmatcherv3.Matcher_OnMatch, onNoMatch *xdsmatcherv3.Matcher_OnMatch) *xdsmatcherv3.Matcher {
	return &xdsmatcherv3.Matcher{
		MatcherType: &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
			Input:    input,
			TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: m}},
		}},
		OnNoMatch: onNoMatch,
	}
}

func irNested(m *xdsmatcherv3.Matcher) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: m}}
}

// irListener binds port on every case's address. otherPort is a port no case
// dials: the nested matchers of the /32s hold only it.
func irListener(t *testing.T, port, otherPort uint32) *listenerv3.Listener {
	t.Helper()
	dst := irInput(t, "envoy.matching.inputs.destination_ip", &network_inputsv3.DestinationIPInput{})
	onlyOther := func(chain string) *xdsmatcherv3.Matcher_OnMatch {
		return irNested(irExact(t, irInput(t, "envoy.matching.inputs.destination_port", &network_inputsv3.DestinationPortInput{}),
			map[string]*xdsmatcherv3.Matcher_OnMatch{strconv.Itoa(int(otherPort)): irAction(t, chain)}, nil))
	}
	rangeBranch := xdseval.IPRangeTree(dst, []xdseval.IPRange{
		// Declared first and the shortest: by declaration order alone it
		// would take every address it holds.
		{Prefixes: []string{"127.96.0.0/12", "127.200.0.0/16"}, OnMatch: irAction(t, "svc")},
		{Prefixes: []string{"127.96.0.40/32"}, OnMatch: onlyOther("pinned")},
		{Prefixes: []string{"127.96.1.0/24"}, OnMatch: irAction(t, "excl"), Exclusive: true},
		{Prefixes: []string{"127.96.1.9/32"}, OnMatch: onlyOther("pinned2")},
	}, nil)
	byApp := map[string]*xdsmatcherv3.Matcher_OnMatch{}
	for _, a := range []string{"'http/1.1'", "'http/1.0'", "'h2c'"} {
		byApp[a] = irAction(t, "cap_http")
	}
	httpBranch := irExact(t, irInput(t, "envoy.matching.inputs.transport_protocol", &network_inputsv3.TransportProtocolInput{}),
		map[string]*xdsmatcherv3.Matcher_OnMatch{
			"raw_buffer": irNested(irExact(t, irInput(t, "envoy.matching.inputs.application_protocol", &network_inputsv3.ApplicationProtocolInput{}), byApp, nil)),
		}, irNested(rangeBranch))
	root := irExact(t, dst, map[string]*xdsmatcherv3.Matcher_OnMatch{"127.96.0.10": irAction(t, "mesh_tcp")}, irNested(httpBranch))

	chain := func(name string) *listenerv3.FilterChain {
		return &listenerv3.FilterChain{Name: name, Filters: []*listenerv3.Filter{{
			Name: "envoy.filters.network.direct_response",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: fsAny(t, &direct_responsev3.Config{
				Response: &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: "chain=" + name + "\n"}},
			})},
		}}}
	}
	l := &listenerv3.Listener{
		Name: "iprange",
		ListenerFilters: []*listenerv3.ListenerFilter{
			{Name: "envoy.filters.listener.http_inspector", ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: fsAny(t, &http_inspectorv3.HttpInspector{})}},
			{Name: "envoy.filters.listener.tls_inspector", ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: fsAny(t, &tls_inspectorv3.TlsInspector{})}},
		},
		// As the capture listener: an inconclusive first write still selects.
		ListenerFiltersTimeout:           durationpb.New(time.Second),
		ContinueOnListenerFiltersTimeout: true,
		FilterChainMatcher:               root,
		DefaultFilterChain:               chain("passthrough"),
	}
	for _, name := range []string{"mesh_tcp", "cap_http", "svc", "pinned", "excl", "pinned2"} {
		l.FilterChains = append(l.FilterChains, chain(name))
	}
	seen := map[string]bool{}
	for _, c := range irCases() {
		at := net.JoinHostPort(c.ip, strconv.Itoa(int(c.port(port, otherPort))))
		if seen[at] {
			continue
		}
		seen[at] = true
		a := socketAddress(c.ip, int(c.port(port, otherPort)))
		if l.Address == nil {
			l.Address = a
			continue
		}
		l.AdditionalAddresses = append(l.AdditionalAddresses, &listenerv3.AdditionalAddress{Address: a})
	}
	return l
}

// irDial connects to ip:port, writes an HTTP/1.1 request or bytes that are
// neither HTTP nor TLS, and returns the chain that answered.
func irDial(ip string, port uint32, http bool) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	if http {
		_, _ = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: a.example\r\n\r\n")
	} else {
		_, _ = c.Write([]byte("\x00\x01binary-not-http-or-tls\r\n"))
	}
	b, _ := io.ReadAll(c)
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "chain=")
}

func TestIPRangeMatcher_XdsevalAgreesWithTheProxy(t *testing.T) {
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	p := ohFreePorts(t)
	port, otherPort := p.a, p.o
	l := irListener(t, port, otherPort)

	cp := startADSControlPlane(t, map[resourcev3.Type][]types.Resource{})
	launchEnvoyOverADS(t, bin, cp)
	p.release()
	snap, err := cachev3.NewSnapshot("v1", map[resourcev3.Type][]types.Resource{resourcev3.ListenerType: {l}})
	require.NoError(t, err)
	require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, snap))
	ppAwaitListening(t, net.JoinHostPort(irCases()[0].ip, strconv.Itoa(int(port))))

	for _, c := range irCases() {
		dialled := c.port(port, otherPort)
		conn := xdseval.Conn{IP: c.ip, Port: dialled, Transport: "raw_buffer"}
		if c.http {
			conn.App = []string{"http/1.1"}
		}
		model, err := xdseval.SelectChain(l, conn)
		require.NoError(t, err)
		got := irDial(c.ip, dialled, c.http)
		t.Logf("%s:%d http=%v: proxy %q, xdseval %q (%s)", c.ip, dialled, c.http, got, model, c.why)
		assert.Equal(t, got, model, "%s:%d http=%v: xdseval disagrees with the pinned proxy (%s)", c.ip, dialled, c.http, c.why)
		assert.Equal(t, c.want, got, "%s:%d http=%v: %s", c.ip, dialled, c.http, c.why)
	}
}
