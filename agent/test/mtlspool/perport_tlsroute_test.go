package mtlspool

// Which chain does the pinned proxy select for cleartext HTTP to the address
// of a service that is also the parent of a TLSRoute, on each port its HTTP is
// reachable by: the HTTP ports it declares (8080, and 80, which this one
// declares too) and the mesh's HTTP proxy port (18081), where the port in Host
// is the port?
//
// The capture listener is the one SnapshotCache generates. To run it outside
// a pod's network namespace, one listener is published per dialled port D:
// bound to 0.0.0.0:<free port>, with D re-keyed onto that port in its
// filter_chain_matcher, cap_http recording D as the dialled port, and every
// service address moved to a loopback alias (127.0.0.N). A connection to
// 127.0.0.N on that listener is, to the matcher and the route table, a
// connection to the service's address on port D. The passthrough cluster is replaced by a plain cluster to
// a second server, so "left the mesh" has a body of its own.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// pptRekey moves, in a listener's filter_chain_matcher, every service address
// onto its alias and the port number from onto to: exact-map keys and exact
// string predicates alike.
func pptRekey(t *testing.T, m *xdsmatcherv3.Matcher, alias map[string]string, from, to uint32) {
	t.Helper()
	if m == nil {
		return
	}
	key := func(k string) string {
		if a, ok := alias[k]; ok {
			return a
		}
		if k == fmt.Sprint(from) {
			return fmt.Sprint(to)
		}
		return k
	}
	var pred func(p *xdsmatcherv3.Matcher_MatcherList_Predicate)
	pred = func(p *xdsmatcherv3.Matcher_MatcherList_Predicate) {
		if sm := p.GetSinglePredicate().GetValueMatch(); sm != nil && sm.GetExact() != "" {
			sm.MatchPattern = &xdsmatcherv3.StringMatcher_Exact{Exact: key(sm.GetExact())}
		}
		for _, q := range p.GetOrMatcher().GetPredicate() {
			pred(q)
		}
		if p.GetNotMatcher() != nil {
			pred(p.GetNotMatcher())
		}
	}
	for _, fm := range m.GetMatcherList().GetMatchers() {
		pred(fm.GetPredicate())
		pptRekey(t, fm.GetOnMatch().GetMatcher(), alias, from, to)
	}
	if mm := m.GetMatcherTree().GetExactMatchMap(); mm != nil {
		next := map[string]*xdsmatcherv3.Matcher_OnMatch{}
		for k, om := range mm.GetMap() {
			pptRekey(t, om.GetMatcher(), alias, from, to)
			next[key(k)] = om
		}
		mm.Map = next
	}
	pptRekey(t, m.GetOnNoMatch().GetMatcher(), alias, from, to)
}

func pptServer(t *testing.T, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func pptPlain(name, addr string) *clusterv3.Cluster {
	host, p, _ := net.SplitHostPort(addr)
	var port uint32
	_, _ = fmt.Sscanf(p, "%d", &port)
	return &clusterv3.Cluster{
		Name: name, ConnectTimeout: durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: name, Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
			HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
				Address: host, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
			}}}}},
		}}}}},
	}
}

func pptGet(addr, host string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Host = host
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func TestPerPort_TLSRouteParent_CleartextHTTP_Live(t *testing.T) {
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	services := []ppaSvc{
		{ns: "demo", name: "httponly", ip: "10.0.3.1", vip: "10.96.3.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{80: h, 8080: h}},
		{ns: "demo", name: "plain", ip: "10.0.3.2", vip: "10.96.3.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}},
		{ns: "demo", name: "tcponly", ip: "10.0.3.3", vip: "10.96.3.3", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp}},
	}
	alias := map[string]string{"10.96.3.1": "127.0.0.2", "10.96.3.2": "127.0.0.3", "10.96.3.3": "127.0.0.4"}

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
	// The TLSRoute: parent demo/httponly.
	c.SetTLSServiceRoutes(map[string][]proxy.L4ServiceRoute{"demo/httponly": {{
		SNIHostnames: []string{"tls.example"},
		Backends:     []proxy.L4Backend{{Service: "demo/tcponly", Cluster: "tcp:tcponly.demo.aether.internal", Weight: 1}},
	}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", ppNodeName, &ppaRegistry{services: services}))

	mesh := pptServer(t, "mesh")
	outside := pptServer(t, "passthrough")
	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	res := map[resourcev3.Type][]types.Resource{}
	for _, r := range snap.GetResources(resourcev3.RouteType) {
		res[resourcev3.RouteType] = append(res[resourcev3.RouteType], r)
	}
	for _, r := range snap.GetResources(resourcev3.ExtensionConfigType) {
		res[resourcev3.ExtensionConfigType] = append(res[resourcev3.ExtensionConfigType], r)
	}
	for name := range snap.GetResources(resourcev3.ClusterType) {
		if name == proxy.PassthroughClusterName {
			res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], pptPlain(name, outside))
			continue
		}
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], pptPlain(name, mesh))
	}
	// east-west QUIC twins are fetched on demand by the default cluster's
	// routes; serve them as plain clusters up front.
	res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType],
		pptPlain("quic:httponly.demo.aether.internal@demo/client", mesh), pptPlain("quic:plain.demo.aether.internal@demo/client", mesh))

	var capture *listenerv3.Listener
	for name, r := range snap.GetResources(resourcev3.ListenerType) {
		if strings.HasPrefix(name, "capture_") && !strings.Contains(name, "udp") {
			capture = r.(*listenerv3.Listener)
		}
	}
	require.NotNil(t, capture)
	dialled := []uint32{8080, 18081, 80}
	ports := map[uint32]uint32{}
	for _, d := range dialled {
		l := proto.Clone(capture).(*listenerv3.Listener)
		p := ppFreePort(t)
		ports[d] = p
		l.Name = fmt.Sprintf("capture_d%d", d)
		l.StatPrefix = l.Name
		l.Address = &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "0.0.0.0", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: p},
		}}}
		l.Transparent = nil
		var lf []*listenerv3.ListenerFilter
		for _, f := range l.GetListenerFilters() {
			if !strings.Contains(f.GetName(), "original_dst") {
				lf = append(lf, f)
			}
		}
		l.ListenerFilters = lf
		// The listener selects its chains with a matching tree: the service
		// addresses and the dialled port D it names are moved onto the
		// aliases and this listener's port, and cap_http records D as the
		// dialled port (it would record the loopback port).
		pptRekey(t, l.GetFilterChainMatcher(), alias, d, p)
		var kept []string
		for _, fc := range l.GetFilterChains() {
			if fc.GetName() == proxy.CaptureHTTPChainName {
				ppSetDialledPort(t, fc, fmt.Sprint(d))
			}
			kept = append(kept, fc.GetName())
		}
		t.Logf("dialled port %d: chains %v + default %s", d, kept, l.GetDefaultFilterChain().GetName())
		res[resourcev3.ListenerType] = append(res[resourcev3.ListenerType], l)
	}

	cp := startADSControlPlane(t, res)
	e := launchEnvoyOverADS(t, bin, cp)
	for _, d := range dialled {
		e.listenerAddr(t, fmt.Sprintf("capture_d%d", d))
	}
	time.Sleep(time.Second)

	// What a client writes in Host for each dialled port: the port it dialled
	// (none for 80, as a URL without a port), and on the proxy port the port
	// of the service it wants.
	host := func(svc string, d uint32) string {
		switch d {
		case 80:
			return svc + ".demo.aether.internal"
		case 18081:
			return svc + ".demo.aether.internal:8080"
		}
		return fmt.Sprintf("%s.demo.aether.internal:%d", svc, d)
	}
	for _, d := range dialled {
		control := pptGet(fmt.Sprintf("127.0.0.3:%d", ports[d]), host("plain", d))
		got := pptGet(fmt.Sprintf("127.0.0.2:%d", ports[d]), host("httponly", d))
		t.Logf("dialled %-5d  plain (no TLSRoute): %-18.18s  httponly (TLSRoute parent): %.60s", d, control, got)
		// plain does not declare port 80: a dial to it is refused by name,
		// which still shows that the HTTP chain of port 80 took it.
		wantControl := "200 mesh"
		if d == 80 {
			wantControl = "421 "
		}
		if !strings.HasPrefix(control, wantControl) {
			t.Errorf("control: HTTP to plain:%d answered %q, want %q; the rig does not show the mesh path", d, control, wantControl)
		}
		if got != "200 mesh" {
			t.Errorf("cleartext HTTP to the TLSRoute parent's address on port %d did not reach the mesh: %q", d, got)
		}
	}
	// On the proxy port nothing dialled decides: a Host that names 18081
	// itself is a port the service does not declare, and is refused there.
	if got := pptGet(fmt.Sprintf("127.0.0.2:%d", ports[18081]), "httponly.demo.aether.internal:18081"); !strings.HasPrefix(got, "421 ") {
		t.Errorf("Host httponly:18081 on the proxy port: want a 421 (18081 is not a port of the service), got %q", got)
	}
}
