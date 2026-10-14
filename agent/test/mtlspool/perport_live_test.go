package mtlspool

// The claim under test: a request that is routed by the "*" (on-demand
// catch-all) virtual host of a capture route table is answered WITHOUT the
// destination service's service-wide chain filter (proposal 025 CHAIN scope;
// ext_authz is one such filter), because that filter is enabled only by the
// service's own virtual host (typed_per_filter_config).
//
// What runs: the pinned proxy over delta-ADS. The route tables and the capture
// listener are the ones agent/internal/xds/cache.SnapshotCache generates for a
// node; nothing about routing or filters is hand-written. Two things are
// replaced so the listener can run outside a pod's network namespace: its
// address (loopback, no netns, not transparent; the HTTP chain under test is
// re-keyed onto the loopback port), and the clusters' bodies (plain clusters of
// the SAME NAMES pointing at a local HTTP server, since no SPIRE is here).
//
// The chain filter is header_mutation appending a RESPONSE header: the header
// is on the response iff the filter ran for that request. It is enabled the
// way ext_authz is: default-disabled in the HCM, enabled per virtual host.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	ppSvc    = "demo/multi"
	ppMesh   = "multi.demo.aether.internal"
	ppHeader = "x-chain-filter"
	// ppRuleHeader is the response header the service's HTTPRoute rule adds.
	ppRuleHeader = "x-route-rule"
	// ppInvalidPath is matched by a rule whose only backendRef names a port
	// the backend does not have.
	ppInvalidPath = "/invalid-backend"
	ppNodeName    = "node-1"
	ppNodeSpiffe  = "spiffe://aether.internal/ns/aether-system/sa/aether-agent"
)

// ppRegistry lists nothing until armed, then one pod of demo/multi serving
// HTTP on 8080 (its declared port) and 9090.
type ppRegistry struct{ armed atomic.Bool }

func (r *ppRegistry) Initialize(context.Context) error { return nil }
func (r *ppRegistry) Close() error                     { return nil }
func (r *ppRegistry) RegisterEndpoint(context.Context, string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint) error {
	return nil
}
func (r *ppRegistry) UnregisterEndpoint(context.Context, string, string) error    { return nil }
func (r *ppRegistry) UnregisterEndpoints(context.Context, string, []string) error { return nil }
func (r *ppRegistry) ListEndpoints(context.Context, string, registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func ppEndpoint(ip, sa string) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip: ip, ClusterName: "cluster-1", Port: 8080, Weight: 100,
		Ports: []uint32{8080, 9090},
		PortProtocols: map[uint32]registryv1.PortProtocol{
			8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
			9090: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
		},
		ContainerMetadata:  &registryv1.ServiceEndpoint_ContainerMetadata{ContainerId: "c", NetworkNamespace: "/proc/1/ns/net"},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "demo", PodName: sa + "-0", NodeName: "node-2"},
	}
}

// ListAllEndpoints always lists demo/other (so the node's mesh has HTTP on
// 8080 from the start), and demo/multi only once armed.
func (r *ppRegistry) ListAllEndpoints(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	out := map[string][]*registryv1.ServiceEndpoint{}
	if protocol != registryv1.Service_PROTOCOL_HTTP {
		return out, nil
	}
	out["demo/other"] = []*registryv1.ServiceEndpoint{ppEndpoint("10.0.1.3", "other")}
	// A service of the SAME NAME as the one under test, in another
	// namespace, with no filter and no route: what a short name must never
	// reach from a caller in "demo", and must reach from one in "elsewhere".
	out[ppElsewhere] = []*registryv1.ServiceEndpoint{ppEndpoint("10.0.1.4", "multi")}
	if r.armed.Load() {
		out[ppSvc] = []*registryv1.ServiceEndpoint{ppEndpoint("10.0.1.2", "multi")}
	}
	return out, nil
}

// ppElsewhere is a second service named "multi", in another namespace.
const ppElsewhere = "elsewhere/multi"

// ppCallerNamespace is the namespace of the pod whose listeners the rig
// serves. A test that calls from another namespace sets it before ppStart.
var ppCallerNamespace = "demo"

// ppDeclared is whether the generated Services carry their real ports. With
// false the agent knows the services by name only.
var ppDeclared = true

func ppProject(c *cache.SnapshotCache) {
	c.SetCaptureAuthorities(map[string]string{
		ppSvc: "multi.demo.svc.cluster.local", "demo/other": "other.demo.svc.cluster.local", ppElsewhere: "multi.elsewhere.svc.cluster.local",
	})
	var declared []uint32
	if ppDeclared {
		declared = []uint32{8080, 9090}
	}
	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: ppSvc, ClusterIP: "10.96.1.2", DeclaredHTTP: declared},
		{ServiceName: "demo/other", ClusterIP: "10.96.1.3", DeclaredHTTP: declared},
		{ServiceName: ppElsewhere, ClusterIP: "10.96.1.4", DeclaredHTTP: declared},
	})
}

func ppFreePort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return uint32(ln.Addr().(*net.TCPAddr).Port)
}

// ppTranslate turns the agent's snapshot into what the harness proxy can run:
// the capture listener on loopback (hcmChain re-keyed onto port), every route
// table and extension config verbatim, and a plain cluster per agent cluster.
//
// The capture listener has one HTTP chain, cap_http, which records the port
// the client dialled for its route table. hcmChain "cap_http_<P>" names the
// per-port chain these tests were written against: it is cap_http with the
// dialled port recorded as P whatever loopback port the listener is on, so a
// request answers as one dialled to P. "cap_http" alone is P = 8080.
func ppTranslate(t *testing.T, c *cache.SnapshotCache, port uint32, hcmChain, backend string, extra ...string) map[resourcev3.Type][]types.Resource {
	t.Helper()
	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	out := map[resourcev3.Type][]types.Resource{}
	for _, r := range snap.GetResources(resourcev3.RouteType) {
		out[resourcev3.RouteType] = append(out[resourcev3.RouteType], r)
	}
	for _, r := range snap.GetResources(resourcev3.ExtensionConfigType) {
		out[resourcev3.ExtensionConfigType] = append(out[resourcev3.ExtensionConfigType], r)
	}
	host, p, err := net.SplitHostPort(backend)
	require.NoError(t, err)
	var bp uint32
	_, err = fmt.Sscanf(p, "%d", &bp)
	require.NoError(t, err)
	names := append([]string(nil), extra...)
	for name := range snap.GetResources(resourcev3.ClusterType) {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if name == proxy.PassthroughClusterName {
			continue
		}
		out[resourcev3.ClusterType] = append(out[resourcev3.ClusterType], &clusterv3.Cluster{
			Name:                 name,
			ConnectTimeout:       durationpb.New(2 * time.Second),
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			LoadAssignment: &endpointv3.ClusterLoadAssignment{
				ClusterName: name,
				Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
							Address: host, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: bp},
						}}},
					}},
				}}}},
			},
		})
	}
	for name, r := range snap.GetResources(resourcev3.ListenerType) {
		if !strings.HasPrefix(name, "capture_") || strings.Contains(name, "udp") {
			continue
		}
		l := proto.Clone(r).(*listenerv3.Listener)
		l.Address = &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		}}}
		l.Transparent = nil
		l.UseOriginalDst = nil
		l.DefaultFilterChain = nil
		var lf []*listenerv3.ListenerFilter
		for _, f := range l.GetListenerFilters() {
			if !strings.Contains(f.GetName(), "original_dst") {
				lf = append(lf, f)
			}
		}
		l.ListenerFilters = lf
		var chains []*listenerv3.FilterChain
		var names []string
		chain, dialled := ppDialledChain(hcmChain)
		// One chain is kept, so the listener's matching tree goes with the
		// others: the kept chain is matched on the loopback port alone.
		l.FilterChainMatcher = nil
		for _, fc := range l.GetFilterChains() {
			names = append(names, fc.GetName())
			switch {
			case fc.GetName() == chain:
				if fc.FilterChainMatch == nil {
					fc.FilterChainMatch = &listenerv3.FilterChainMatch{}
				}
				fc.FilterChainMatch.DestinationPort = wrapperspb.UInt32(port)
				if dialled != "" {
					ppSetDialledPort(t, fc, dialled)
				}
				chains = append(chains, fc)
			}
		}
		require.NotEmptyf(t, chains, "the capture listener has no chain %q; it has %v", hcmChain, names)
		l.FilterChains = chains
		l.Name = "capture"
		out[resourcev3.ListenerType] = append(out[resourcev3.ListenerType], l)
	}
	require.Len(t, out[resourcev3.ListenerType], 1, "one capture listener")
	return out
}

type ppResult struct {
	status int
	filter bool
	rule   bool
	body   string
	took   time.Duration
	err    error
}

func (r ppResult) String() string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	return fmt.Sprintf("status=%d chain-filter-ran=%v route-rule-applied=%v took=%s body=%q", r.status, r.filter, r.rule, r.took.Round(time.Millisecond), r.body)
}

func ppGet(addr, host string) ppResult { return ppGetPath(addr, host, "/") }

func ppGetPath(addr, host, path string) ppResult {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	req.Host = host
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return ppResult{err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return ppResult{status: resp.StatusCode, filter: resp.Header.Get(ppHeader) != "", rule: resp.Header.Get(ppRuleHeader) != "", body: strings.TrimSpace(string(b)), took: time.Since(start)}
}

// ppRun starts the agent's cache with NO endpoint listed, the proxy, and the
// backend; returns what a request sees.
type ppRig struct {
	t       *testing.T
	c       *cache.SnapshotCache
	reg     *ppRegistry
	cp      *adsControlPlane
	addr    string
	port    uint32
	chain   string
	backend string
	hits    *atomic.Int64
	mu      sync.Mutex
	version int
	// onClusterSubscribe is called (once per name) when the proxy subscribes to
	// a cluster BY NAME: its on-demand (ODCDS) request.
	onClusterSubscribe func(name string)
	seen               map[string]bool
	extra              []string
	// proxyPort, when set, also publishes the pod's loopback outbound listener
	// on that port.
	proxyPort uint32
	// mutate, when set, edits the translated resources before every publish:
	// for a measurement of a mechanism the cache does not generate yet.
	mutate func(map[resourcev3.Type][]types.Resource)
}

func (r *ppRig) publish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	res := ppTranslate(r.t, r.c, r.port, r.chain, r.backend, r.extra...)
	if r.proxyPort != 0 {
		ppWithProxyPort(r.t, r.c, res, r.proxyPort)
	}
	if r.mutate != nil {
		r.mutate(res)
	}
	snap, err := cachev3.NewSnapshot(fmt.Sprintf("v%d", r.version), res)
	require.NoError(r.t, err)
	require.NoError(r.t, r.cp.cache.SetSnapshot(context.Background(), envoyNodeID, snap))
}

// ppBoot, when set, configures the rig's cache before any pod is added: the
// boot-time settings (the authz sidecar is one).
var ppBoot func(c *cache.SnapshotCache)

func ppStart(t *testing.T, hcmChain string) *ppRig {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	ctx := context.Background()
	hits := &atomic.Int64{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, req.Host)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := cache.NewSnapshotCache(ppNodeName, slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	if ppBoot != nil {
		ppBoot(c)
	}
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: ppCallerNamespace, ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, ppNodeSpiffe))
	ppProject(c)
	perRoute, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: ppHeader, Value: "ran"},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}}}},
	}})
	require.NoError(t, err)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{ppSvc: {Name: "envoy.filters.http.header_mutation", Config: perRoute}})
	// The service's HTTPRoute: a rule on every port that marks the response,
	// and one whose only backendRef names a port its Service does not have.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{ppSvc: {
		{
			Matches:  []proxy.GammaMatch{{Prefix: ppInvalidPath}},
			Backends: []proxy.GammaBackend{{Service: "demo/other", Cluster: "other.demo.aether.internal", Port: 7777, Weight: 1}},
		},
		{
			Matches:        []proxy.GammaMatch{{Prefix: "/"}},
			HeaderMutation: &proxy.GammaHeaderMutation{AddResponse: []proxy.GammaHeaderKV{{Name: ppRuleHeader, Value: "on"}}},
		},
	}})

	r := &ppRig{t: t, c: c, reg: &ppRegistry{}, chain: hcmChain, backend: ln.Addr().String(), hits: hits, seen: map[string]bool{}}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", ppNodeName, r.reg))

	// The proxy starts WITHOUT the capture listener: its port is picked only
	// once the proxy has bound its admin port. A port probed before that can
	// be the very one the kernel then hands the admin endpoint, and the
	// listener would fail to bind.
	initial := ppTranslate(t, c, 1, hcmChain, r.backend)
	delete(initial, resourcev3.ListenerType)
	r.cp = startADSControlPlaneWithHook(t, initial, func(_ int64, req *discoverygrpc.DeltaDiscoveryRequest) {
		if req.GetTypeUrl() != resourcev3.ClusterType {
			return
		}
		for _, name := range req.GetResourceNamesSubscribe() {
			if name == "*" || name == "" {
				continue
			}
			r.mu.Lock()
			first := !r.seen[name]
			r.seen[name] = true
			fn := r.onClusterSubscribe
			r.mu.Unlock()
			t.Logf("[odcds] the proxy asked for cluster %q by name", name)
			switch {
			case !first:
			case strings.HasPrefix(name, "quic:"):
				// The east-west QUIC twin of the default cluster, fetched on
				// demand by a route of the service's OWN virtual host. Not what
				// is under test: serve it as one more plain cluster.
				go func() {
					r.mu.Lock()
					r.extra = append(r.extra, name)
					r.mu.Unlock()
					r.publish()
				}()
			case fn != nil:
				go fn(name)
			}
		}
	})
	e := launchEnvoyOverADS(t, bin, r.cp)
	r.mu.Lock()
	r.port = ppFreePort(t)
	r.mu.Unlock()
	r.publish()
	r.addr = e.listenerAddr(t, "capture")
	ppAwaitListening(t, r.addr)
	return r
}

// ppEachRoute calls f on every route of rc: a virtual host's own, and those of
// every RouteList its matcher ends in (cap_http keeps all of its routes
// there, one branch per dialled port), which f may edit in place.
func ppEachRoute(t *testing.T, rc *routev3.RouteConfiguration, f func(*routev3.Route)) {
	t.Helper()
	var walk func(m *xdsmatcherv3.Matcher)
	onMatch := func(om *xdsmatcherv3.Matcher_OnMatch) {
		if om.GetMatcher() != nil {
			walk(om.GetMatcher())
			return
		}
		a := om.GetAction()
		if a == nil {
			return
		}
		list := &routev3.RouteList{}
		if a.GetTypedConfig().UnmarshalTo(list) != nil {
			return
		}
		for _, r := range list.GetRoutes() {
			f(r)
		}
		any, err := anypb.New(list)
		require.NoError(t, err)
		a.TypedConfig = any
	}
	walk = func(m *xdsmatcherv3.Matcher) {
		if m == nil {
			return
		}
		for _, fm := range m.GetMatcherList().GetMatchers() {
			onMatch(fm.GetOnMatch())
		}
		for _, om := range m.GetMatcherTree().GetExactMatchMap().GetMap() {
			onMatch(om)
		}
		if m.GetOnNoMatch() != nil {
			onMatch(m.GetOnNoMatch())
		}
	}
	for _, vh := range rc.GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			f(r)
		}
		walk(vh.GetMatcher())
	}
}

// ppDialledChain maps a chain name these tests use onto the listener's chain
// and the dialled port to record on it (ppTranslate). The proxy port's chain
// records none.
func ppDialledChain(hcmChain string) (chain, dialled string) {
	if rest, ok := strings.CutPrefix(hcmChain, "cap_http_"); ok && rest != "18081" {
		return proxy.CaptureHTTPChainName, rest
	}
	if hcmChain == proxy.CaptureHTTPChainName {
		return hcmChain, "8080"
	}
	return hcmChain, ""
}

// ppSetDialledPort makes the chain record port as the dialled port instead of
// the connection's destination port (the harness listener is on a loopback
// port of its own).
func ppSetDialledPort(t *testing.T, fc *listenerv3.FilterChain, port string) {
	t.Helper()
	found := false
	for _, f := range fc.GetFilters() {
		cfg := &set_filter_state_v3.Config{}
		if f.GetTypedConfig().UnmarshalTo(cfg) != nil {
			continue
		}
		for _, v := range cfg.GetOnNewConnection() {
			if v.GetObjectKey() == proxy.DialledPortFilterStateKey {
				v.GetFormatString().GetTextFormatSource().Specifier = &corev3.DataSource_InlineString{InlineString: port}
				found = true
			}
		}
		a, err := anypb.New(cfg)
		require.NoError(t, err)
		f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: a}
	}
	require.True(t, found, "chain %s records no dialled port", fc.GetName())
}

// ppChain is the capture listener's HTTP chain that serves port 8080: the
// per-port chain of chart 3.0.0, cap_http since the one-HCM listener.
func ppChain(t *testing.T) string {
	t.Helper()
	c := cache.NewSnapshotCache(ppNodeName, slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(context.Background(), &cniv1.CNIPod{Name: "client-0", Namespace: ppCallerNamespace, ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(context.Background(), ppNodeSpiffe))
	ppProject(c)
	reg := &ppRegistry{}
	reg.armed.Store(true)
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, reg))
	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	var names []string
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		t.Logf("listener %s", name)
		if !strings.HasPrefix(name, "capture_") {
			continue
		}
		for _, fc := range res.(*listenerv3.Listener).GetFilterChains() {
			names = append(names, fc.GetName())
		}
	}
	if slices.Contains(names, "cap_http_8080") {
		return "cap_http_8080"
	}
	for _, n := range names {
		if strings.HasPrefix(n, "capture_") || n == "cap_http" {
			return n
		}
	}
	t.Fatalf("no HTTP chain among %v", names)
	return ""
}

// TestPerPort_SteadyState_RealPortRunsTheChainFilter: the node holds the
// service's endpoints. A captured request to a REAL port of the service,
// "<mesh name>:8080", must run the service's chain filter.
func TestPerPort_SteadyState_RealPortRunsTheChainFilter(t *testing.T) {
	r := ppStart(t, ppChain(t))
	r.reg.armed.Store(true)
	require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
	r.publish()
	time.Sleep(2 * time.Second) // let CDS and RDS land

	for _, host := range []string{ppMesh + ":8080", ppMesh, "multi.demo.svc.cluster.local:8080", "multi.demo:8080"} {
		got := ppGet(r.addr, host)
		t.Logf("steady state, Host %-40s -> %s", host, got)
		if got.status == 200 && !got.filter {
			t.Errorf("Host %s: delivered to the backend (200) WITHOUT the service's chain filter", host)
		}
	}
}

// TestPerPort_ParkedRequestSkipsTheChainFilter: the node holds NO endpoint
// of the service when the request arrives (the agent's registry is not
// reachable yet). The request is parked by the on-demand filter; the registry
// then answers, the agent publishes the cluster, and the request resumes.
func TestPerPort_ParkedRequestSkipsTheChainFilter(t *testing.T) {
	r := ppStart(t, ppChain(t))
	published := make(chan struct{})
	r.onClusterSubscribe = func(name string) {
		// The registry becomes reachable while the request is parked.
		time.Sleep(300 * time.Millisecond)
		r.reg.armed.Store(true)
		require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
		r.publish()
		close(published)
	}
	host := ppMesh + ":8080"
	first := ppGet(r.addr, host)
	t.Logf("no endpoint held, Host %s (parked on ODCDS) -> %s", host, first)
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Logf("the proxy never asked for the cluster by name: the request was not parked on the on-demand route")
	}
	time.Sleep(2 * time.Second)
	second := ppGet(r.addr, host)
	t.Logf("endpoints held,   Host %s                   -> %s", host, second)
	t.Logf("backend hits: %d", r.hits.Load())

	if !second.filter {
		t.Errorf("control: the SECOND request did not run the chain filter either (%s); the harness cannot see the filter", second)
	}
	if first.status == 200 && !first.filter {
		t.Errorf("the parked request was delivered to the backend (200) WITHOUT the service's chain filter: %s", first)
	}
}
