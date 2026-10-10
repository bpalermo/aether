package mtlspool

// More live cases for the outcome header (aether#1641, aether#1646), each
// needing something the mesh of begun_request_1641_test.go does not have: an
// overloaded destination proxy, a real gRPC client and server, and an edge
// rule that splits between a mesh service and a cleartext backend. They came
// out of the second adversarial review of the pull request.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	overloadv3 "github.com/envoyproxy/go-control-plane/envoy/config/overload/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	fixedheapv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/resource_monitors/fixed_heap/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ---------------------------------------------------------------- a real gRPC
// application (grpc-go server, the standard health service), scripted by the
// request's "service" string, which is also what identifies one call.

type grpcApp struct {
	healthpb.UnimplementedHealthServer
	name  string
	ln    net.Listener
	mu    sync.Mutex
	seen  map[string]int
	conns []net.Conn
}

type trackingListener struct {
	net.Listener
	app *grpcApp
}

func (l trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.app.mu.Lock()
		l.app.conns = append(l.app.conns, c)
		l.app.mu.Unlock()
	}
	return c, err
}

func startGRPCApp(t *testing.T, name string) *grpcApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a := &grpcApp{name: name, ln: ln, seen: map[string]int{}}
	s := grpc.NewServer()
	healthpb.RegisterHealthServer(s, a)
	go func() { _ = s.Serve(trackingListener{ln, a}) }()
	t.Cleanup(s.Stop)
	return a
}

func (a *grpcApp) times(call string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[call]
}

// die drops every connection of the application: the process died while it
// was working on a call.
func (a *grpcApp) die() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.conns {
		_ = c.Close()
	}
	a.conns = nil
}

func (a *grpcApp) behave(call string) string {
	a.mu.Lock()
	a.seen[call]++
	a.mu.Unlock()
	if a.name == "good" {
		return "ok"
	}
	b, _, _ := strings.Cut(call, "/")
	return b
}

func (a *grpcApp) Check(_ context.Context, r *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	switch a.behave(r.GetService()) {
	case "die":
		a.die()
		time.Sleep(200 * time.Millisecond)
		return nil, status.Error(codes.Internal, "unreachable")
	case "unavailable":
		return nil, status.Error(codes.Unavailable, "the application says: unavailable")
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func (a *grpcApp) Watch(r *healthpb.HealthCheckRequest, s healthpb.Health_WatchServer) error {
	switch a.behave(r.GetService()) {
	case "die":
		a.die()
		time.Sleep(200 * time.Millisecond)
		return status.Error(codes.Internal, "unreachable")
	case "dieafterfirst":
		_ = s.Send(&healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING})
		time.Sleep(50 * time.Millisecond)
		a.die()
		time.Sleep(200 * time.Millisecond)
		return status.Error(codes.Internal, "unreachable")
	}
	return s.Send(&healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING})
}

// reflectingApp is a cleartext HTTP/1.1 backend OUTSIDE the mesh that copies
// the request's x-aether-outcome header into its 200 response, as an echo or
// debug endpoint does with request headers.
type reflectingApp struct {
	ln   net.Listener
	mu   sync.Mutex
	seen map[string]int
}

func startReflectingApp(t *testing.T) *reflectingApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a := &reflectingApp{ln: ln, seen: map[string]int{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a.mu.Lock()
		a.seen[r.Method+" "+r.URL.RequestURI()]++
		a.mu.Unlock()
		if v := r.Header.Get(outcomeHeader); v != "" {
			w.Header().Set(outcomeHeader, v)
		}
		_, _ = io.WriteString(w, "legacy")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return a
}

func (a *reflectingApp) times(k string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[k]
}

// ------------------------------------------------------------------ the mesh

type outcomeMesh struct {
	source, previous, edge string
	good                   *recordingApp
	grpcBad, grpcGood      *grpcApp
	legacy                 *reflectingApp
}

func bypassOverload(l *listenerv3.Listener) *listenerv3.Listener {
	l.BypassOverloadManager = true
	return l
}

// startOutcomeMesh: this release's caller and the previous release's, in front
// of
//
//	ovl         a destination pod whose node proxy is in overload
//	            (stop_accepting_requests, the top-but-one rung of the chart's
//	            ladder) + a healthy pod
//	grpc        a real gRPC application that can die mid-call + a healthy one
//	grpcrefused a gRPC pod with nothing listening + a healthy one
//
// and an edge-style listener with one generated rule that splits between the
// mesh service "ovl" (healthy share only matters) and a cleartext Kubernetes
// Service.
//
// One Envoy plays every proxy, so "the destination's node is overloaded" is
// modelled by letting the overload manager act on that one inbound listener
// only: every other listener bypasses it.
func startOutcomeMesh(t *testing.T) *outcomeMesh {
	t.Helper()
	m := &outcomeMesh{
		good:     startRecordingApp(t, "good", true),
		grpcBad:  startGRPCApp(t, "bad"),
		grpcGood: startGRPCApp(t, "good"),
		legacy:   startReflectingApp(t),
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedAddr := closed.Addr().String()
	require.NoError(t, closed.Close())

	var clusters []*clusterv3.Cluster
	var listeners []*listenerv3.Listener
	var vhosts []*routev3.VirtualHost
	inbound := func(name string, bypass bool, app *clusterv3.Cluster) {
		pod := begunPod(name)
		l := productionInbound(t, pod, begunSock("r2-"+name), false, false)
		if bypass {
			bypassOverload(l)
		}
		listeners = append(listeners, l)
		if app != nil {
			app.Name = begunAppCluster(pod)
			app.LoadAssignment.ClusterName = app.Name
			clusters = append(clusters, app)
		}
	}
	service := func(name string, pods ...string) {
		addrs := make([]*corev3.Address, 0, len(pods))
		for _, p := range pods {
			addrs = append(addrs, begunSock("r2-"+p))
		}
		clusters = append(clusters, meshServiceCluster(t, begunService(name), false, "", addrs...))
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(begunService(name), []string{begunService(name)}))
	}

	inbound("good", true, staticCluster("x", false, tcpAddress(m.good.addr())))
	inbound("ovl", false, staticCluster("x", false, tcpAddress(m.good.addr())))
	service("ovl", "ovl", "good")

	inbound("grpcbad", true, staticCluster("x", true, tcpAddress(m.grpcBad.ln.Addr().String())))
	inbound("grpcgood", true, staticCluster("x", true, tcpAddress(m.grpcGood.ln.Addr().String())))
	inbound("grpcnone", true, staticCluster("x", true, tcpAddress(refusedAddr)))
	service("grpc", "grpcbad", "grpcgood")
	service("grpcrefused", "grpcnone", "grpcgood")

	// The edge: one generated rule, half to a mesh service, half to a
	// cleartext Kubernetes Service.
	k8s := proxy.EdgeK8sClusterName("demo", "legacy", 8080)
	clusters = append(clusters, staticCluster(k8s, false, tcpAddress(m.legacy.ln.Addr().String())))
	mixed := proxy.BuildEdgeRouteWeighted("/", "", nil, "", nil, []proxy.WeightedRouteBackend{
		{Cluster: begunService("ovl"), Weight: 1}, {Cluster: k8s, Weight: 1},
	}, nil, nil, nil, nil)
	// The edge's own per-Gateway HTTP listener (its connection manager with
	// the edge hardening), serving the generated route table inline instead of
	// over RDS.
	edge := proxy.BuildEdgeGatewayHTTPListener("demo", "gw", 8080, false, nil, nil)
	edge.Name, edge.Address, edge.BypassOverloadManager = "edge", socketAddress("127.0.0.1", envoyPicksPort), true
	eachHCM(t, edge, func(h *hcmv3.HttpConnectionManager) {
		require.NotNil(t, h.GetRds(), "the production edge connection manager takes its routes over RDS")
		h.RouteSpecifier = &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: proxy.BuildEdgeGatewayRouteConfiguration("demo", "gw",
			[]*routev3.VirtualHost{proxy.BuildEdgeVirtualHost("edge", []string{"*"}, []*routev3.Route{mixed})}, 0)}
	})

	listeners = append(listeners, edge,
		bypassOverload(productionOutbound(t, "source", proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain))),
		bypassOverload(productionOutbound(t, "source_previous", previousReleaseRoutes(proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain)))),
	)
	const heap = "envoy.resource_monitors.fixed_heap"
	bs := &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Listeners: listeners, Clusters: clusters},
		// charts/aether/templates/agent-proxy-configmap.yaml, the
		// stop_accepting_requests rung, with a heap limit that is always
		// exceeded.
		OverloadManager: &overloadv3.OverloadManager{
			RefreshInterval: durationpb.New(100 * time.Millisecond),
			ResourceMonitors: []*overloadv3.ResourceMonitor{{
				Name:       heap,
				ConfigType: &overloadv3.ResourceMonitor_TypedConfig{TypedConfig: config.TypedConfig(&fixedheapv3.FixedHeapConfig{MaxHeapSizeBytes: 1})},
			}},
			Actions: []*overloadv3.OverloadAction{{
				Name: "envoy.overload_actions.stop_accepting_requests",
				Triggers: []*overloadv3.Trigger{{Name: heap, TriggerOneof: &overloadv3.Trigger_Threshold{
					Threshold: &overloadv3.ThresholdTrigger{Value: 0.95},
				}}},
			}},
		},
	}
	e := runEnvoy(t, "outcome", bs, 1, "source", "source_previous", "edge")
	m.source, m.previous, m.edge = e.addrs["source"], e.addrs["source_previous"], e.addrs["edge"]
	return m
}

// direct asks a destination pod's inbound listener with no caller in front.
func (m *outcomeMesh) direct(t *testing.T, pod, method, path string) (status int, outcome []string, body string) {
	t.Helper()
	client := &http.Client{Timeout: envoyWait, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", begunSock("r2-"+pod).GetPipe().GetPath())
		},
	}}
	req, err := http.NewRequest(method, "http://destination"+path, strings.NewReader("payload"))
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Values(outcomeHeader), string(b)
}

func (m *outcomeMesh) via(t *testing.T, addr, host, method, uri string, hdr map[string]string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+addr+uri, strings.NewReader("payload"))
	require.NoError(t, err)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: envoyWait}).Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// rawStatusLine sends raw bytes to a destination pod's inbound listener and
// returns the response head it answers with.
func rawExchange(t *testing.T, pod, request string) string {
	t.Helper()
	c, err := net.Dial("unix", begunSock("r2-"+pod).GetPipe().GetPath())
	require.NoError(t, err)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(envoyWait))
	_, err = io.WriteString(c, request)
	require.NoError(t, err)
	b, _ := io.ReadAll(c)
	head, _, _ := strings.Cut(string(b), "\r\n\r\n")
	return head
}

// TestOverloadedDestinationIsShedToAnotherEndpoint: the chart's overload
// ladder (proxy.overload, on by default) ends in stop_accepting_requests, and
// says of it: "stop_accepting_requests 503s new streams, which the mesh's
// client retry policy retries on a DIFFERENT endpoint — an overloaded node
// sheds load to other replicas". That 503 is written by the connection manager
// before the filter chain exists; a caller that retries on the outcome header
// alone must still get one it recognises, for every method: the request never
// reached an application.
func TestOverloadedDestinationIsShedToAnotherEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy")
	}
	m := startOutcomeMesh(t)
	require.Eventually(t, func() bool {
		_, _, body := m.direct(t, "ovl", http.MethodPost, "/warm")
		return strings.Contains(body, "overloaded")
	}, envoyWait, 50*time.Millisecond, "the overload action never engaged")

	// What the overloaded proxy answers a client that is not a mesh caller
	// (no route is chosen for it): the mapper's stamp.
	st, outcome, body := m.direct(t, "ovl", http.MethodPost, "/direct")
	t.Logf("the overloaded destination answers: %d outcome=%q body=%q", st, outcome, body)
	assert.Equal(t, http.StatusServiceUnavailable, st)
	assert.Equal(t, []string{"503;-;POST;overload"}, outcome)

	// Other replies the connection manager writes itself, for the record: a
	// value, and one the caller's rule does not retry.
	head := rawExchange(t, "good", "NOT HTTP AT ALL\r\n\r\n")
	t.Logf("a malformed request is answered:\n%s", head)
	assert.Contains(t, head, " 400 ")
	assert.NotContains(t, strings.ToLower(head), outcomeHeader+": 503;")
	assert.NotContains(t, strings.ToLower(head), outcomeHeader+": 0;")

	for _, c := range []struct{ name, addr string }{{"caller of the previous release", m.previous}, {"caller of this release", m.source}} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(c.name+"/"+method, func(t *testing.T) {
				counts := map[int]int{}
				for i := range 12 {
					uri := fmt.Sprintf("/x?ovl=%s-%d&via=%s", method, i, c.addr)
					st, h := m.via(t, c.addr, begunService("ovl"), method, uri, nil)
					counts[st]++
					assert.Equalf(t, 1, m.good.times(method, uri), "%s: run exactly once, by the healthy endpoint", uri)
					if c.addr == m.source {
						assert.Emptyf(t, h.Values(outcomeHeader), "%s: the client saw %s", uri, outcomeHeader)
					}
				}
				t.Logf("%s, %s x12 to a service with one overloaded and one healthy endpoint: %v", c.name, method, counts)
				assert.Zerof(t, counts[http.StatusServiceUnavailable],
					"%d of 12 %s requests that no application ever received were answered 503 "+
						"instead of moving to the healthy endpoint", counts[http.StatusServiceUnavailable], method)
			})
		}
	}
}

// TestGRPCCallsWithARealClientAndServer: grpc-go on both ends (the standard
// health service), through both callers. A call the destination could not
// deliver is retried by this release's caller (aether#1646); a call the
// application had read when it died is never replayed, unary or streaming,
// before or after its first message; an UNAVAILABLE the application returns
// itself is the application's.
func TestGRPCCallsWithARealClientAndServer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy")
	}
	m := startOutcomeMesh(t)
	dial := func(addr, svc string) healthpb.HealthClient {
		cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithAuthority(begunService(svc)), grpc.WithDisableRetry())
		require.NoError(t, err)
		t.Cleanup(func() { _ = cc.Close() })
		return healthpb.NewHealthClient(cc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*envoyWait)
	defer cancel()

	type row struct {
		code          codes.Code
		atBad, atGood int
	}
	unary := func(c healthpb.HealthClient, call string) row {
		_, err := c.Check(ctx, &healthpb.HealthCheckRequest{Service: call})
		return row{status.Code(err), m.grpcBad.times(call), m.grpcGood.times(call)}
	}
	stream := func(c healthpb.HealthClient, call string) row {
		s, err := c.Watch(ctx, &healthpb.HealthCheckRequest{Service: call})
		if err == nil {
			for {
				if _, err = s.Recv(); err != nil {
					break
				}
			}
		}
		if err == io.EOF {
			err = nil
		}
		return row{status.Code(err), m.grpcBad.times(call), m.grpcGood.times(call)}
	}
	summarise := func(rows []row) string {
		c := map[string]int{}
		for _, r := range rows {
			c[fmt.Sprintf("%s(bad=%d,good=%d)", r.code, r.atBad, r.atGood)]++
		}
		return fmt.Sprint(c)
	}
	run := func(f func(healthpb.HealthClient, string) row, c healthpb.HealthClient, prefix string) []row {
		rows := make([]row, 0, 12)
		for i := range 12 {
			rows = append(rows, f(c, fmt.Sprintf("%s/%d", prefix, i)))
		}
		return rows
	}

	for _, rel := range []struct{ name, addr string }{{"previous", m.previous}, {"this", m.source}} {
		refused, svc := dial(rel.addr, "grpcrefused"), dial(rel.addr, "grpc")

		rows := run(unary, refused, "ok/refused-"+rel.name)
		t.Logf("[%s release caller] unary, one endpoint's application not listening: %s", rel.name, summarise(rows))
		if rel.name == "this" {
			for _, r := range rows {
				assert.Equal(t, codes.OK, r.code, "aether#1646: a call the destination could not deliver is retried")
				assert.Equal(t, 1, r.atGood)
			}
		}

		for _, k := range []struct {
			what   string
			f      func(healthpb.HealthClient, string) row
			prefix string
		}{
			{"unary, application dies after reading the call", unary, "die/u-"},
			{"server-streaming, application dies before its first message", stream, "die/s-"},
			{"server-streaming, application dies after its first message", stream, "dieafterfirst/s-"},
			{"unary, application answers UNAVAILABLE itself", unary, "unavailable/u-"},
		} {
			rows := run(k.f, svc, k.prefix+rel.name)
			t.Logf("[%s release caller] %s: %s", rel.name, k.what, summarise(rows))
			reached := 0
			for _, r := range rows {
				if r.atBad == 0 {
					continue
				}
				reached++
				assert.Equalf(t, 1, r.atBad, "%s: read more than once by the failing application", k.what)
				assert.Zerof(t, r.atGood, "%s: REPLAYED on the healthy endpoint", k.what)
			}
			assert.Positive(t, reached, k.what)
		}
	}
}

// TestEdgeClientCannotDriveARetryThroughACleartextBackend: an edge rule that
// splits between a mesh service and a cleartext Kubernetes Service takes the
// mesh retry policy for both shares, and nothing overwrites x-aether-outcome
// on the cleartext share. A cleartext backend that reflects request headers
// would let the EXTERNAL CLIENT decide the edge's retry, and have its own POST
// run again after a 200. The edge removes that request header.
func TestEdgeClientCannotDriveARetryThroughACleartextBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy")
	}
	m := startOutcomeMesh(t)
	reached, meshShare := 0, 0
	for i := range 16 {
		uri := fmt.Sprintf("/pay?edge=%d", i)
		st, h := m.via(t, m.edge, "shop.example", http.MethodPost, uri, map[string]string{outcomeHeader: "0;UF;POST;x"})
		assert.Equalf(t, http.StatusOK, st, "%s", uri)
		assert.Emptyf(t, h.Values(outcomeHeader), "%s: the external client saw %s", uri, outcomeHeader)
		n, mesh := m.legacy.times("POST "+uri), m.good.times(http.MethodPost, uri)
		assert.Equalf(t, 1, n+mesh, "%s: a POST answered 200 was executed %d times in total because the client sent %s", uri, n+mesh, outcomeHeader)
		reached += n
		meshShare += mesh
	}
	require.Positive(t, reached, "no request went to the cleartext share")
	require.Positive(t, meshShare, "no request went to the mesh share")
}
