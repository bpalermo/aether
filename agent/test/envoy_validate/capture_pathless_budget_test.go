package envoy_validate

import (
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	meshconst "aethermesh.dev/common/constants/mesh"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The conditions MergeCaptureRouteTables adds so that a request with no path
// never takes another answer's route (chart 3.0.3) are regexes the proxy
// compiles with RE2 and refuses past a program size of 100, and a refused
// regex rejects the WHOLE cap_http. These tests put the shapes that grow with
// the number of a service's HTTP ports in front of the pinned proxy.

// pathlessBudgetPorts returns n HTTP port numbers that share few digits.
func pathlessBudgetPorts(n int) []int {
	out := make([]int, 0, n)
	for i := range n {
		out = append(out, 1000+i*1373)
	}
	return out
}

// pathlessConnectRule is a CONNECT matcher route: the one route that can
// take a request with no path (no route the agent builds is one today).
func pathlessConnectRule(cluster string) *routev3.Route {
	return &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_ConnectMatcher_{ConnectMatcher: &routev3.RouteMatch_ConnectMatcher{}}},
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}}},
	}
}

// validateTable runs the pinned proxy in validate mode over rc served inline.
func validateTable(t *testing.T, rc *routev3.RouteConfiguration) (string, error) {
	t.Helper()
	rc = proto.Clone(rc).(*routev3.RouteConfiguration)
	rc.ValidateClusters = wrapperspb.Bool(false)
	bs := &bootstrapv3.Bootstrap{StaticResources: &bootstrapv3.Bootstrap_StaticResources{
		Listeners: []*listenerv3.Listener{inlineTableListener(rc, 15999)},
	}}
	data, err := protojson.Marshal(bs)
	require.NoError(t, err)
	return runValidate(t, data)
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestCaptureTable_InvertedSuffixConditionsFitTheProxysBudget: one service
// with n HTTP ports in the proxy port's shape (the port in Host decides),
// each port's virtual host with a CONNECT route, so the default port answer
// is conditioned on the Host's suffix being none of the other answers'. One
// inverted regex over every other answer's suffixes was refused by the proxy
// from about two dozen ports (adversarial review of the change); one inverted
// condition per answer stays within the budget at any count.
func TestCaptureTable_InvertedSuffixConditionsFitTheProxysBudget(t *testing.T) {
	const mesh = "a.ns.mesh"
	for _, n := range []int{2, 8, 24, 32} {
		var vhs []*routev3.VirtualHost
		for i, p := range pathlessBudgetPorts(n) {
			doms := []string{mesh + ":" + strconv.Itoa(p)}
			if i == 0 {
				doms = append(doms, mesh)
			}
			vh := proxy.BuildServiceVirtualHost(mesh+"|"+strconv.Itoa(p), mesh+":"+strconv.Itoa(p), doms, nil)
			vh.Routes = append([]*routev3.Route{pathlessConnectRule("by-connect")}, vh.Routes...)
			vhs = append(vhs, vh)
		}
		perPort := map[uint32]*routev3.RouteConfiguration{
			meshconst.ProxyOutboundPort: proxy.BuildProxyPortCaptureRouteConfiguration(proxy.CaptureHTTPRouteName(meshconst.ProxyOutboundPort), vhs, "mesh", true, []string{mesh}),
		}
		other := proxy.BuildCaptureRouteConfiguration(proxy.CaptureHTTPOtherRouteName, nil, proxy.BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
		merged, err := proxy.MergeCaptureRouteTables(proxy.CaptureHTTPChainName, perPort, other)
		require.NoError(t, err)
		inverted := 0
		for _, vh := range merged.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				for _, h := range r.GetMatch().GetHeaders() {
					if h.GetInvertMatch() && h.GetName() == ":authority" {
						inverted++
					}
				}
			}
		}
		require.Positive(t, inverted, "ports=%d: the default answer carries inverted suffix conditions", n)
		if out, err := validateTable(t, merged); err != nil {
			t.Errorf("ports=%d: the pinned proxy refuses the compiled cap_http:\n%s", n, lastLines(out, 4))
		}
	}
}

// TestCaptureTable_DialledPortComplementFitsTheProxysBudget: a service
// declared on n dialled ports whose tables answer it with "/"-prefixed routes
// only, while cap_http_other answers it with a CONNECT route. The other ports'
// block then takes a request with no path only when its dialled port is none
// of the n, by one small regex per piece of that complement.
func TestCaptureTable_DialledPortComplementFitsTheProxysBudget(t *testing.T) {
	const mesh = "a.ns.mesh"
	for _, n := range []int{1, 8, 24, 32} {
		perPort := map[uint32]*routev3.RouteConfiguration{}
		for _, p := range pathlessBudgetPorts(n) {
			vh := proxy.BuildServiceVirtualHost(mesh+"|"+strconv.Itoa(p), mesh+":"+strconv.Itoa(p), []string{mesh, mesh + ":" + strconv.Itoa(p)}, nil)
			perPort[uint32(p)] = proxy.BuildCaptureRouteConfiguration(proxy.CaptureHTTPRouteName(uint32(p)), []*routev3.VirtualHost{vh}, proxy.BuildCaptureCatchAllVirtualHost("mesh", uint32(p), false, true), nil)
		}
		otherVH := proxy.BuildServiceVirtualHost(mesh+"|other", mesh, []string{mesh, mesh + ":*"}, nil)
		otherVH.Routes = append([]*routev3.Route{pathlessConnectRule("other-by-connect")}, otherVH.Routes...)
		other := proxy.BuildCaptureRouteConfiguration(proxy.CaptureHTTPOtherRouteName, []*routev3.VirtualHost{otherVH}, proxy.BuildCaptureCatchAllVirtualHost("mesh", 0, false, true), nil)
		merged, err := proxy.MergeCaptureRouteTables(proxy.CaptureHTTPChainName, perPort, other)
		require.NoError(t, err)
		pieces := 0
		for _, vh := range merged.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				for _, fs := range r.GetMatch().GetFilterState() {
					if fs.GetStringMatch().GetSafeRegex() != nil {
						pieces++
					}
				}
			}
		}
		require.Positive(t, pieces, "ports=%d: the other ports' CONNECT route is conditioned on the dialled port", n)
		if out, err := validateTable(t, merged); err != nil {
			t.Errorf("ports=%d: the pinned proxy refuses the compiled cap_http:\n%s", n, lastLines(out, 4))
		}
	}
}
