package mtlspool

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// begunMark is the response header a destination marks a begun request with.
// Spelled out: Envoy's retry state reads this exact name.
const begunMark = "x-envoy-ratelimited"

const begunMeshDomain = "mesh.internal"

// recordingApp is an HTTP/1.1 application the test controls down to the
// socket, because the cases that matter are what it does to the CONNECTION:
// it reads a whole request and then closes, or resets, without answering.
// The request path chooses the behaviour; every request it reads is recorded
// by method and request URI, which is how the test knows whether a request
// was run once or twice.
type recordingApp struct {
	name string
	// alwaysOK makes it answer 200 to everything: the healthy endpoint a
	// retried request lands on.
	alwaysOK bool
	ln       net.Listener
	mu       sync.Mutex
	seen     map[string]int
}

func startRecordingApp(t *testing.T, name string, alwaysOK bool) *recordingApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a := &recordingApp{name: name, alwaysOK: alwaysOK, ln: ln, seen: map[string]int{}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go a.serve(c)
		}
	}()
	return a
}

func (a *recordingApp) addr() string { return a.ln.Addr().String() }

func (a *recordingApp) record(method, uri string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen[method+" "+uri]++
}

// times is how often the application read the request.
func (a *recordingApp) times(method, uri string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[method+" "+uri]
}

func (a *recordingApp) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		// The whole request, body included, before anything else: the cases
		// below are about a request the application HAS received.
		_, _ = io.Copy(io.Discard, req.Body)
		a.record(req.Method, req.URL.RequestURI())
		answer := func(status int, extraHeaders string) {
			body := a.name
			if req.Method == http.MethodHead {
				body = "" // a HEAD response carries the length and no body
			}
			fmt.Fprintf(c, "HTTP/1.1 %d X\r\ncontent-length: %d\r\n%s\r\n%s", status, len(a.name), extraHeaders, body)
		}
		switch path := req.URL.Path; {
		case a.alwaysOK:
			answer(http.StatusOK, "")
		case path == "/close":
			return // FIN after the request, no response
		case path == "/reset":
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // RST instead of FIN
			}
			return
		case path == "/app503":
			answer(http.StatusServiceUnavailable, "")
		case path == "/forged503":
			// An application trying to switch its own 503 out of the retry.
			answer(http.StatusServiceUnavailable, begunMark+": true\r\n")
		case path == "/forged200":
			answer(http.StatusOK, begunMark+": true\r\n")
		default:
			answer(http.StatusOK, "")
		}
	}
}

// startAbortingH2App is an h2c application whose handler aborts the stream
// after reading the request (RST_STREAM), which the proxy reports as UR where
// a closed HTTP/1.1 connection is UC.
func startAbortingH2App(t *testing.T) *recordingApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a := &recordingApp{name: "h2", ln: ln, seen: map[string]int{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a.record(r.Method, r.URL.RequestURI())
		panic(http.ErrAbortHandler)
	})}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetUnencryptedHTTP2(true)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return a
}

func pipeAddress(name string) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{Path: name}}}
}

func tcpAddress(hostPort string) *corev3.Address {
	return staticEndpoint("", hostPort).GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
}

func staticCluster(name string, h2 bool, addrs ...*corev3.Address) *clusterv3.Cluster {
	eps := make([]*endpointv3.LbEndpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, &endpointv3.LbEndpoint{HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: a}}})
	}
	c := &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(envoyWait),
		LoadAssignment:       &endpointv3.ClusterLoadAssignment{ClusterName: name, Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: eps}}},
	}
	if h2 {
		c.TypedExtensionProtocolOptions = map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		}
	}
	return c
}

// begunPod is one destination pod of the test.
func begunPod(name string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name: name, Namespace: "demo", ServiceAccount: "echo",
		NetworkNamespace: "/var/run/netns/" + name, ContainerId: "c-" + name, Ips: []string{"10.0.0.7"},
	}
}

// productionInbound is the pod's inbound listener exactly as the agent builds
// it for a mesh without SPIRE (one cleartext HTTP connection manager chain:
// the same connection manager, filters and route as the mTLS and HTTP/3
// chains), moved from the pod's network namespace onto a socket the test's
// source cluster can dial. previousRelease removes what aether#1641 added, to
// stand in for a destination proxy whose agent has not been upgraded.
func productionInbound(t *testing.T, pod *cniv1.CNIPod, addr *corev3.Address, previousRelease bool) *listenerv3.Listener {
	t.Helper()
	l, err := proxy.NewInboundListener(pod, trustDomain, false, true, nil, nil)
	require.NoError(t, err)
	l.Address = addr
	if !previousRelease {
		return l
	}
	l.Name += "_previous"
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			var h hcmv3.HttpConnectionManager
			if !f.GetTypedConfig().MessageIs(&h) {
				continue
			}
			require.NoError(t, f.GetTypedConfig().UnmarshalTo(&h))
			h.LocalReplyConfig = nil
			for _, vh := range h.GetRouteConfig().GetVirtualHosts() {
				vh.ResponseHeadersToRemove = nil
			}
			f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(&h)}
		}
	}
	return l
}

// begunMesh is a source proxy and a set of destination pods, all generated by
// the agent's own builders, in one Envoy.
type begunMesh struct {
	source string // the source listener's address
	good   *recordingApp
	bad    *recordingApp
	h2     *recordingApp
}

// begunCases are the destination pods. Each is one endpoint of a two-endpoint
// service whose other endpoint is the healthy pod "good", so a retry on
// another endpoint always has somewhere to succeed.
const (
	caseApp       = "app"       // an application the test scripts (HTTP/1.1)
	caseH2        = "h2"        // an h2c application that aborts the stream
	caseRefused   = "refused"   // nothing listens on the application port
	caseUnhealthy = "unhealthy" // the application cluster has no healthy host
	caseNoCluster = "nocluster" // the application cluster is not there
	casePrevious  = "previous"  // caseApp behind a destination of the previous release
)

func startBegunMesh(t *testing.T) *begunMesh {
	t.Helper()
	m := &begunMesh{
		good: startRecordingApp(t, "good", true),
		bad:  startRecordingApp(t, "bad", false),
		h2:   startAbortingH2App(t),
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedAddr := closed.Addr().String()
	require.NoError(t, closed.Close())

	// Abstract Unix sockets for the destination listeners: the source cluster
	// has to name them in the same bootstrap, before any port exists.
	sock := func(name string) *corev3.Address {
		return pipeAddress(fmt.Sprintf("@aether-1641-%d-%s", os.Getpid(), name))
	}
	appCluster := func(pod *cniv1.CNIPod) string { return proxy.AppClusterName(pod, proxy.AppPortFromPod(pod)) }

	goodPod := begunPod("good")
	listeners := []*listenerv3.Listener{productionInbound(t, goodPod, sock("good"), false)}
	clusters := []*clusterv3.Cluster{staticCluster(appCluster(goodPod), false, tcpAddress(m.good.addr()))}
	var vhosts []*routev3.VirtualHost

	for _, c := range []string{caseApp, caseH2, caseRefused, caseUnhealthy, caseNoCluster, casePrevious} {
		pod := begunPod(c)
		listeners = append(listeners, productionInbound(t, pod, sock(c), c == casePrevious))
		switch c {
		case caseApp, casePrevious:
			clusters = append(clusters, staticCluster(appCluster(pod), false, tcpAddress(m.bad.addr())))
		case caseH2:
			clusters = append(clusters, staticCluster(appCluster(pod), true, tcpAddress(m.h2.addr())))
		case caseRefused:
			clusters = append(clusters, staticCluster(appCluster(pod), false, tcpAddress(refusedAddr)))
		case caseUnhealthy:
			cl := staticCluster(appCluster(pod), false, tcpAddress(m.good.addr()))
			cl.LoadAssignment.Endpoints[0].LbEndpoints[0].HealthStatus = corev3.HealthStatus_UNHEALTHY
			cl.CommonLbConfig = &clusterv3.Cluster_CommonLbConfig{HealthyPanicThreshold: &typev3.Percent{Value: 0}}
			clusters = append(clusters, cl)
		case caseNoCluster:
			// no application cluster at all
		}
		// The caller's side: the service's cluster (this pod and the healthy
		// one) and the agent's outbound virtual host for it.
		service := c + ".demo." + begunMeshDomain
		clusters = append(clusters, staticCluster(service, true, sock(c), sock("good")))
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(service, []string{service}))
	}

	source := &listenerv3.Listener{
		Name:    "source",
		Address: socketAddress("127.0.0.1", envoyPicksPort),
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{{
			Name: "envoy.filters.network.http_connection_manager",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(func() *hcmv3.HttpConnectionManager {
				h := appHCM("source", hcmv3.HttpConnectionManager_AUTO, nil)
				// The agent's outbound route table, retry policy included.
				h.RouteSpecifier = &hcmv3.HttpConnectionManager_RouteConfig{
					RouteConfig: proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain),
				}
				return h
			}())},
		}}}},
	}
	bs := &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Id: "begun", Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Listeners: append(listeners, source), Clusters: clusters},
	}
	m.source = runEnvoy(t, "begun", bs, 1, "source").addrs["source"]
	return m
}

// begunResult is what the client and the applications saw for one request.
type begunResult struct {
	uri       string
	status    int
	sawMark   bool
	atFailing int // times the failing endpoint's application read it
	atGood    int // times the healthy endpoint's application read it
}

// send issues n requests of one kind through the source proxy and reports each.
// Every request has its own URI, so an application reading the same one twice
// is a replay and nothing else.
func (m *begunMesh) send(t *testing.T, failing *recordingApp, service, method, path string, n int) []begunResult {
	t.Helper()
	client := &http.Client{Timeout: envoyWait}
	out := make([]begunResult, 0, n)
	for i := range n {
		uri := fmt.Sprintf("%s?case=%s&m=%s&i=%d", path, service, method, i)
		var body io.Reader
		if method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut {
			body = strings.NewReader("payload")
		}
		req, err := http.NewRequest(method, "http://"+m.source+uri, body)
		require.NoError(t, err)
		req.Host = service + ".demo." + begunMeshDomain
		resp, err := client.Do(req)
		require.NoErrorf(t, err, "%s %s", method, uri)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		r := begunResult{uri: uri, status: resp.StatusCode, sawMark: len(resp.Header.Values(begunMark)) > 0, atGood: m.good.times(method, uri)}
		if failing != nil {
			r.atFailing = failing.times(method, uri)
		}
		out = append(out, r)
	}
	return out
}

func statusCounts(rs []begunResult) string {
	counts := map[int]int{}
	for _, r := range rs {
		counts[r.status]++
	}
	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d x%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// TestBegunRequestIsNotReplayed runs the agent's caller-side route table
// against the agent's inbound connection manager on the pinned proxy
// (aether#1641).
//
// Before the fix a destination proxy that had already sent a request to its
// application, and then lost the application's connection, answered 503, and
// the caller retried that 503 on another endpoint whatever the method: the
// application could run a POST twice. The destination now marks that 503 and
// the caller does not retry a marked response; the status code is unchanged.
// Everything else the caller retried, it still retries.
func TestBegunRequestIsNotReplayed(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	m := startBegunMesh(t)
	// Round-robin over two endpoints: about half of these start at the
	// failing one. Each assertion that needs at least one such request says so.
	const n = 12

	// A request that had begun to the application and got no answer.
	begun := []struct {
		name    string
		service string
		path    string
		app     *recordingApp
	}{
		{"application closes the connection (UC)", caseApp, "/close", m.bad},
		{"application resets the connection (UC)", caseApp, "/reset", m.bad},
		{"application resets the stream (UR)", caseH2, "/rst", m.h2},
	}
	for _, tc := range begun {
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			t.Run(tc.name+"/"+method+" is answered 503 and never replayed", func(t *testing.T) {
				rs := m.send(t, tc.app, tc.service, method, tc.path, n)
				t.Logf("%s %s: %s", method, tc.path, statusCounts(rs))
				reached := 0
				for _, r := range rs {
					assert.Falsef(t, r.sawMark, "%s: the client saw %s", r.uri, begunMark)
					if r.atFailing == 0 {
						assert.Equalf(t, http.StatusOK, r.status, "%s never reached the failing application", r.uri)
						continue
					}
					reached++
					assert.Equalf(t, 1, r.atFailing, "%s: read by the failing application more than once", r.uri)
					assert.Zerof(t, r.atGood, "%s: the application of one endpoint read it and got no answer out, "+
						"and the request was then run AGAIN on another endpoint", r.uri)
					assert.Equalf(t, http.StatusServiceUnavailable, r.status, "%s: the client must get the destination's 503", r.uri)
				}
				assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
			})
		}
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead, http.MethodOptions} {
			t.Run(tc.name+"/"+method+" is idempotent and still retried", func(t *testing.T) {
				rs := m.send(t, tc.app, tc.service, method, tc.path, n)
				t.Logf("%s %s: %s", method, tc.path, statusCounts(rs))
				reached := 0
				for _, r := range rs {
					assert.Falsef(t, r.sawMark, "%s: the client saw %s", r.uri, begunMark)
					assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
					if r.atFailing > 0 {
						reached++
						assert.Equalf(t, 1, r.atGood, "%s: retried on the other endpoint exactly once", r.uri)
					}
				}
				assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
			})
		}
	}

	// The destination never reached its application: safe for every method,
	// and what keeps a pod roll hitless.
	for _, service := range []string{caseRefused, caseUnhealthy, caseNoCluster} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run("application never reached ("+service+")/"+method+" is retried", func(t *testing.T) {
				rs := m.send(t, nil, service, method, "/any", n)
				t.Logf("%s %s: %s", method, service, statusCounts(rs))
				for _, r := range rs {
					assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
					assert.Equalf(t, 1, r.atGood, "%s", r.uri)
					assert.Falsef(t, r.sawMark, "%s: the client saw %s", r.uri, begunMark)
				}
			})
		}
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		// The application answered 503 itself: it asked for another endpoint.
		t.Run("application answers 503/"+method+" is retried", func(t *testing.T) {
			rs := m.send(t, m.bad, caseApp, method, "/app503", n)
			reached := 0
			for _, r := range rs {
				assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
				if r.atFailing > 0 {
					reached++
					assert.Equalf(t, 1, r.atGood, "%s", r.uri)
				}
			}
			assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
		})
		// The mark is the proxy's: an application that sets the header on its
		// own 503 does not switch the retry off.
		t.Run("application forges the mark on its 503/"+method+" is still retried", func(t *testing.T) {
			rs := m.send(t, m.bad, caseApp, method, "/forged503", n)
			reached := 0
			for _, r := range rs {
				assert.Equalf(t, http.StatusOK, r.status, "%s: an application's own header decided the mesh's retry", r.uri)
				assert.Falsef(t, r.sawMark, "%s: the client saw %s", r.uri, begunMark)
				if r.atFailing > 0 {
					reached++
				}
			}
			assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
		})
		t.Run("a 200 is never retried/"+method, func(t *testing.T) {
			for _, path := range []string{"/ok", "/forged200"} {
				rs := m.send(t, m.bad, caseApp, method, path, n)
				for _, r := range rs {
					assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
					assert.Equalf(t, 1, r.atFailing+r.atGood, "%s: read by exactly one application", r.uri)
					assert.Falsef(t, r.sawMark, "%s: the client saw %s", r.uri, begunMark)
				}
			}
		})
	}

	// A destination of the previous release does not mark anything. Its 503s
	// are retried as they always were: an upgrade or a rollback in progress
	// degrades to the old behaviour, never to "no retry".
	t.Run("destination of the previous release", func(t *testing.T) {
		for _, tc := range []struct {
			method, path string
		}{{http.MethodPost, "/close"}, {http.MethodPost, "/app503"}, {http.MethodGet, "/close"}} {
			rs := m.send(t, m.bad, casePrevious, tc.method, tc.path, n)
			reached := 0
			for _, r := range rs {
				assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
				if r.atFailing > 0 {
					reached++
					assert.Equalf(t, 1, r.atGood, "%s", r.uri)
				}
			}
			assert.Positivef(t, reached, "%s %s: no request started at the failing endpoint", tc.method, tc.path)
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rs := m.send(t, nil, casePrevious, method, "/ok", n)
			for _, r := range rs {
				assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
			}
		}
	})
}
