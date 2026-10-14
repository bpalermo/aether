package mtlspool

// The one-HCM capture listener, live (gate (d) of the change). The capture
// listener and its route tables are what agent/internal/xds/cache generates;
// the pinned proxy runs them over delta-ADS. The listener selects its chain
// with filter_chain_matcher and one HTTP chain serves every port; cap_http's
// one route table branches on the port that was dialled.
//
// What is replaced so the listener can run outside a pod's network namespace:
// its addresses (loopback ones, one per (address, port) a case dials, with no
// netns, not transparent, no original_dst: the local address of an accepted
// socket IS the dialled one), and the clusters' bodies (a local server per
// cluster name that says which cluster it is).
//
// Two controls run the same cases against a listener that is wrong in a way
// the change exists to rule out:
//
//   - control 1: the chains matched by filter_chain_match, with the one HTTP
//     chain beside the per-address chains (chart 3.0.0 without its per-port
//     chains). It does not backtrack: the trap cases leave through the
//     passthrough.
//   - control 2: cap_http without the dialled-port filter state. Every mesh
//     name is refused (421), never delivered: the table fails closed.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var ohQUICName = regexp.MustCompile(`quic:[a-z0-9.-]+@[a-z0-9.-]+/[a-z0-9.-]+`)

const (
	ohNS     = "demo"
	ohMesh   = "aether.internal"
	ohLocal  = "127.0.0.1"
	ohVIPofB = "127.0.0.20" // service b's address: raw TCP, an HTTP port and a TLSRoute
	ohSNI    = "b.example"
)

// ohPorts are the port numbers of one run, all free on this host.
type ohPorts struct {
	a, a2 uint32 // a's HTTP ports (a is the legacy target)
	c     uint32 // c's HTTP port: an HTTP port number of the mesh, not a's
	t     uint32 // b's raw-TCP port (its legacy target)
	bh    uint32 // b's HTTP port
	o     uint32 // a port no service declares
	proxy bool   // 18081 is free here, and the proxy-port case runs
	// hold keeps every port bound until the proxy is about to bind them, so
	// that nothing else started meanwhile (the proxy's admin endpoint, a
	// backend) is handed one by the kernel.
	hold []net.Listener
}

func (p *ohPorts) release() {
	for _, l := range p.hold {
		_ = l.Close()
	}
	p.hold = nil
}

func ohFreePorts(t *testing.T) *ohPorts {
	t.Helper()
	p := &ohPorts{}
	t.Cleanup(p.release)
	next := func() uint32 {
		for {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			port := uint32(ln.Addr().(*net.TCPAddr).Port)
			p.hold = append(p.hold, ln)
			if port != 18081 && port != 18082 {
				return port
			}
		}
	}
	p.a, p.a2, p.c, p.t, p.bh, p.o = next(), next(), next(), next(), next(), next()
	if ln, err := net.Listen("tcp", "127.0.0.1:18081"); err == nil {
		p.hold = append(p.hold, ln)
		p.proxy = true
	}
	return p
}

func (p *ohPorts) services() []ppaSvc {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	return []ppaSvc{
		{ns: ohNS, name: "a", ip: "10.0.5.1", vip: "127.0.0.30", legacy: p.a, ports: map[uint32]registryv1.PortProtocol{p.a: h, p.a2: h}},
		{ns: ohNS, name: "c", ip: "10.0.5.3", vip: "127.0.0.31", legacy: p.c, ports: map[uint32]registryv1.PortProtocol{p.c: h}},
		{ns: ohNS, name: "b", ip: "10.0.5.2", vip: ohVIPofB, legacy: p.t, ports: map[uint32]registryv1.PortProtocol{p.t: tcp, p.bh: h}},
	}
}

// ohAddrs are the (address, port) pairs the listener binds: every one a case
// dials.
func (p *ohPorts) addrs() [][2]any {
	out := [][2]any{
		{ohLocal, p.a},
		{ohLocal, p.a2},
		{ohLocal, p.c},
		{ohLocal, p.o},
		{ohLocal, p.t},
		{ohVIPofB, p.t},
		{ohVIPofB, p.a},
		{ohVIPofB, p.bh},
		{ohVIPofB, p.o},
	}
	if p.proxy {
		out = append(out, [2]any{ohLocal, uint32(18081)})
	}
	return out
}

// ohBackends serves one local endpoint per cluster name: an HTTP server that
// answers "cluster=<name>" for a mesh HTTP cluster, and a TCP server that
// writes "<KIND> <name>" for anything else.
type ohBackends struct {
	mu   sync.Mutex
	addr map[string]string
}

func (b *ohBackends) get(t *testing.T, name string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if a, ok := b.addr[name]; ok {
		return a
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	b.addr[name] = ln.Addr().String()
	switch {
	case name == proxy.PassthroughClusterName, strings.HasPrefix(name, "tcp:"):
		reply := "TCP " + name + "\n"
		if name == proxy.PassthroughClusterName {
			reply = "PASSTHROUGH\n"
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(3 * time.Second))
					buf := make([]byte, 4096)
					n, _ := c.Read(buf)
					// The passthrough is also an HTTP route's cluster (the
					// catch-all): answer HTTP in HTTP, so the proxy relays it.
					if strings.HasPrefix(string(buf[:n]), "GET ") {
						_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reply), reply)
						return
					}
					_, _ = io.WriteString(c, reply)
				}()
			}
		}()
	default:
		// A `quic:` twin answers as the cluster it is the twin of: the
		// default route selects it per source identity (proposal 038).
		as := name
		if rest, ok := strings.CutPrefix(name, "quic:"); ok {
			as, _, _ = strings.Cut(rest, "@")
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "cluster="+as+"\n")
		}), ReadHeaderTimeout: 3 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
	return b.addr[name]
}

// ohSnapshot builds the agent's snapshot for the run's services.
func ohSnapshot(t *testing.T, p *ohPorts) *cachev3Snapshot {
	t.Helper()
	ctx := context.Background()
	services := p.services()
	c := cache.NewSnapshotCache(ppNodeName, slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: ohNS, ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, ohMesh))
	require.NoError(t, c.SetNodeIdentity(ctx, ppNodeSpiffe))
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	for _, s := range services {
		authorities[s.key()] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
		vips = append(vips, capture.CaptureTCPService{ServiceName: s.key(), ClusterIP: s.vip})
	}
	c.SetCaptureAuthorities(authorities)
	c.SetCaptureTCPServices(vips)
	c.SetTLSServiceRoutes(map[string][]proxy.L4ServiceRoute{ohNS + "/b": {{
		SNIHostnames: []string{ohSNI},
		Backends:     []proxy.L4Backend{{Service: ohNS + "/tlsback", Cluster: "tcp:tlsback." + ohNS + "." + ohMesh, Weight: 1}},
	}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", ppNodeName, &ppaRegistry{services: services}))
	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	return &cachev3Snapshot{
		routes: snap.GetResources(resourcev3.RouteType), clusters: snap.GetResources(resourcev3.ClusterType),
		listeners: snap.GetResources(resourcev3.ListenerType), extensions: snap.GetResources(resourcev3.ExtensionConfigType),
	}
}

type cachev3Snapshot struct {
	routes, clusters, listeners, extensions map[string]types.Resource
}

// ohVariant edits the capture listener for a control; nil runs it as built.
type ohVariant func(t *testing.T, l *listenerv3.Listener, p *ohPorts)

// ohRun publishes the snapshot, with the capture listener on the run's
// loopback addresses and edited by variant, to a pinned proxy.
func ohRun(t *testing.T, p *ohPorts, snap *cachev3Snapshot, variant ohVariant) {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	backends := &ohBackends{addr: map[string]string{}}
	res := map[resourcev3.Type][]types.Resource{}
	for _, r := range snap.routes {
		res[resourcev3.RouteType] = append(res[resourcev3.RouteType], r)
	}
	for _, r := range snap.extensions {
		res[resourcev3.ExtensionConfigType] = append(res[resourcev3.ExtensionConfigType], r)
	}
	var capture *listenerv3.Listener
	for name, r := range snap.listeners {
		if strings.HasPrefix(name, "capture_") && !strings.Contains(name, "udp") {
			capture = proto.Clone(r).(*listenerv3.Listener)
		}
	}
	require.NotNil(t, capture)
	// Every cluster the snapshot carries or a chain names.
	names := map[string]bool{proxy.PassthroughClusterName: true}
	for n := range snap.clusters {
		names[n] = true
	}
	// The `quic:` twins the default routes select per source identity: the
	// agent publishes one when the proxy first asks for it (ODCDS); here
	// every one a table names is published up front.
	for _, r := range snap.routes {
		b, err := proto.Marshal(r.(proto.Message))
		require.NoError(t, err)
		for _, m := range ohQUICName.FindAll(b, -1) {
			names[string(m)] = true
		}
	}
	for _, fc := range capture.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			tc := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig().UnmarshalTo(tc) != nil {
				continue
			}
			if tc.GetCluster() != "" {
				names[tc.GetCluster()] = true
			}
			for _, w := range tc.GetWeightedClusters().GetClusters() {
				names[w.GetName()] = true
			}
		}
	}
	for _, n := range slices.Sorted(maps2Keys(names)) {
		host, port, err := net.SplitHostPort(backends.get(t, n))
		require.NoError(t, err)
		pn, _ := strconv.Atoi(port)
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], &clusterv3.Cluster{
			Name: n, ConnectTimeout: durationpb.New(2 * time.Second),
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			LoadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: n, Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(host, pn)}},
			}}}}},
		})
	}

	// The listener on the run's addresses: no netns, not transparent, no
	// original_dst. The accepted socket's local address is what was dialled.
	addrs := p.addrs()
	capture.Name = "capture"
	capture.Address = socketAddress(addrs[0][0].(string), int(addrs[0][1].(uint32)))
	for _, a := range addrs[1:] {
		capture.AdditionalAddresses = append(capture.AdditionalAddresses, &listenerv3.AdditionalAddress{Address: socketAddress(a[0].(string), int(a[1].(uint32)))})
	}
	capture.Transparent = nil
	capture.UseOriginalDst = nil
	var lf []*listenerv3.ListenerFilter
	for _, f := range capture.GetListenerFilters() {
		if !strings.Contains(f.GetName(), "original_dst") {
			lf = append(lf, f)
		}
	}
	capture.ListenerFilters = lf
	if variant != nil {
		variant(t, capture, p)
	}
	// The proxy starts without the listener; the ports are released and the
	// listener published once its admin endpoint is bound.
	cp := startADSControlPlane(t, res)
	e := launchEnvoyOverADS(t, bin, cp)
	_ = e.listenerAddr
	p.release()
	res[resourcev3.ListenerType] = []types.Resource{capture}
	full, err := cachev3.NewSnapshot("v2", res)
	require.NoError(t, err)
	require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, full))
	ppAwaitListening(t, net.JoinHostPort(ohLocal, strconv.Itoa(int(p.a))))
	// RDS has to land too: the first HTTP request must not race it.
	deadline := time.Now().Add(20 * time.Second)
	for {
		last := ohHTTP1(ohLocal, p.o, "warmup.example")
		if strings.Contains(last, "PASSTHROUGH") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the capture listener never served its route table; last answer %q", last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func maps2Keys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// ohHTTP1 sends one HTTP/1.1 request and returns everything that came back.
func ohHTTP1(ip string, port uint32, host string) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	b, _ := io.ReadAll(c)
	return string(b)
}

// ohRaw sends bytes that are not HTTP or TLS.
func ohRaw(ip string, port uint32) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	_, _ = c.Write([]byte("\x00\x01binary-not-http-or-tls\r\n"))
	b, _ := io.ReadAll(c)
	return string(b)
}

// ohTLS sends a TLS ClientHello with sni and returns what the far end wrote
// back (the handshake itself fails: the backends speak no TLS).
func ohTLS(ip string, port uint32, sni string) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	rec := &ohRecording{Conn: c}
	_ = tls.Client(rec, &tls.Config{ServerName: sni, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}).Handshake() //nolint:gosec // the far end is a test server that speaks no TLS
	return rec.read.String()
}

type ohRecording struct {
	net.Conn
	read strings.Builder
}

func (r *ohRecording) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	r.read.Write(b[:n])
	return n, err
}

// ohH2C sends one request over cleartext HTTP/2 with prior knowledge and
// returns the DATA frames' payload (the body).
func ohH2C(ip string, port uint32, host string) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	lit := func(name, value string) []byte {
		return append(append([]byte{0x00, byte(len(name))}, name...), append([]byte{byte(len(value))}, value...)...)
	}
	var block []byte
	for _, kv := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", host}} {
		block = append(block, lit(kv[0], kv[1])...)
	}
	frame := func(typ, flags byte, stream uint32, payload []byte) []byte {
		n := len(payload)
		return append([]byte{byte(n >> 16), byte(n >> 8), byte(n), typ, flags, byte(stream >> 24), byte(stream >> 16), byte(stream >> 8), byte(stream)}, payload...)
	}
	out := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	out = append(out, frame(0x4, 0, 0, nil)...)
	out = append(out, frame(0x1, 0x5, 1, block)...)
	if _, err := c.Write(out); err != nil {
		return "ERR " + err.Error()
	}
	r := bufio.NewReader(c)
	var body strings.Builder
	for {
		hdr := make([]byte, 9)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return body.String()
		}
		n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return body.String()
		}
		if hdr[3] == 0x0 && hdr[5]|hdr[6]|hdr[7]|hdr[8] != 0 {
			body.Write(payload)
			if hdr[4]&0x1 != 0 {
				return body.String()
			}
		}
		if hdr[3] == 0x1 && hdr[4]&0x1 != 0 { // HEADERS with END_STREAM: no body
			return body.String() + "<headers only>"
		}
	}
}

// ohCase is one probe: what to send where, and the substring the answer has.
type ohCase struct {
	id   string
	send func() string
	want string
	// trap: a case the legacy filter_chain_match listener answers wrongly.
	trap bool
}

func ohCases(p *ohPorts) []ohCase {
	a := "a." + ohNS + "." + ohMesh
	aKube := "a." + ohNS + ".svc.cluster.local"
	aDefault := "cluster=" + a + "\n"
	a2 := fmt.Sprintf("cluster=%s:%d\n", a, p.a2)
	mismatch := "x-aether-error: port-mismatch"
	undeclared := "x-aether-error: port-not-declared"
	tcpB := "TCP tcp:b." + ohNS + "." + ohMesh
	cases := []ohCase{
		{id: "D1 port-less Host, dialled a's port", send: func() string { return ohHTTP1(ohLocal, p.a, a) }, want: aDefault},
		{id: "D2 Host :a, dialled a's port", send: func() string { return ohHTTP1(ohLocal, p.a, fmt.Sprintf("%s:%d", a, p.a)) }, want: aDefault},
		{id: "D3 cluster.local name, dialled a's port", send: func() string { return ohHTTP1(ohLocal, p.a, fmt.Sprintf("%s:%d", aKube, p.a)) }, want: aDefault},
		{id: "M1 Host names a's other port, dialled a's port", send: func() string { return ohHTTP1(ohLocal, p.a, fmt.Sprintf("%s:%d", a, p.a2)) }, want: mismatch},
		{id: "M2 Host names port 7, dialled a's port", send: func() string { return ohHTTP1(ohLocal, p.a, a+":7") }, want: mismatch},
		{id: "M3 Host names the dialled port twice", send: func() string { return ohHTTP1(ohLocal, p.a, fmt.Sprintf("%s:%d:%d", a, p.a, p.a)) }, want: mismatch},
		{id: "D4 port-less Host, dialled a's other port", send: func() string { return ohHTTP1(ohLocal, p.a2, a) }, want: a2},
		{id: "N1 dialled an HTTP port number of the mesh that is not a's", send: func() string { return ohHTTP1(ohLocal, p.c, a) }, want: undeclared},
		{id: "N2 that port, Host names a's port", send: func() string { return ohHTTP1(ohLocal, p.c, fmt.Sprintf("%s:%d", a, p.a)) }, want: mismatch},
		{id: "O1 dialled a port no service declares", send: func() string { return ohHTTP1(ohLocal, p.o, fmt.Sprintf("%s:%d", a, p.o)) }, want: undeclared},
		{id: "O2 that port, a Host that is no mesh name", send: func() string { return ohHTTP1(ohLocal, p.o, "example.com") }, want: "PASSTHROUGH"},
		{id: "C1 a Host that is no mesh name, dialled a's port", send: func() string { return ohHTTP1(ohLocal, p.a, "example.com") }, want: "PASSTHROUGH"},
		{id: "T1 HTTP bytes to b's raw-TCP port", send: func() string { return ohHTTP1(ohVIPofB, p.t, a) }, want: tcpB},
		{id: "T2 raw bytes to b's raw-TCP port", send: func() string { return ohRaw(ohVIPofB, p.t) }, want: tcpB},
		{id: "B1 HTTP to b's address on a's port (the trap)", send: func() string { return ohHTTP1(ohVIPofB, p.a, a) }, want: aDefault, trap: true},
		{id: "B2 HTTP to b's raw-TCP port number at another address", send: func() string { return ohHTTP1(ohLocal, p.t, a) }, want: undeclared, trap: true},
		{id: "P1 raw bytes to an HTTP port", send: func() string { return ohRaw(ohLocal, p.a) }, want: "PASSTHROUGH"},
		{id: "H2 h2c, dialled a's port", send: func() string { return ohH2C(ohLocal, p.a, a) }, want: aDefault},
		{id: "H3 h2c, Host names a's other port", send: func() string { return ohH2C(ohLocal, p.a, fmt.Sprintf("%s:%d", a, p.a2)) }, want: "with a Host header that names another port"},
		// #911 under filter_chain_match: the port tier wins before the name.
		{id: "S1 TLS with b's TLSRoute name to b's raw-TCP port", send: func() string { return ohTLS(ohVIPofB, p.t, ohSNI) }, want: "TCP tcp:tlsback.", trap: true},
		{id: "S2 TLS with b's TLSRoute name to another port of b", send: func() string { return ohTLS(ohVIPofB, p.o, ohSNI) }, want: "TCP tcp:tlsback."},
		{id: "S3 TLS with b's TLSRoute name to b's OWN HTTP port (the ruling)", send: func() string { return ohTLS(ohVIPofB, p.bh, ohSNI) }, want: "PASSTHROUGH", trap: true},
		{id: "S4 TLS with another name to b's raw-TCP port", send: func() string { return ohTLS(ohVIPofB, p.t, "other.example") }, want: tcpB},
		{id: "S5 HTTP to b's own HTTP port", send: func() string { return ohHTTP1(ohVIPofB, p.bh, fmt.Sprintf("b.%s.%s", ohNS, ohMesh)) }, want: fmt.Sprintf("cluster=b.%s.%s:%d", ohNS, ohMesh, p.bh), trap: true},
	}
	if p.proxy {
		cases = append(cases, ohCase{id: "X1 dialled the proxy port (the Host's port decides)", send: func() string { return ohHTTP1(ohLocal, 18081, fmt.Sprintf("%s:%d", a, p.a)) }, want: aDefault})
	}
	return cases
}

func ohShort(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " | ")
	if len(s) > 220 {
		s = s[:220]
	}
	return s
}

// TestOneHCM_LiveProbe runs every case against the listener as the agent
// builds it: all must give their answer.
func TestOneHCM_LiveProbe(t *testing.T) {
	p := ohFreePorts(t)
	snap := ohSnapshot(t, p)
	ohRun(t, p, snap, nil)
	for _, c := range ohCases(p) {
		got := c.send()
		t.Logf("%-62s -> %s", c.id, ohShort(got))
		assert.Containsf(t, got, c.want, "%s", c.id)
	}
}

// TestOneHCM_LiveControl_LegacyChainMatch is control 1: the same chains, the
// same tables, selected by filter_chain_match with one HTTP chain (what the
// per-port layout of chart 3.0.0 worked around). The trap cases must answer
// wrongly here; the rest as with the matcher.
func TestOneHCM_LiveControl_LegacyChainMatch(t *testing.T) {
	p := ohFreePorts(t)
	snap := ohSnapshot(t, p)
	ohRun(t, p, snap, func(t *testing.T, l *listenerv3.Listener, p *ohPorts) {
		l.FilterChainMatcher = nil
		httpMatch := func() *listenerv3.FilterChainMatch {
			return &listenerv3.FilterChainMatch{TransportProtocol: "raw_buffer", ApplicationProtocols: []string{"http/1.0", "http/1.1", "h2c"}}
		}
		vip := func() []*corev3.CidrRange {
			return []*corev3.CidrRange{{AddressPrefix: ohVIPofB, PrefixLen: wrapperspb.UInt32(32)}}
		}
		for _, fc := range l.GetFilterChains() {
			name := fc.GetName()
			switch {
			case name == proxy.CaptureHTTPChainName:
				fc.FilterChainMatch = httpMatch()
			case name == "cap_http_18081":
				fc.FilterChainMatch = httpMatch()
				fc.FilterChainMatch.DestinationPort = wrapperspb.UInt32(18081)
			case strings.HasPrefix(name, "cap_tls_"):
				fc.FilterChainMatch = &listenerv3.FilterChainMatch{PrefixRanges: vip(), ServerNames: []string{ohSNI}}
			case strings.HasPrefix(name, "cap_tcp_"):
				port, err := strconv.Atoi(name[strings.LastIndex(name, "_")+1:])
				require.NoError(t, err)
				fc.FilterChainMatch = &listenerv3.FilterChainMatch{PrefixRanges: vip(), DestinationPort: wrapperspb.UInt32(uint32(port))}
			default:
				t.Fatalf("chain %s has no legacy match in this control", name)
			}
		}
	})
	wrong := 0
	for _, c := range ohCases(p) {
		got := c.send()
		t.Logf("%-62s -> %s", c.id, ohShort(got))
		if c.trap {
			if !assert.NotContainsf(t, got, c.want, "%s: the legacy listener was expected to get this wrong", c.id) {
				continue
			}
			wrong++
			continue
		}
		assert.Containsf(t, got, c.want, "%s", c.id)
	}
	assert.Equal(t, 5, wrong, "every trap case answered wrongly by the legacy listener")
}

// TestOneHCM_LiveControl_NoDialledPort is control 2: cap_http without the
// filter state that records the dialled port. Its table cannot tell which
// port was dialled, and must then refuse every mesh name: never deliver one,
// to any port.
func TestOneHCM_LiveControl_NoDialledPort(t *testing.T) {
	p := ohFreePorts(t)
	snap := ohSnapshot(t, p)
	ohRun(t, p, snap, func(t *testing.T, l *listenerv3.Listener, _ *ohPorts) {
		removed := 0
		for _, fc := range l.GetFilterChains() {
			if fc.GetName() != proxy.CaptureHTTPChainName {
				continue
			}
			var kept []*listenerv3.Filter
			for _, f := range fc.GetFilters() {
				cfg := &set_filter_state_v3.Config{}
				if f.GetTypedConfig().UnmarshalTo(cfg) == nil && len(cfg.GetOnNewConnection()) == 1 && cfg.GetOnNewConnection()[0].GetObjectKey() == proxy.DialledPortFilterStateKey {
					removed++
					continue
				}
				kept = append(kept, f)
			}
			fc.Filters = kept
		}
		require.Equal(t, 1, removed)
	})
	a := "a." + ohNS + "." + ohMesh
	for _, c := range []struct {
		id   string
		port uint32
		host string
	}{
		{"D1 without the dialled port", p.a, a},
		{"D2 without the dialled port", p.a, fmt.Sprintf("%s:%d", a, p.a)},
		{"D4 without the dialled port", p.a2, a},
		{"D4' Host names a's other port, dialled it", p.a2, fmt.Sprintf("%s:%d", a, p.a2)},
		{"cluster.local name", p.a, fmt.Sprintf("a.%s.svc.cluster.local:%d", ohNS, p.a)},
	} {
		got := ohHTTP1(ohLocal, c.port, c.host)
		t.Logf("%-50s -> %s", c.id, ohShort(got))
		assert.Containsf(t, got, "HTTP/1.1 421", "%s: a mesh name must be refused", c.id)
		assert.Containsf(t, got, "x-aether-error: port-not-declared", c.id)
		assert.NotContainsf(t, got, "cluster=", "%s: delivered without knowing the dialled port", c.id)
	}
	// A name that is not the mesh's still leaves as such.
	assert.Contains(t, ohHTTP1(ohLocal, p.a, "example.com"), "PASSTHROUGH")
}
