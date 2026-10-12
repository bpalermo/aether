package mtlspool

// Does the pinned proxy ACCEPT what agent/internal/xds/cache generates for
// hostile service shapes? //agent/test/envoy_validate's per-port fixture
// puts a hand-written copy of the table builder in front of the proxy
// (perport.go, PerPortRouteTable), not the cache's own output. Here the
// snapshot of a real SnapshotCache is served over delta-ADS: the capture
// listener with every chain it has (address moved to loopback), and every
// route table its chains name. A rejected listener or table shows as
// update_rejected.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

type ppaSvc struct {
	ns, name, ip, vip string
	legacy            uint32
	ports             map[uint32]registryv1.PortProtocol
}

func (s ppaSvc) key() string { return s.ns + "/" + s.name }

type ppaRegistry struct{ services []ppaSvc }

func (r *ppaRegistry) Initialize(context.Context) error { return nil }
func (r *ppaRegistry) Close() error                     { return nil }
func (r *ppaRegistry) RegisterEndpoint(context.Context, string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint) error {
	return nil
}
func (r *ppaRegistry) UnregisterEndpoint(context.Context, string, string) error    { return nil }
func (r *ppaRegistry) UnregisterEndpoints(context.Context, string, []string) error { return nil }
func (r *ppaRegistry) ListEndpoints(context.Context, string, registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func (r *ppaRegistry) ListAllEndpoints(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
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
			if registryv1.Service_Protocol(class) == protocol {
				serves = true
			}
		}
		slices.Sort(ep.Ports)
		if serves {
			out[s.key()] = append(out[s.key()], ep)
		}
	}
	return out, nil
}

func ppaFixture() []ppaSvc {
	h, tcp, udp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_UDP
	m := func(kv ...any) map[uint32]registryv1.PortProtocol {
		out := map[uint32]registryv1.PortProtocol{}
		for i := 0; i < len(kv); i += 2 {
			out[uint32(kv[i].(int))] = kv[i+1].(registryv1.PortProtocol)
		}
		return out
	}
	i := 0
	s := func(ns, name string, legacy uint32, ports map[uint32]registryv1.PortProtocol) ppaSvc {
		i++
		return ppaSvc{ns: ns, name: name, legacy: legacy, ports: ports, ip: fmt.Sprintf("10.0.3.%d", i), vip: fmt.Sprintf("10.96.3.%d", i)}
	}
	return []ppaSvc{
		s("demo", "multi", 8080, m(8080, h, 9090, h)),
		s("other", "multi", 7070, m(7070, h)), // a bare name two namespaces share
		s("demo", "mixed", 8080, m(8080, h, 9000, tcp)),
		s("demo", "tcpfirst", 9000, m(9000, tcp, 8080, h)),
		s("demo", "nine", 9000, m(9000, h)),
		s("demo", "tcponly", 9000, m(9000, tcp)),
		s("demo", "w80", 80, m(80, h)),
		s("demo", "t80", 8080, m(8080, h, 80, tcp)),
		s("demo", "m81", 8080, m(8080, h, 18081, h)),
		s("demo", "l81", 18081, m(18081, h)),
		s("demo", "t81", 18081, m(18081, tcp)),
		s("demo", "h82", 18082, m(18082, h)),
		s("demo", "t82", 8080, m(8080, h, 18082, tcp)),
		s("demo", "l82", 18082, m(18082, tcp)),
		s("demo", "u53", 8080, m(8080, h, 5353, udp)),
		s("demo", "tcp80", 80, m(80, tcp)),
		s("demo", "tmulti", 9000, m(9000, tcp, 8080, tcp, 80, tcp, 18081, tcp)),
		s("demo", "udponly", 5353, m(5353, udp)),
	}
}

func ppaStats(t *testing.T, admin, filter string) map[string]string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + admin + "/stats?filter=" + filter)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			out[k] = v
		}
	}
	return out
}

func TestPerPort_PinnedProxyAcceptsTheCacheSnapshot(t *testing.T) {
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	ctx := context.Background()
	services := ppaFixture()
	c := cache.NewSnapshotCache(ppNodeName, slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: "demo", ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, ppNodeSpiffe))
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	for _, s := range services {
		authorities[s.key()] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
		vips = append(vips, capture.CaptureTCPService{ServiceName: s.key(), ClusterIP: s.vip})
	}
	c.SetCaptureAuthorities(authorities)
	c.SetCaptureTCPServices(vips)

	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	hm := proxy.ExtensionFilter{Name: "envoy.filters.http.header_mutation", Config: cfg}
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/multi": hm, "demo/usersvc": hm, "demo/tcponly": hm, "demo/l81": hm})
	mesh := func(svc string) string { return proxy.ServiceClusterName(svc, "aether.internal") }
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/multi": {
			{Matches: []proxy.GammaMatch{{Prefix: "/a"}}, Backends: []proxy.GammaBackend{
				{Service: "demo/w80", Cluster: mesh("demo/w80"), Weight: 1, Port: 80},
				{Service: "demo/multi", Cluster: mesh("demo/multi"), Weight: 1, Port: 9999},
				{Service: "demo/t80", Cluster: mesh("demo/t80"), Weight: 1, Port: 80},
			}},
			{ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/b"}}, Backends: []proxy.GammaBackend{{Service: "demo/m81", Cluster: mesh("demo/m81"), Weight: 1, Port: 18081}}, ExtensionFilters: []proxy.ExtensionFilter{hm}},
		},
		"demo/usersvc": {{ParentPort: 80, Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: mesh("demo/multi"), Weight: 1, Port: 8080}}}},
		"demo/tcponly": {{Backends: []proxy.GammaBackend{{Service: "demo/tcponly", Cluster: mesh("demo/tcponly"), Weight: 1, Port: 9000}}}},
		"demo/w80":     {{ParentPort: 18081, Backends: []proxy.GammaBackend{{Service: "demo/l81", Cluster: mesh("demo/l81"), Weight: 1, Port: 18081}}}},
		"demo/h82":     {{ParentPort: 18082, Backends: []proxy.GammaBackend{{Service: "demo/h82", Cluster: mesh("demo/h82"), Weight: 1, Port: 18082}}}},
	})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/usersvc": {80: 8080, 81: 0, 9090: 9090, 18081: 8080, 18082: 8080}})
	c.SetRouteTargetPorts(map[string][]uint32{"demo/usersvc": {80}, "demo/multi": {9090}, "demo/w80": {18081}, "demo/h82": {18082}})
	tls := map[string][]proxy.L4ServiceRoute{}
	for _, p := range []string{"demo/w80", "demo/m81", "demo/tcp80", "demo/tmulti", "demo/l82", "demo/multi", "demo/t81"} {
		tls[p] = []proxy.L4ServiceRoute{{
			SNIHostnames: []string{"tls." + strings.ReplaceAll(p, "/", ".") + ".example"},
			Backends:     []proxy.L4Backend{{Service: "demo/tcponly", Cluster: "tcp:tcponly.demo.aether.internal", Weight: 1}},
		}}
	}
	c.SetTLSServiceRoutes(tls)
	c.SetTCPServiceRoutes(map[string][]proxy.L4ServiceRoute{"demo/tcp80": {{Backends: []proxy.L4Backend{
		{Service: "demo/tcponly", Cluster: "tcp:tcponly.demo.aether.internal", Weight: 1}, {Service: "demo/l82", Cluster: "tcp:l82.demo.aether.internal", Weight: 3},
	}}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", ppNodeName, &ppaRegistry{services: services}))

	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	port := ppFreePort(t)
	res := map[resourcev3.Type][]types.Resource{}
	for _, r := range snap.GetResources(resourcev3.RouteType) {
		res[resourcev3.RouteType] = append(res[resourcev3.RouteType], r)
	}
	for _, r := range snap.GetResources(resourcev3.ExtensionConfigType) {
		res[resourcev3.ExtensionConfigType] = append(res[resourcev3.ExtensionConfigType], r)
	}
	for name, r := range snap.GetResources(resourcev3.ClusterType) {
		if name == proxy.PassthroughClusterName {
			res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], r)
			continue
		}
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], &clusterv3.Cluster{
			Name: name, ConnectTimeout: durationpb.New(time.Second),
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			LoadAssignment:       &endpointv3.ClusterLoadAssignment{ClusterName: name},
		})
	}
	var chainNames []string
	tables := 0
	for name, r := range snap.GetResources(resourcev3.ListenerType) {
		if !strings.HasPrefix(name, "capture_") || strings.Contains(name, "udp") {
			continue
		}
		l := proto.Clone(r).(*listenerv3.Listener)
		l.Address = &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		}}}
		l.Transparent = nil
		l.Name = "capture"
		for _, fc := range l.GetFilterChains() {
			chainNames = append(chainNames, fc.GetName())
			if strings.HasPrefix(fc.GetName(), "cap_http_") {
				tables++
			}
		}
		res[resourcev3.ListenerType] = append(res[resourcev3.ListenerType], l)
	}
	require.Len(t, res[resourcev3.ListenerType], 1)
	slices.Sort(chainNames)
	t.Logf("%d chains, %d HTTP chains: %v", len(chainNames), tables, chainNames)

	cp := startADSControlPlane(t, res)
	e := launchEnvoyOverADS(t, bin, cp)

	var stats map[string]string
	deadline := time.Now().Add(20 * time.Second)
	for {
		stats = ppaStats(t, e.admin, "(update_rejected|update_success|update_failure|listener_added|total_listeners_active|total_listeners_warming)")
		ok := 0
		for k, v := range stats {
			if strings.Contains(k, ".rds.cap_http_") && strings.HasSuffix(k, "update_success") && v != "0" {
				ok++
			}
		}
		if ok >= tables || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if stats[k] != "0" || strings.Contains(k, "rejected") {
			t.Logf("%s = %s", k, stats[k])
		}
	}
	accepted := 0
	for _, k := range keys {
		if strings.HasSuffix(k, "update_rejected") && stats[k] != "0" {
			t.Errorf("the pinned proxy REJECTED an update: %s = %s", k, stats[k])
		}
		if strings.Contains(k, ".rds.cap_http_") && strings.HasSuffix(k, "update_success") && stats[k] != "0" {
			accepted++
		}
	}
	require.Equal(t, tables, accepted, "every capture route table accepted")
	require.Equal(t, "1", stats["listener_manager.total_listeners_active"], "the capture listener is active")
	_ = net.IPv4len
}
