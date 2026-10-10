package mtlspool

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
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
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// outcomeHeader is the response header a destination proxy reports what
// happened in, and the caller's retry policy reads. Spelled out, like the old
// policy below: both are what an agent of another release has on the wire.
const (
	outcomeHeader     = "x-aether-outcome"
	rateLimitedHeader = "x-envoy-ratelimited"

	begunMeshDomain = "mesh.internal"
)

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
		case path == "/halfclose":
			// The application stops writing and keeps reading.
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
				_, _ = io.Copy(io.Discard, br)
			}
			return
		case path == "/reset":
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // RST instead of FIN
			}
			return
		case path == "/app503":
			answer(http.StatusServiceUnavailable, "")
		case path == "/app500":
			answer(http.StatusInternalServerError, "")
		case path == "/forged503":
			// An application trying to write its own outcome: "this was a 200".
			answer(http.StatusServiceUnavailable, outcomeHeader+": 200;-;GET;via_upstream\r\n")
		case path == "/forged200":
			// ... and "retry this 200".
			answer(http.StatusOK, outcomeHeader+": 503;-;GET;via_upstream\r\n")
		case path == "/ratelimited503":
			// Envoy's own "do not retry this" response header, set by an
			// application. The mesh has no opinion about it.
			answer(http.StatusServiceUnavailable, rateLimitedHeader+": true\r\n")
		default:
			answer(http.StatusOK, "")
		}
	}
}

// startH2App is an h2c (prior knowledge) application written at the frame
// level, because what it must do is answer a stream with RST_STREAM and a
// chosen error code:
//
//   - with alwaysOK set (read here as "refuse every stream"): REFUSED_STREAM
//     (7) as soon as the HEADERS frame is read. RFC 9113, 8.7: the request was
//     not processed and "can be safely retried";
//   - otherwise: the whole request is read and the stream is reset with
//     INTERNAL_ERROR (2): the application worked on it and died.
//
// Envoy reports both as response flag UR.
func startH2App(t *testing.T) *recordingApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a := &recordingApp{name: "h2", ln: ln, seen: map[string]int{}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go a.serveH2(c)
		}
	}()
	return a
}

func (a *recordingApp) serveH2(c net.Conn) {
	defer c.Close()
	const (
		frameData, frameHeaders, frameRST, frameSettings, framePing = 0, 1, 3, 4, 6
		flagEndStream, flagAck                                      = 1, 1
	)
	if _, err := io.ReadFull(c, make([]byte, 24)); err != nil { // client preface
		return
	}
	write := func(typ, flags byte, stream uint32, payload []byte) {
		h := make([]byte, 9, 9+len(payload))
		h[0], h[1], h[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
		h[3], h[4] = typ, flags
		binary.BigEndian.PutUint32(h[5:], stream)
		_, _ = c.Write(append(h, payload...))
	}
	reset := func(stream uint32, code byte) { write(frameRST, 0, stream, []byte{0, 0, 0, code}) }
	write(frameSettings, 0, 0, nil)
	hdr := make([]byte, 9)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		payload := make([]byte, int(hdr[0])<<16|int(hdr[1])<<8|int(hdr[2]))
		if _, err := io.ReadFull(c, payload); err != nil {
			return
		}
		typ, flags := hdr[3], hdr[4]
		stream := binary.BigEndian.Uint32(hdr[5:]) & 0x7fffffff
		switch typ {
		case frameSettings:
			if flags&flagAck == 0 {
				write(frameSettings, flagAck, 0, nil)
			}
		case framePing:
			if flags&flagAck == 0 {
				write(framePing, flagAck, 0, payload)
			}
		case frameHeaders:
			// No HPACK decoder here, so the behaviour is per application and
			// not per path, and requests are counted, not recorded by URI.
			a.mu.Lock()
			refuse := a.alwaysOK // for an h2c application: refuse every stream
			a.seen["streams"]++
			a.mu.Unlock()
			if refuse {
				reset(stream, 7) // REFUSED_STREAM
			} else if flags&flagEndStream != 0 {
				reset(stream, 2) // INTERNAL_ERROR
			}
		case frameData:
			if flags&flagEndStream != 0 {
				reset(stream, 2)
			}
		}
	}
}

func pipeAddress(name string) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{Path: name}}}
}

func tcpAddress(hostPort string) *corev3.Address {
	return staticEndpoint("", hostPort).GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
}

func staticCluster(name string, h2 bool, addrs ...*corev3.Address) *clusterv3.Cluster {
	c := &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(envoyWait),
		LoadAssignment:       staticAssignment(name, addrs...),
	}
	if h2 {
		c.TypedExtensionProtocolOptions = map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		}
	}
	return c
}

func staticAssignment(name string, addrs ...*corev3.Address) *endpointv3.ClusterLoadAssignment {
	eps := make([]*endpointv3.LbEndpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, &endpointv3.LbEndpoint{HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: a}}})
	}
	return &endpointv3.ClusterLoadAssignment{ClusterName: name, Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: eps}}}
}

// meshServiceCluster is the caller's cluster for a mesh service exactly as the
// agent builds it (proxy.NewServiceCluster: h2 with the mesh keep-alive),
// turned from EDS to STATIC so that it resolves without a control plane and
// with outlier detection off (see below). With
// mtls it also carries production's upstream mTLS (proxy.InjectUpstreamMTLS),
// its SDS sources repointed at the harness.
func meshServiceCluster(t *testing.T, name string, mtls bool, sni string, addrs ...*corev3.Address) *clusterv3.Cluster {
	t.Helper()
	cl := proxy.NewServiceCluster(name, name, name, nil)
	cl.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
	cl.EdsClusterConfig = nil
	cl.LbSubsetConfig = nil
	cl.LoadAssignment = staticAssignment(name, addrs...)
	// Production ejects an endpoint after five consecutive 5xx. Here that would
	// take the endpoint under test out of rotation a few requests into the
	// first case, and every later case would test nothing.
	cl.OutlierDetection = nil
	if mtls {
		proxy.InjectUpstreamMTLS(cl, spiffeNode, validationContextName, []string{spiffeDest}, sni, "")
		require.NotNil(t, cl.GetTransportSocket())
		rewriteSDSToHarness(t, cl.GetTransportSocket())
	}
	return cl
}

// begunPod is one destination pod of the test. Every pod has the identity
// spiffeDest (namespace demo, ServiceAccount echo).
func begunPod(name string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name: name, Namespace: "demo", ServiceAccount: "echo",
		NetworkNamespace: "/var/run/netns/" + name, ContainerId: "c-" + name, Ips: []string{"10.0.0.7"},
	}
}

// eachHCM calls edit on every HTTP connection manager of l and stores the
// result back.
func eachHCM(t *testing.T, l *listenerv3.Listener, edit func(*hcmv3.HttpConnectionManager)) {
	t.Helper()
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			var h hcmv3.HttpConnectionManager
			if !f.GetTypedConfig().MessageIs(&h) {
				continue
			}
			require.NoError(t, f.GetTypedConfig().UnmarshalTo(&h))
			edit(&h)
			f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(&h)}
		}
	}
}

// productionInbound is the pod's inbound listener exactly as the agent builds
// it, moved from the pod's network namespace onto a socket the test's source
// cluster can dial.
//
// Without mtls it is the listener of a mesh without SPIRE: one cleartext HTTP
// connection manager chain. With mtls it is the default one: every chain
// terminates mTLS with the pod's certificate and requires the caller's, both
// over SDS (repointed at the harness's SDS server, the one substitution).
//
// previousRelease removes what aether#1641 added, to stand in for a
// destination proxy whose agent has not been upgraded.
func productionInbound(t *testing.T, pod *cniv1.CNIPod, addr *corev3.Address, mtls, previousRelease bool) *listenerv3.Listener {
	t.Helper()
	l, err := proxy.NewInboundListener(pod, trustDomain, false, !mtls, nil, nil)
	require.NoError(t, err)
	l.Address = addr
	if mtls {
		explicit := config.SDSConfigSourceFromCluster(sdsClusterName)
		for _, fc := range l.GetFilterChains() {
			var ctx tlsv3.DownstreamTlsContext
			require.NoError(t, fc.GetTransportSocket().GetTypedConfig().UnmarshalTo(&ctx))
			for _, sc := range ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs() {
				sc.SdsConfig = explicit
			}
			require.NotNil(t, ctx.GetCommonTlsContext().GetCombinedValidationContext(), "the inbound pins the caller's SAN shape")
			ctx.GetCommonTlsContext().GetCombinedValidationContext().ValidationContextSdsSecretConfig.SdsConfig = explicit
			fc.TransportSocket.ConfigType = &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(&ctx)}
		}
	}
	if previousRelease {
		eachHCM(t, l, func(h *hcmv3.HttpConnectionManager) {
			for _, vh := range h.GetRouteConfig().GetVirtualHosts() {
				vh.ResponseHeadersToAdd = nil
			}
		})
	}
	return l
}

// productionOutbound is a pod's outbound listener as the agent builds it
// (proxy.GenerateOutboundHTTPListener: the source filter states and the
// outbound connection manager with its own settings, which is what decides
// which request headers a client application may use), on a loopback port,
// serving routes inline instead of over RDS. Two of its HTTP filters are
// configured over the agent's ADS stream (the subset-header filter over ECDS,
// the on-demand cluster filter over ODCDS) and are left out: this harness has
// no ADS, and neither touches a response.
func productionOutbound(t *testing.T, name string, routes *routev3.RouteConfiguration) *listenerv3.Listener {
	t.Helper()
	pod := &cniv1.CNIPod{Name: name, Namespace: "demo", ServiceAccount: "source-a", NetworkNamespace: "/var/run/netns/" + name}
	l, err := proxy.GenerateOutboundHTTPListener(pod, spiffeSourceA, begunMeshDomain, false, nil)
	require.NoError(t, err)
	l.Name = name
	l.Address = socketAddress("127.0.0.1", envoyPicksPort)
	onDemand := proxy.OnDemandHTTPFilter().GetName()
	eachHCM(t, l, func(h *hcmv3.HttpConnectionManager) {
		require.NotNil(t, h.GetRds(), "the production outbound connection manager takes its routes over RDS")
		h.RouteSpecifier = &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: routes}
		kept := h.HttpFilters[:0]
		for _, f := range h.GetHttpFilters() {
			if f.GetConfigDiscovery() != nil || f.GetName() == onDemand {
				continue
			}
			kept = append(kept, f)
		}
		h.HttpFilters = kept
	})
	return l
}

// previousReleaseRoutes rewrites a route table into what the previous release
// served: every retrying route retries a 503 by status code and forwards the
// destination's headers untouched.
func previousReleaseRoutes(rc *routev3.RouteConfiguration) *routev3.RouteConfiguration {
	for _, vh := range rc.GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			rp := r.GetRoute().GetRetryPolicy()
			if rp == nil {
				continue
			}
			rp.RetryOn = "connect-failure,refused-stream,reset-before-request,retriable-status-codes"
			rp.RetriableStatusCodes = []uint32{503}
			rp.RetriableHeaders = nil
			r.ResponseHeadersToRemove = nil
		}
	}
	return rc
}

// begunMesh is two source proxies (this release's and the previous one's) and
// a set of destination pods, all generated by the agent's own builders, in one
// Envoy.
type begunMesh struct {
	source   string // this release's caller
	previous string // a caller of the previous release
	good     *recordingApp
	bad      *recordingApp
	h2       *recordingApp // resets every stream after reading the request
	refusing *recordingApp // refuses every stream
}

// The destination pods. Each is one endpoint of a two-endpoint service whose
// other endpoint is a healthy pod, so a retry on another endpoint always has
// somewhere to succeed.
const (
	caseApp       = "app"       // an application the test scripts (HTTP/1.1)
	caseH2        = "h2"        // an h2c application that resets the stream after reading it
	caseRefusing  = "refusing"  // an h2c application that refuses the stream
	caseRefused   = "refused"   // nothing listens on the application port
	caseUnhealthy = "unhealthy" // the application cluster has no healthy host
	caseNoCluster = "nocluster" // the application cluster is not there
	casePrevious  = "previous"  // caseApp behind a destination of the previous release
	caseMTLS      = "mtls"      // caseApp behind the mTLS inbound, with an mTLS healthy pod
)

func begunSock(name string) *corev3.Address {
	// Abstract Unix sockets for the destination listeners: the source cluster
	// has to name them in the same bootstrap, before any port exists.
	return pipeAddress(fmt.Sprintf("@aether-1641-%d-%s", os.Getpid(), name))
}

func begunService(c string) string { return c + ".demo." + begunMeshDomain }

func begunAppCluster(pod *cniv1.CNIPod) string {
	return proxy.AppClusterName(pod, proxy.AppPortFromPod(pod))
}

// startBegunSDS serves what the mTLS case needs: the caller's client
// certificate (selected per connection from the source identity), the node's,
// the destination pods' server certificate, and the trust bundle.
func startBegunSDS(t *testing.T, p *pki) string {
	t.Helper()
	secrets := secretResources(t, p, []string{spiffeSourceA, spiffeNode})
	certPath, keyPath := p.leaf(t, "begun-dest", spiffeDest, true)
	secrets = append(secrets, &tlsv3.Secret{
		Name: spiffeDest,
		Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
			CertificateChain: fileDataSource(certPath), PrivateKey: fileDataSource(keyPath),
		}},
	})
	snapshot, err := cachev3.NewSnapshot("1", map[resourcev3.Type][]types.Resource{resourcev3.SecretType: secrets})
	require.NoError(t, err)
	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	require.NoError(t, cache.SetSnapshot(context.Background(), envoyNodeID, snapshot))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), cache, nil))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	return ln.Addr().String()
}

func startBegunMesh(t *testing.T) *begunMesh {
	t.Helper()
	m := &begunMesh{
		good:     startRecordingApp(t, "good", true),
		bad:      startRecordingApp(t, "bad", false),
		h2:       startH2App(t),
		refusing: startH2App(t),
	}
	m.refusing.alwaysOK = true // see serveH2: refuse every stream
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedAddr := closed.Addr().String()
	require.NoError(t, closed.Close())

	p := newPKI(t)
	clusters := []*clusterv3.Cluster{sdsCluster(startBegunSDS(t, p))}
	var listeners []*listenerv3.Listener
	var vhosts []*routev3.VirtualHost

	// The healthy pod, behind the cleartext inbound and behind the mTLS one.
	for name, mtls := range map[string]bool{"good": false, "mtls-good": true} {
		pod := begunPod(name)
		listeners = append(listeners, productionInbound(t, pod, begunSock(name), mtls, false))
		clusters = append(clusters, staticCluster(begunAppCluster(pod), false, tcpAddress(m.good.addr())))
	}

	for _, c := range []string{caseApp, caseH2, caseRefusing, caseRefused, caseUnhealthy, caseNoCluster, casePrevious, caseMTLS} {
		pod := begunPod(c)
		mtls := c == caseMTLS
		listeners = append(listeners, productionInbound(t, pod, begunSock(c), mtls, c == casePrevious))
		switch c {
		case caseApp, casePrevious, caseMTLS:
			clusters = append(clusters, staticCluster(begunAppCluster(pod), false, tcpAddress(m.bad.addr())))
		case caseH2:
			clusters = append(clusters, staticCluster(begunAppCluster(pod), true, tcpAddress(m.h2.addr())))
		case caseRefusing:
			clusters = append(clusters, staticCluster(begunAppCluster(pod), true, tcpAddress(m.refusing.addr())))
		case caseRefused:
			clusters = append(clusters, staticCluster(begunAppCluster(pod), false, tcpAddress(refusedAddr)))
		case caseUnhealthy:
			cl := staticCluster(begunAppCluster(pod), false, tcpAddress(m.good.addr()))
			cl.LoadAssignment.Endpoints[0].LbEndpoints[0].HealthStatus = corev3.HealthStatus_UNHEALTHY
			cl.CommonLbConfig = &clusterv3.Cluster_CommonLbConfig{HealthyPanicThreshold: &typev3.Percent{Value: 0}}
			clusters = append(clusters, cl)
		case caseNoCluster:
			// no application cluster at all
		}
		// The caller's side: the service's cluster (this pod and the healthy
		// one) and the agent's outbound virtual host for it.
		healthy := "good"
		if mtls {
			healthy = "mtls-good"
		}
		sni := strconv.Itoa(int(proxy.AppPortFromPod(pod))) // the destination port, as production sets it
		clusters = append(clusters, meshServiceCluster(t, begunService(c), mtls, sni, begunSock(c), begunSock(healthy)))
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(begunService(c), []string{begunService(c)}))
	}

	listeners = append(listeners,
		productionOutbound(t, "source", proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain)),
		productionOutbound(t, "source_previous", previousReleaseRoutes(proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain))),
	)
	bs := &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Listeners: listeners, Clusters: clusters},
	}
	e := runEnvoy(t, "begun", bs, 1, "source", "source_previous")
	m.source, m.previous = e.addrs["source"], e.addrs["source_previous"]
	return m
}

// begunResult is what the client and the applications saw for one request.
type begunResult struct {
	uri         string
	status      int
	outcome     []string // the outcome header as the client received it (never, through this release's caller)
	rateLimited bool
	grpcStatus  string
	atFailing   int // times the failing endpoint's application read it
	atGood      int // times the healthy endpoint's application read it
}

// begunRequest is one kind of request.
type begunRequest struct {
	via     string // the caller's address; "" is this release's
	service string
	method  string
	path    string
	header  map[string]string
	failing *recordingApp // the application behind the endpoint under test, if it records per request
}

// send issues n requests of one kind through a caller and reports each. Every
// request has its own URI, so an application reading the same one twice is a
// replay and nothing else.
func (m *begunMesh) send(t *testing.T, rq begunRequest, n int) []begunResult {
	t.Helper()
	via := rq.via
	if via == "" {
		via = m.source
	}
	client := &http.Client{Timeout: envoyWait}
	out := make([]begunResult, 0, n)
	for i := range n {
		uri := fmt.Sprintf("%s?case=%s&m=%s&via=%s&i=%d", rq.path, rq.service, rq.method, via, i)
		for k := range rq.header {
			uri += "&h=" + k
		}
		var body io.Reader
		if rq.method == http.MethodPost || rq.method == http.MethodPatch || rq.method == http.MethodPut {
			body = strings.NewReader("payload")
		}
		req, err := http.NewRequest(rq.method, "http://"+via+uri, body)
		require.NoError(t, err)
		req.Host = begunService(rq.service)
		for k, v := range rq.header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		require.NoErrorf(t, err, "%s %s", rq.method, uri)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		grpcStatus := resp.Header.Get("grpc-status")
		if grpcStatus == "" {
			grpcStatus = resp.Trailer.Get("grpc-status")
		}
		r := begunResult{
			uri: uri, status: resp.StatusCode, outcome: resp.Header.Values(outcomeHeader),
			rateLimited: len(resp.Header.Values(rateLimitedHeader)) > 0, grpcStatus: grpcStatus,
			atGood: m.good.times(rq.method, uri),
		}
		if rq.failing != nil {
			r.atFailing = rq.failing.times(rq.method, uri)
		}
		out = append(out, r)
	}
	return out
}

// atDestination asks a destination pod's cleartext inbound listener directly,
// with no caller in front: what the destination proxy itself answers.
func (m *begunMesh) atDestination(t *testing.T, pod, method, path string, header map[string]string) (status int, outcome string) {
	t.Helper()
	client := &http.Client{Timeout: envoyWait, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", begunSock(pod).GetPipe().GetPath())
		},
	}}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("payload")
	}
	req, err := http.NewRequest(method, "http://destination"+path, body)
	require.NoError(t, err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	require.NoErrorf(t, err, "%s %s at %s", method, path, pod)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	values := resp.Header.Values(outcomeHeader)
	require.Lenf(t, values, 1, "%s %s at %s: exactly one %s header, got %q", method, path, pod, outcomeHeader, values)
	return resp.StatusCode, values[0]
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

// begunN: round-robin over two endpoints, so about half of these start at the
// failing one. Each assertion that needs at least one such request says so.
const begunN = 12

// requireRetried: every request ended 200 on the healthy endpoint, and the
// client saw no outcome header.
func requireRetried(t *testing.T, rs []begunResult) {
	t.Helper()
	for _, r := range rs {
		assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
		assert.Equalf(t, 1, r.atGood, "%s: read by the healthy application exactly once", r.uri)
		assert.Emptyf(t, r.outcome, "%s: the client saw %s", r.uri, outcomeHeader)
	}
}

// requireNotReplayed: a request the failing application read was read once,
// by nobody else, and the client got wantStatus for it; the others went
// straight to the healthy endpoint.
func requireNotReplayed(t *testing.T, rs []begunResult, wantStatus int) {
	t.Helper()
	reached := 0
	for _, r := range rs {
		assert.Emptyf(t, r.outcome, "%s: the client saw %s", r.uri, outcomeHeader)
		if r.atFailing == 0 {
			assert.Equalf(t, http.StatusOK, r.status, "%s never reached the failing application", r.uri)
			continue
		}
		reached++
		assert.Equalf(t, 1, r.atFailing, "%s: read by the failing application more than once", r.uri)
		assert.Zerof(t, r.atGood, "%s: the application of one endpoint read it and got no answer out, "+
			"and the request was then run AGAIN on another endpoint", r.uri)
		assert.Equalf(t, wantStatus, r.status, "%s: the client must get the destination's answer", r.uri)
	}
	assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
}

// TestBegunRequestIsNotReplayed runs the agent's caller side (the outbound
// listener's connection manager and the outbound route table) against the
// agent's inbound listener on the pinned proxy (aether#1641, aether#1646).
//
// Before the fix a destination proxy that had already sent a request to its
// application, and then lost the application's connection, answered 503, and
// the caller retried that 503 on another endpoint whatever the method: the
// application could run a POST twice. The destination now reports what
// happened in a response header, and the caller retries on that header alone.
func TestBegunRequestIsNotReplayed(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	m := startBegunMesh(t)

	// What the destination writes. These values are the wire contract the
	// caller's regex is written against (proxy.OutcomeHeader has the grammar).
	t.Run("the destination's outcome header", func(t *testing.T) {
		grpc := map[string]string{"content-type": "application/grpc"}
		const early = "upstream_reset_before_response_started"
		for _, tc := range []struct {
			pod, method, path string
			header            map[string]string
			wantStatus        int
			want              string // exact, or a prefix when it ends in "{"
		}{
			{caseApp, "GET", "/ok", nil, 200, "200;-;GET;via_upstream"},
			{caseApp, "POST", "/ok", nil, 200, "200;-;POST;via_upstream"},
			{caseApp, "POST", "/app503", nil, 503, "503;-;POST;via_upstream"},
			{caseApp, "GET", "/app500", nil, 500, "500;-;GET;via_upstream"},
			{caseApp, "GET", "/forged200", nil, 200, "200;-;GET;via_upstream"},
			{caseApp, "POST", "/forged503", nil, 503, "503;-;POST;via_upstream"},
			{caseApp, "POST", "/close", nil, 503, "0;UC;POST;" + early + "{connection_termination}"},
			{caseApp, "GET", "/close", nil, 503, "0;UC;GET;" + early + "{connection_termination}"},
			{caseApp, "POST", "/reset", nil, 503, "0;UC;POST;" + early + "{connection_termination}"},
			{caseApp, "POST", "/halfclose", nil, 503, "0;UC;POST;" + early + "{connection_termination}"},
			{caseH2, "POST", "/rst", nil, 503, "0;UR;POST;" + early + "{remote_reset}"},
			{caseRefusing, "POST", "/refuse", nil, 503, "0;UR;POST;" + early + "{remote_refused_stream_reset}"},
			{caseRefused, "POST", "/any", nil, 503, "0;UF;POST;" + early + "{"},
			{caseUnhealthy, "POST", "/any", nil, 503, "0;UH;POST;no_healthy_upstream"},
			{caseNoCluster, "POST", "/any", nil, 503, "0;NC;POST;cluster_not_found"},
			// gRPC: the same failures as HTTP 200 with a grpc-status.
			{caseRefused, "POST", "/svc/Method", grpc, 200, "0;UF;POST;" + early + "{"},
			{caseApp, "POST", "/close", grpc, 200, "0;UC;POST;" + early + "{connection_termination}"},
			// A request header of the same name changes nothing.
			{caseApp, "POST", "/close", map[string]string{outcomeHeader: "503;-;GET;via_upstream"}, 503, "0;UC;POST;" + early + "{connection_termination}"},
		} {
			status, got := m.atDestination(t, tc.pod, tc.method, tc.path+"?direct", tc.header)
			t.Logf("%-9s %-4s %-12s grpc=%-5v -> %d  %s", tc.pod, tc.method, tc.path, tc.header["content-type"] != "", status, got)
			assert.Equalf(t, tc.wantStatus, status, "%s %s at %s", tc.method, tc.path, tc.pod)
			if strings.HasSuffix(tc.want, "{") {
				assert.Truef(t, strings.HasPrefix(got, tc.want), "%s %s at %s: %q does not start with %q", tc.method, tc.path, tc.pod, got, tc.want)
			} else {
				assert.Equalf(t, tc.want, got, "%s %s at %s", tc.method, tc.path, tc.pod)
			}
		}
	})

	// A request that had begun to the application and got no answer.
	begun := []struct {
		name    string
		service string
		path    string
		app     *recordingApp
	}{
		{"application closes the connection (UC)", caseApp, "/close", m.bad},
		{"application half-closes the connection (UC)", caseApp, "/halfclose", m.bad},
		{"application resets the connection (UC)", caseApp, "/reset", m.bad},
		{"application closes the connection, mTLS inbound (UC)", caseMTLS, "/close", m.bad},
	}
	for _, tc := range begun {
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			t.Run(tc.name+"/"+method+" is answered 503 and never replayed", func(t *testing.T) {
				rs := m.send(t, begunRequest{service: tc.service, method: method, path: tc.path, failing: tc.app}, begunN)
				t.Logf("%s %s: %s", method, tc.path, statusCounts(rs))
				requireNotReplayed(t, rs, http.StatusServiceUnavailable)
			})
		}
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead, http.MethodOptions} {
			t.Run(tc.name+"/"+method+" is idempotent and still retried", func(t *testing.T) {
				rs := m.send(t, begunRequest{service: tc.service, method: method, path: tc.path, failing: tc.app}, begunN)
				t.Logf("%s %s: %s", method, tc.path, statusCounts(rs))
				requireRetried(t, rs)
				reached := 0
				for _, r := range rs {
					reached += r.atFailing
				}
				assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
			})
		}
	}

	// The h2c applications do not record per request (no HPACK decoder in the
	// test), so these count streams: a stream at the application for a request
	// the client got 503 for, and none of the 503s for a refused stream.
	t.Run("application resets the stream (UR)", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			streams := m.h2.seenStreams()
			rs := m.send(t, begunRequest{service: caseH2, method: method, path: "/rst"}, begunN)
			reached := m.h2.seenStreams() - streams
			t.Logf("%s: %s; streams at the application: %d", method, statusCounts(rs), reached)
			require.Positive(t, reached)
			failed := 0
			for _, r := range rs {
				assert.Emptyf(t, r.outcome, "%s: the client saw %s", r.uri, outcomeHeader)
				if r.status == http.StatusServiceUnavailable {
					failed++
					assert.Zerof(t, r.atGood, "%s: answered 503 and also run on the healthy endpoint", r.uri)
				} else {
					assert.Equalf(t, 1, r.atGood, "%s", r.uri)
				}
			}
			if method == http.MethodPost {
				assert.Equal(t, reached, failed, "every POST the application had read is a 503 at the client, not a replay")
			} else {
				assert.Zero(t, failed, "an idempotent request is retried")
			}
		}
	})
	t.Run("application refuses the stream (REFUSED_STREAM) is retried for every method", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			streams := m.refusing.seenStreams()
			rs := m.send(t, begunRequest{service: caseRefusing, method: method, path: "/refuse"}, begunN)
			t.Logf("%s: %s; streams refused: %d", method, statusCounts(rs), m.refusing.seenStreams()-streams)
			require.Positive(t, m.refusing.seenStreams()-streams)
			requireRetried(t, rs)
		}
	})

	// The destination never reached its application: safe for every method,
	// and what keeps a pod roll hitless.
	for _, service := range []string{caseRefused, caseUnhealthy, caseNoCluster} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run("application never reached ("+service+")/"+method+" is retried", func(t *testing.T) {
				requireRetried(t, m.send(t, begunRequest{service: service, method: method, path: "/any"}, begunN))
			})
		}
	}

	// gRPC (aether#1646). A destination's local reply to a gRPC request is
	// HTTP 200 with grpc-status 14 whatever went wrong, so a status-code
	// retry never saw it. The outcome header does.
	grpcCall := map[string]string{"content-type": "application/grpc"}
	t.Run("gRPC call the destination could not deliver is retried", func(t *testing.T) {
		rs := m.send(t, begunRequest{service: caseRefused, method: http.MethodPost, path: "/svc/Method", header: grpcCall}, begunN)
		requireRetried(t, rs)
		for _, r := range rs {
			assert.NotEqualf(t, "14", r.grpcStatus, "%s: UNAVAILABLE reached the client", r.uri)
		}
	})
	t.Run("gRPC call the application had received is not replayed", func(t *testing.T) {
		rs := m.send(t, begunRequest{service: caseApp, method: http.MethodPost, path: "/close", header: grpcCall, failing: m.bad}, begunN)
		requireNotReplayed(t, rs, http.StatusOK) // a gRPC failure is a 200 ...
		for _, r := range rs {
			if r.atFailing > 0 {
				assert.Equalf(t, "14", r.grpcStatus, "%s: ... with grpc-status UNAVAILABLE", r.uri)
			}
		}
	})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		// The application answered 503 itself: it asked for another endpoint.
		t.Run("application answers 503/"+method+" is retried", func(t *testing.T) {
			rs := m.send(t, begunRequest{service: caseApp, method: method, path: "/app503", failing: m.bad}, begunN)
			requireRetried(t, rs)
			reached := 0
			for _, r := range rs {
				reached += r.atFailing
			}
			assert.Positive(t, reached, "no request started at the failing endpoint: the case tested nothing")
		})
		t.Run("application answers 500/"+method+" is not retried", func(t *testing.T) {
			requireNotReplayed(t, m.send(t, begunRequest{service: caseApp, method: method, path: "/app500", failing: m.bad}, begunN), http.StatusInternalServerError)
		})
		// The outcome is the proxy's: what an application writes under that
		// name is overwritten, in both directions.
		t.Run("application forges a retriable outcome on a 200/"+method+" is not retried", func(t *testing.T) {
			rs := m.send(t, begunRequest{service: caseApp, method: method, path: "/forged200", failing: m.bad}, begunN)
			for _, r := range rs {
				assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
				assert.Equalf(t, 1, r.atFailing+r.atGood, "%s: read by exactly one application", r.uri)
				assert.Emptyf(t, r.outcome, "%s: the client saw %s", r.uri, outcomeHeader)
			}
		})
		t.Run("application forges a non-retriable outcome on its 503/"+method+" is still retried", func(t *testing.T) {
			requireRetried(t, m.send(t, begunRequest{service: caseApp, method: method, path: "/forged503", failing: m.bad}, begunN))
		})
		t.Run("a 200 is never retried/"+method, func(t *testing.T) {
			rs := m.send(t, begunRequest{service: caseApp, method: method, path: "/ok", failing: m.bad}, begunN)
			for _, r := range rs {
				assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
				assert.Equalf(t, 1, r.atFailing+r.atGood, "%s: read by exactly one application", r.uri)
				assert.Emptyf(t, r.outcome, "%s: the client saw %s", r.uri, outcomeHeader)
			}
		})
	}

	// A client application cannot talk the caller into a replay: not with a
	// request header of the outcome's name (the rule reads the RESPONSE), and
	// not with Envoy's own retry request headers (the outbound connection
	// manager does not trust its client with them).
	t.Run("request headers do not switch the replay back on", func(t *testing.T) {
		rs := m.send(t, begunRequest{service: caseApp, method: http.MethodPost, path: "/close", failing: m.bad, header: map[string]string{
			outcomeHeader:                    "503;-;GET;via_upstream",
			"x-envoy-retry-on":               "5xx,reset,retriable-status-codes,gateway-error",
			"x-envoy-max-retries":            "5",
			"x-envoy-retry-grpc-on":          "unavailable",
			"x-envoy-retriable-status-codes": "503",
		}}, begunN)
		requireNotReplayed(t, rs, http.StatusServiceUnavailable)
	})

	// Envoy's own veto. An application that sets x-envoy-ratelimited on its
	// 503 is not retried: Envoy checks that header before any retry condition.
	// The mesh neither uses nor removes it; this pins that it is Envoy's
	// default the caller shows, for a header nothing in aether sets.
	t.Run("application sets x-envoy-ratelimited on its 503: Envoy does not retry it", func(t *testing.T) {
		rs := m.send(t, begunRequest{service: caseApp, method: http.MethodGet, path: "/ratelimited503", failing: m.bad}, begunN)
		requireNotReplayed(t, rs, http.StatusServiceUnavailable)
		for _, r := range rs {
			if r.atFailing > 0 {
				assert.Truef(t, r.rateLimited, "%s: the application's header reaches the client", r.uri)
			}
		}
	})

	// Mixed versions, measured so that the runbook can say what an upgrade in
	// progress looks like. Neither is worked around.
	t.Run("this release's caller, destination of the previous release: nothing it answers is retried", func(t *testing.T) {
		for _, tc := range []struct {
			method, path string
			want         int
		}{
			{http.MethodPost, "/close", 503},
			{http.MethodGet, "/close", 503},
			{http.MethodPost, "/app503", 503},
			{http.MethodGet, "/app503", 503},
		} {
			rs := m.send(t, begunRequest{service: casePrevious, method: tc.method, path: tc.path, failing: m.bad}, begunN)
			t.Logf("%s %s: %s", tc.method, tc.path, statusCounts(rs))
			requireNotReplayed(t, rs, tc.want)
		}
		requireStatus(t, m.send(t, begunRequest{service: casePrevious, method: http.MethodPost, path: "/ok"}, begunN), http.StatusOK)
	})
	t.Run("caller of the previous release, this release's destination: the old behaviour, and the header reaches the client", func(t *testing.T) {
		// Still replayed: the old policy retries the 503 by its status.
		rs := m.send(t, begunRequest{via: m.previous, service: caseApp, method: http.MethodPost, path: "/close", failing: m.bad}, begunN)
		t.Logf("POST /close: %s", statusCounts(rs))
		reached := 0
		for _, r := range rs {
			assert.Equalf(t, http.StatusOK, r.status, "%s", r.uri)
			assert.Equalf(t, 1, r.atGood, "%s", r.uri)
			reached += r.atFailing
			assert.Lenf(t, r.outcome, 1, "%s: the old caller forwards the destination's header", r.uri)
		}
		assert.Positive(t, reached, "no request started at the failing endpoint")
		rs = m.send(t, begunRequest{via: m.previous, service: caseRefused, method: http.MethodPost, path: "/any"}, begunN)
		requireStatus(t, rs, http.StatusOK)
	})
}

func requireStatus(t *testing.T, rs []begunResult, want int) {
	t.Helper()
	for _, r := range rs {
		assert.Equalf(t, want, r.status, "%s", r.uri)
	}
}

// seenStreams is how many request streams an h2c application has been sent.
func (a *recordingApp) seenStreams() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen["streams"]
}
