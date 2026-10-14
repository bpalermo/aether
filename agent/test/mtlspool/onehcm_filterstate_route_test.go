package mtlspool

// A route's own RouteMatch.filter_state reads the CONNECTION's filter state,
// which the capture chain's network set_filter_state writes
// (proxy.BuildDialledPortFilterState). The one-HCM capture table selects a
// port's routes that way, with no matcher on the virtual host.
//
// Why it works, at the pinned Envoy (189f198):
//   - RouteEntryImplBase::matchRoute evaluates route.match().filter_state()
//     against stream_info.filterState() (source/common/router/config_impl.cc,
//     the filter_state_ loop at 944-950);
//   - FilterStateMatcher::match looks the key up with getDataReadOnlyGeneric
//     (source/common/common/matchers.cc:211-217);
//   - a stream's FilterStateImpl falls back to its parent, the connection's,
//     for a key it does not hold (source/common/stream_info/filter_state_impl.cc:
//     114-125).
//
// This holds it live on the pinned proxy, with a red control: the same
// listener without the network filter answers every request from the
// catch-all route.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func fsDirect(status uint32, body string) *routev3.Route_DirectResponse {
	return &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
		Status: status, Body: &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: body}},
	}}
}

// fsListener serves, on ports, one route per port selected by the route's
// filter_state match on the dialled port, and a catch-all after them.
func fsListener(t *testing.T, ports []uint32, withFilterState bool) *listenerv3.Listener {
	t.Helper()
	var routes []*routev3.Route
	for _, p := range ports {
		routes = append(routes, &routev3.Route{
			Match: &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
				FilterState: []*matcherv3.FilterStateMatcher{{
					Key: proxy.DialledPortFilterStateKey,
					Matcher: &matcherv3.FilterStateMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{Exact: strconv.Itoa(int(p))},
					}},
				}},
			},
			Action: fsDirect(200, fmt.Sprintf("dialled=%d", p)),
		})
	}
	routes = append(routes, &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: fsDirect(421, "no-dialled-port"),
	})
	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: "fs",
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name:         "fs",
			VirtualHosts: []*routev3.VirtualHost{{Name: "all", Domains: []string{"*"}, Routes: routes}},
		}},
		HttpFilters: []*http_connection_managerv3.HttpFilter{{Name: "envoy.filters.http.router", ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{TypedConfig: fsAny(t, &routerv3.Router{})}}},
	}
	var filters []*listenerv3.Filter
	if withFilterState {
		filters = append(filters, proxy.BuildDialledPortFilterState())
	}
	filters = append(filters, &listenerv3.Filter{Name: "envoy.filters.network.http_connection_manager", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: fsAny(t, hcm)}})
	l := &listenerv3.Listener{
		Name:         "fs",
		Address:      socketAddress("127.0.0.1", int(ports[0])),
		FilterChains: []*listenerv3.FilterChain{{Name: "fs", Filters: filters}},
	}
	for _, p := range ports[1:] {
		l.AdditionalAddresses = append(l.AdditionalAddresses, &listenerv3.AdditionalAddress{Address: socketAddress("127.0.0.1", int(p))})
	}
	return l
}

func fsAny(t *testing.T, m proto.Message) *anypb.Any {
	t.Helper()
	a, err := anypb.New(m)
	require.NoError(t, err)
	return a
}

func fsGet(port uint32) (int, string) {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestOneHCM_RouteFilterStateSeesTheConnectionsDialledPort(t *testing.T) {
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	for _, withFilterState := range []bool{true, false} {
		t.Run(fmt.Sprintf("filter_state=%v", withFilterState), func(t *testing.T) {
			p := ohFreePorts(t)
			ports := []uint32{p.a, p.a2, p.c}
			cp := startADSControlPlane(t, map[resourcev3.Type][]types.Resource{})
			launchEnvoyOverADS(t, bin, cp)
			p.release()
			snap, err := cachev3.NewSnapshot("v1", map[resourcev3.Type][]types.Resource{
				resourcev3.ListenerType: {fsListener(t, ports, withFilterState)},
			})
			require.NoError(t, err)
			require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, snap))
			for _, port := range ports {
				ppAwaitListening(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
			}
			for _, port := range ports {
				status, body := fsGet(port)
				t.Logf("dialled %d -> %d %q", port, status, body)
				if withFilterState {
					assert.Equal(t, 200, status, "dialled %d", port)
					assert.Equal(t, fmt.Sprintf("dialled=%d", port), body, "the route of the port that was dialled, and no other")
				} else {
					assert.Equal(t, 421, status, "without the connection's filter state no port's route matches")
					assert.Equal(t, "no-dialled-port", body)
				}
			}
		})
	}
}
