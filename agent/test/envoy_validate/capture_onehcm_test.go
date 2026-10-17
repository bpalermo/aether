package envoy_validate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Gate (c) of the one-HCM capture listener: the capture listener and the
// cap_http table the AGENT generates (agent/internal/xds/cache, from a
// registry listing and the mesh Services, not hand-written) are accepted by
// the pinned proxy's `envoy --mode validate`. The listener selects its chains
// with filter_chain_matcher; the table branches on the dialled port through a
// virtual-host matcher with FilterStateInput and :authority inputs.
//
// The listener's chains fetch their tables over RDS, which validate mode does
// not resolve, so each table is also served inline by a loopback listener of
// its own: the proxy then parses the compiled table itself.

type ohvRegistry struct{ services []ohvSvc }

type ohvSvc struct {
	ns, name, ip string
	legacy       uint32
	ports        map[uint32]registryv1.PortProtocol
}

func (r *ohvRegistry) Initialize(context.Context) error { return nil }
func (r *ohvRegistry) Close() error                     { return nil }
func (r *ohvRegistry) RegisterEndpoint(context.Context, string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint) error {
	return nil
}
func (r *ohvRegistry) UnregisterEndpoint(context.Context, string, string) error    { return nil }
func (r *ohvRegistry) UnregisterEndpoints(context.Context, string, []string) error { return nil }
func (r *ohvRegistry) ListEndpoints(context.Context, string, registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func (r *ohvRegistry) ListAllEndpoints(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	out := map[string][]*registryv1.ServiceEndpoint{}
	for _, s := range r.services {
		ep := &registryv1.ServiceEndpoint{
			Ip: s.ip, ClusterName: "cluster-1", Port: s.legacy, Weight: 100, PortProtocols: s.ports,
			ContainerMetadata:  &registryv1.ServiceEndpoint_ContainerMetadata{ContainerId: "c", NetworkNamespace: "/proc/1/ns/net"},
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: s.ns, PodName: s.name + "-0", NodeName: "node-2"},
		}
		serves := false
		for port, class := range s.ports {
			ep.Ports = append(ep.Ports, port)
			serves = serves || registryv1.Service_Protocol(class) == protocol
		}
		slices.Sort(ep.Ports)
		if serves {
			key := s.ns + "/" + s.name
			out[key] = append(out[key], ep)
		}
	}
	return out, nil
}

// agentCaptureBootstrap builds the bootstrap of gate (c) from the cache.
func agentCaptureBootstrap(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	services := []ohvSvc{
		{ns: "demo", name: "multi", ip: "10.0.1.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}},
		{ns: "demo", name: "eighty", ip: "10.0.1.3", legacy: 80, ports: map[uint32]registryv1.PortProtocol{80: h, 8080: h}},
		{ns: "demo", name: "mixed", ip: "10.0.1.4", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}},
		{ns: "demo", name: "tcponly", ip: "10.0.1.7", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp}},
		{ns: "other", name: "multi", ip: "10.0.2.2", legacy: 7070, ports: map[uint32]registryv1.PortProtocol{7070: h}},
	}
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: "demo", ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, meshDomain))
	require.NoError(t, c.SetNodeIdentity(ctx, "spiffe://"+trustDomain+"/ns/aether-system/sa/aether-agent"))
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	for i, s := range services {
		key := s.ns + "/" + s.name
		authorities[key] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
		vips = append(vips, capture.CaptureTCPService{ServiceName: key, ClusterIP: fmt.Sprintf("10.96.1.%d", i+1)})
	}
	c.SetCaptureAuthorities(authorities)
	c.SetCaptureTCPServices(vips)
	c.SetTLSServiceRoutes(map[string][]proxy.L4ServiceRoute{
		"demo/mixed":   {{SNIHostnames: []string{"mixed.example", "*.wild.example"}, Backends: []proxy.L4Backend{{Service: "demo/tcponly", Cluster: proxy.TCPClusterName("demo/tcponly", meshDomain), Weight: 1}}}},
		"demo/tcponly": {{SNIHostnames: []string{"tcponly.example"}, Backends: []proxy.L4Backend{{Service: "demo/tcponly", Cluster: proxy.TCPClusterName("demo/tcponly", meshDomain), Weight: 1}}}},
	})
	// A service-wide filter (a virtual-host config of multi's compiled
	// virtual host, disabled on its refusals) and a rule's own of the same
	// kind (a route config the proxy combines with it).
	hm := func(key string) *anypb.Any {
		return mustAny(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
			RequestMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Remove{Remove: key}}},
		}})
	}
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": {Name: ohvHeaderMutation, Config: hm("x-service")}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {
		{
			Matches:          []proxy.GammaMatch{{Prefix: "/v2"}},
			Backends:         []proxy.GammaBackend{{Service: "demo/eighty", Cluster: proxy.ServiceClusterName("demo/eighty", meshDomain), Weight: 1, Port: 80}},
			ExtensionFilters: []proxy.ExtensionFilter{{Name: ohvHeaderMutation, Config: hm("x-rule")}},
		},
	}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", &ohvRegistry{services: services}))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	var listeners []*listenerv3.Listener
	for name, r := range snap.GetResources(resourcev3.ListenerType) {
		if strings.HasPrefix(name, "capture_") && !strings.Contains(name, "udp") {
			listeners = append(listeners, proto.Clone(r).(*listenerv3.Listener))
		}
	}
	require.Len(t, listeners, 1)
	require.NotNil(t, listeners[0].GetFilterChainMatcher(), "the agent's capture listener selects chains with a matcher")

	clusters := []*clusterv3.Cluster{xdsCluster(), agentXDSCluster(), proxy.NewPassthroughOriginalDstCluster()}
	for name := range snap.GetResources(resourcev3.ClusterType) {
		if name == proxy.PassthroughClusterName {
			continue
		}
		clusters = append(clusters, &clusterv3.Cluster{
			Name: name, ConnectTimeout: durationpb.New(1e9),
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			LoadAssignment:       &endpointv3.ClusterLoadAssignment{ClusterName: name},
		})
	}
	// One capture table, cap_http, the proxy port's branch included: the
	// proxy port's own table (cap_http_18081) and chain went in chart 3.0.2.
	_, published := snap.GetResources(resourcev3.RouteType)[proxy.CaptureHTTPRouteName(18081)]
	require.False(t, published, "the proxy port's table is compiled into cap_http, never published")
	hcms := 0
	for _, fc := range listeners[0].GetFilterChains() {
		for _, f := range fc.GetFilters() {
			hcm := &http_connection_managerv3.HttpConnectionManager{}
			if f.GetTypedConfig().UnmarshalTo(hcm) == nil {
				hcms++
				require.Equal(t, proxy.CaptureHTTPChainName, hcm.GetRds().GetRouteConfigName(), "chain %s", fc.GetName())
			}
		}
	}
	require.Equal(t, 1, hcms, "one connection manager on the capture listener")
	rc, ok := snap.GetResources(resourcev3.RouteType)[proxy.CaptureHTTPChainName].(*routev3.RouteConfiguration)
	require.True(t, ok, "the snapshot carries %s", proxy.CaptureHTTPChainName)
	requireProxyPortBranch(t, rc)
	listeners = append(listeners, inlineTableListener(rc, 15200))
	data, err := marshalBootstrap(newBootstrap(clusters, listeners))
	require.NoError(t, err)
	return data
}

// requireProxyPortBranch: the compiled table carries routes for a dial to the
// proxy port (18081). (Until chart 3.0.3 some of them told the mesh name
// apart from the service's Kubernetes names by an exact or prefix :authority
// match; the proxy port answers those names as the mesh name now, one class.
// The per-port fixture still has that shape: TestPerPortFixtureIsNotVacuous.)
func requireProxyPortBranch(t *testing.T, rc *routev3.RouteConfiguration) {
	t.Helper()
	proxyPort := 0
	for _, vh := range rc.GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			if isProxyPortRoute(r) {
				proxyPort++
			}
		}
	}
	require.Positive(t, proxyPort, "no route of the proxy port in cap_http")
}

// isProxyPortRoute: the route is of the proxy port's block (dialled 18081).
func isProxyPortRoute(r *routev3.Route) bool {
	for _, fs := range r.GetMatch().GetFilterState() {
		if fs.GetKey() == proxy.DialledPortFilterStateKey && fs.GetStringMatch().GetExact() == "18081" {
			return true
		}
	}
	return false
}

// proxyPortRoutesByName counts the routes of the proxy port's block that tell
// a name apart by an exact or prefix :authority match.
func proxyPortRoutesByName(rc *routev3.RouteConfiguration) int {
	n := 0
	for _, vh := range rc.GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			if !isProxyPortRoute(r) {
				continue
			}
			for _, h := range r.GetMatch().GetHeaders() {
				if h.GetName() == ":authority" && (h.GetStringMatch().GetExact() != "" || h.GetStringMatch().GetPrefix() != "") {
					n++
				}
			}
		}
	}
	return n
}

const ohvHeaderMutation = "envoy.filters.http.header_mutation"

// ohvDisables: a route-level FilterConfig that disables the filter.
func ohvDisables(a *anypb.Any) bool {
	fc := &routev3.FilterConfig{}
	return a != nil && a.MessageIs(fc) && a.UnmarshalTo(fc) == nil && fc.GetDisabled()
}

// inlineTableListener serves rc inline on a loopback port, so that validate
// mode parses it.
func inlineTableListener(rc *routev3.RouteConfiguration, port uint32) *listenerv3.Listener {
	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix:     rc.GetName() + "_validate",
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{RouteConfig: rc},
		HttpFilters: []*http_connection_managerv3.HttpFilter{
			// As in the capture chain: present, disabled by default, enabled
			// per virtual host or route.
			{
				Name:       ohvHeaderMutation,
				Disabled:   true,
				ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{TypedConfig: mustAny(&header_mutationv3.HeaderMutation{})},
			},
			{
				Name:       "envoy.filters.http.router",
				ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{TypedConfig: mustAny(&routerv3.Router{})},
			},
		},
	}
	return &listenerv3.Listener{
		Name: rc.GetName() + "_validate",
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		}}},
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{{
			Name:       "envoy.filters.network.http_connection_manager",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(hcm)},
		}}}},
	}
}

func runValidate(t *testing.T, data []byte) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	out, err := exec.Command(envoyBinary(t), "--mode", "validate", "-c", path).CombinedOutput()
	return string(out), err
}

func TestAgentCaptureListenerAndTableValidate(t *testing.T) {
	data := agentCaptureBootstrap(t)
	out, err := runValidate(t, data)
	t.Logf("envoy --mode validate:\n%s", out)
	require.NoError(t, err, "the pinned proxy refuses the agent's capture listener or its table")

	// The table is a plain route list: no virtual host has a matcher, and
	// the routes of a port match the dialled port's filter state.
	bs := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(data, bs))
	fsRoutes, vhostFilters, routeDisables := 0, 0, 0
	for _, l := range bs.GetStaticResources().GetListeners() {
		if l.GetName() != proxy.CaptureHTTPChainName+"_validate" {
			continue
		}
		hcm := &http_connection_managerv3.HttpConnectionManager{}
		require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
		for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
			require.Nil(t, vh.GetMatcher(), "virtual host %s has a matcher", vh.GetName())
			if _, ok := vh.GetTypedPerFilterConfig()[ohvHeaderMutation]; ok {
				vhostFilters++
			}
			for _, r := range vh.GetRoutes() {
				if ohvDisables(r.GetTypedPerFilterConfig()[ohvHeaderMutation]) {
					routeDisables++
				}
				for _, fs := range r.GetMatch().GetFilterState() {
					if fs.GetKey() == proxy.DialledPortFilterStateKey {
						fsRoutes++
					}
				}
			}
		}
	}
	require.Positive(t, fsRoutes, "anti-vacuity: the validated table has routes on the dialled port")
	require.Positive(t, vhostFilters, "anti-vacuity: the service-wide filter is a virtual-host config of the validated table")
	require.Positive(t, routeDisables, "anti-vacuity: the refusals of that virtual host disable it per route")

	// Seen red: a route-level FilterConfig that neither disables nor
	// configures the filter. The gate must refuse it, or it would not be
	// reading the routes' per-filter entries.
	bad := mutateCompiledTable(t, data, func(vh *routev3.VirtualHost) bool {
		for _, r := range vh.GetRoutes() {
			if ohvDisables(r.GetTypedPerFilterConfig()[ohvHeaderMutation]) {
				r.TypedPerFilterConfig[ohvHeaderMutation] = mustAny(&routev3.FilterConfig{})
				return true
			}
		}
		return false
	})
	out, err = runValidate(t, bad)
	require.Error(t, err, "validate accepted a route-level FilterConfig with neither disabled nor config:\n%s", out)
	t.Logf("refused as it must be:\n%s", out)

	// Seen red: the same bootstrap with one dialled-port match whose key is
	// empty. The gate must refuse it, or it would not be reading the
	// table's route matches at all.
	bad = mutateCompiledTable(t, data, func(vh *routev3.VirtualHost) bool {
		for _, r := range vh.GetRoutes() {
			for _, fs := range r.GetMatch().GetFilterState() {
				if fs.GetKey() == proxy.DialledPortFilterStateKey {
					fs.Key = ""
					return true
				}
			}
		}
		return false
	})
	out, err = runValidate(t, bad)
	require.Error(t, err, "validate accepted a filter-state route match with no key:\n%s", out)
	t.Logf("refused as it must be:\n%s", out)
}

// mutateCompiledTable edits the first virtual host f changes in the inline
// copy of cap_http.
func mutateCompiledTable(t *testing.T, data []byte, f func(*routev3.VirtualHost) bool) []byte {
	t.Helper()
	bs := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(data, bs))
	for _, l := range bs.GetStaticResources().GetListeners() {
		if l.GetName() != proxy.CaptureHTTPChainName+"_validate" {
			continue
		}
		f0 := l.GetFilterChains()[0].GetFilters()[0]
		hcm := &http_connection_managerv3.HttpConnectionManager{}
		require.NoError(t, f0.GetTypedConfig().UnmarshalTo(hcm))
		changed := false
		for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
			if !changed && f(vh) {
				changed = true
			}
		}
		require.True(t, changed)
		a, err := anypb.New(hcm)
		require.NoError(t, err)
		f0.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: a}
	}
	out, err := marshalBootstrap(bs)
	require.NoError(t, err)
	return out
}
