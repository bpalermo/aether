package mtlspool

// Per-port addressing (proposal 043) on the pinned proxy: requests parked on
// the on-demand route, partial listings, and the forms an authority can take.
//
// The rig is perport_live_test.go's with three differences: every cluster name
// gets a backend of its own (the response body says which cluster delivered),
// the service-wide filter also stamps a REQUEST header (the body says whether
// it ran, whatever the protocol of the request), and the registry is a table
// the test edits.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

const spOther = "other.demo.aether.internal"

type spRegistry struct {
	mu   sync.Mutex
	rows map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
}

func (r *spRegistry) Initialize(context.Context) error { return nil }
func (r *spRegistry) Close() error                     { return nil }
func (r *spRegistry) RegisterEndpoint(context.Context, string, registryv1.Service_Protocol, *registryv1.ServiceEndpoint) error {
	return nil
}
func (r *spRegistry) UnregisterEndpoint(context.Context, string, string) error    { return nil }
func (r *spRegistry) UnregisterEndpoints(context.Context, string, []string) error { return nil }
func (r *spRegistry) ListEndpoints(context.Context, string, registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func (r *spRegistry) ListAllEndpoints(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]*registryv1.ServiceEndpoint{}
	for svc, eps := range r.rows[protocol] {
		out[svc] = append([]*registryv1.ServiceEndpoint(nil), eps...)
	}
	return out, nil
}

// set lists one pod of svc under every protocol it serves a port of.
// With only, the pod is listed under those protocols alone (a listing that has
// not caught up).
func (r *spRegistry) set(svc, ip string, legacy uint32, ports map[uint32]registryv1.PortProtocol, only ...registryv1.Service_Protocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{}
	}
	for _, m := range r.rows {
		delete(m, svc)
	}
	if ports == nil {
		return
	}
	parts := strings.SplitN(svc, "/", 2)
	ep := &registryv1.ServiceEndpoint{
		Ip: ip, ClusterName: "cluster-1", Port: legacy, Weight: 100, PortProtocols: ports,
		ContainerMetadata:  &registryv1.ServiceEndpoint_ContainerMetadata{ContainerId: "c", NetworkNamespace: "/proc/1/ns/net"},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: parts[0], PodName: parts[1] + "-0", NodeName: "node-2"},
	}
	for p, class := range ports {
		ep.Ports = append(ep.Ports, p)
		proto := registryv1.Service_Protocol(class)
		if len(only) > 0 && !slices.Contains(only, proto) {
			continue
		}
		if r.rows[proto] == nil {
			r.rows[proto] = map[string][]*registryv1.ServiceEndpoint{}
		}
		r.rows[proto][svc] = []*registryv1.ServiceEndpoint{ep}
	}
}

type spRig struct {
	t        *testing.T
	c        *cache.SnapshotCache
	reg      *spRegistry
	cp       *adsControlPlane
	capAddr  string
	loopAddr string
	capPort  uint32
	loopPort uint32
	chain    string

	mu                 sync.Mutex
	version            int
	backends           map[string]string
	extra              []string
	seen               map[string]bool
	onClusterSubscribe func(name string)
}

// backendFor returns the address of the backend that stands for one cluster
// name. It answers with what it received.
func (r *spRig) backendFor(name string) string {
	if addr, ok := r.backends[name]; ok {
		return addr
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(r.t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = fmt.Fprintf(w, "cluster=%s host=%s chain-filter=%q rule=%q", name, req.Host, req.Header.Get("x-chain-filter"), req.Header.Get("x-rule"))
	})}
	go func() { _ = srv.Serve(ln) }()
	r.t.Cleanup(func() { _ = srv.Close() })
	r.backends[name] = ln.Addr().String()
	return r.backends[name]
}

func (r *spRig) publish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	res := ppTranslate(r.t, r.c, r.capPort, r.chain, "127.0.0.1:1", r.extra...)
	ppWithProxyPort(r.t, r.c, res, r.loopPort)
	for _, cr := range res[resourcev3.ClusterType] {
		cl := cr.(*clusterv3.Cluster)
		host, port, err := net.SplitHostPort(r.backendFor(cl.GetName()))
		require.NoError(r.t, err)
		var p uint32
		_, err = fmt.Sscanf(port, "%d", &p)
		require.NoError(r.t, err)
		sa := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
		sa.Address = host
		sa.PortSpecifier = &corev3.SocketAddress_PortValue{PortValue: p}
	}
	snap, err := cachev3.NewSnapshot(fmt.Sprintf("v%d", r.version), res)
	require.NoError(r.t, err)
	require.NoError(r.t, r.cp.cache.SetSnapshot(context.Background(), envoyNodeID, snap))
}

func (r *spRig) load() {
	require.NoError(r.t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
}

func spHTTP() map[uint32]registryv1.PortProtocol {
	return map[uint32]registryv1.PortProtocol{
		8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
		9090: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
	}
}

// spStart starts the cache (one client pod, demo/other listed, demo/multi
// projected and NOT listed, a service-wide filter on demo/multi), the proxy,
// and both listeners: the capture listener's chain re-keyed onto loopback and
// the pod's loopback outbound listener.
func spStart(t *testing.T, chain string, setup func(c *cache.SnapshotCache)) *spRig {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	ctx := context.Background()
	c := cache.NewSnapshotCache(ppNodeName, slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: "demo", ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, ppNodeSpiffe))
	ppProject(c)
	stamp := func(key string) []*mutation_rulesv3.HeaderMutation {
		return []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: key, Value: "ran"},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}}}}
	}
	perRoute, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		RequestMutations:  stamp(ppHeader),
		ResponseMutations: stamp(ppHeader),
	}})
	require.NoError(t, err)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{ppSvc: {Name: "envoy.filters.http.header_mutation", Config: perRoute}})
	if setup != nil {
		setup(c)
	}
	r := &spRig{t: t, c: c, reg: &spRegistry{}, chain: chain, backends: map[string]string{}, seen: map[string]bool{}}
	r.reg.set("demo/other", "10.0.1.3", 8080, spHTTP())
	r.load()

	// No listener until the proxy has bound its admin port (see below).
	initial := ppTranslate(t, c, 1, chain, "127.0.0.1:1")
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
	// Both picked AFTER the proxy has bound its admin port: a port probed
	// earlier can be the one the admin endpoint then takes.
	r.mu.Lock()
	r.capPort = ppFreePort(t)
	r.loopPort = ppFreePort(t)
	r.mu.Unlock()
	r.publish()
	r.capAddr = e.listenerAddr(t, "capture")
	r.loopAddr = fmt.Sprintf("127.0.0.1:%d", r.loopPort)
	ppAwaitListening(t, r.loopAddr)
	time.Sleep(2 * time.Second) // let RDS land
	return r
}

type spResp struct {
	status int
	reason string
	body   string
	err    string
}

func (r spResp) String() string {
	if r.err != "" {
		return "no response: " + r.err
	}
	return fmt.Sprintf("status=%d x-aether-error=%q body=%q", r.status, r.reason, r.body)
}

func (r spResp) cluster() string {
	for _, f := range strings.Fields(r.body) {
		if v, ok := strings.CutPrefix(f, "cluster="); ok {
			return v
		}
	}
	return ""
}
func (r spResp) filterRan() bool { return strings.Contains(r.body, `chain-filter="ran"`) }
func (r spResp) ruleRan() bool   { return strings.Contains(r.body, `rule="on"`) }

// spRaw writes one HTTP/1.1 request byte for byte.
func spRaw(addr, requestLine string, headers ...string) spResp {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return spResp{err: err.Error()}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	req := requestLine + "\r\n" + strings.Join(headers, "\r\n")
	if len(headers) > 0 {
		req += "\r\n"
	}
	req += "User-Agent: probe\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return spResp{err: err.Error()}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return spResp{err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return spResp{status: resp.StatusCode, reason: resp.Header.Get("x-aether-error"), body: strings.TrimSpace(string(body))}
}

func spGet(addr, host string) spResp { return spRaw(addr, "GET / HTTP/1.1", "Host: "+host) }

// spH2 speaks prior-knowledge HTTP/2 by hand: one GET with exactly the header
// fields given, in order (literal, never indexed, no Huffman). It returns the
// DATA of stream 1 and the first byte of the response header block (0x88 is
// ":status: 200" in the static table).
func spH2(addr string, fields [][2]string) (body string, firstHeaderByte byte, err error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var block bytes.Buffer
	lit := func(s string) {
		if len(s) >= 127 {
			panic("header too long for this encoder")
		}
		block.WriteByte(byte(len(s)))
		block.WriteString(s)
	}
	for _, f := range fields {
		block.WriteByte(0x00) // literal header field without indexing, new name
		lit(f[0])
		lit(f[1])
	}
	frame := func(typ, flags byte, stream uint32, payload []byte) []byte {
		h := make([]byte, 9)
		h[0], h[1], h[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
		h[3], h[4] = typ, flags
		binary.BigEndian.PutUint32(h[5:], stream)
		return append(h, payload...)
	}
	out := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	out = append(out, frame(0x4, 0, 0, nil)...)                 // SETTINGS
	out = append(out, frame(0x1, 0x4|0x1, 1, block.Bytes())...) // HEADERS, END_HEADERS|END_STREAM
	if _, err := conn.Write(out); err != nil {
		return "", 0, err
	}
	br := bufio.NewReader(conn)
	var data bytes.Buffer
	seenHeaders := false
	for {
		h := make([]byte, 9)
		if _, err := io.ReadFull(br, h); err != nil {
			return data.String(), firstHeaderByte, fmt.Errorf("reading a frame: %w (data so far %q)", err, data.String())
		}
		n := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			return data.String(), firstHeaderByte, err
		}
		typ, flags, stream := h[3], h[4], binary.BigEndian.Uint32(h[5:])&0x7fffffff
		switch {
		case typ == 0x4 && flags&0x1 == 0: // SETTINGS: ack it
			if _, err := conn.Write(frame(0x4, 0x1, 0, nil)); err != nil {
				return "", 0, err
			}
		case typ == 0x1 && stream == 1:
			if !seenHeaders && n > 0 {
				firstHeaderByte, seenHeaders = payload[0], true
			}
			if flags&0x1 != 0 {
				return data.String(), firstHeaderByte, nil
			}
		case typ == 0x0 && stream == 1:
			data.Write(payload)
			if flags&0x1 != 0 {
				return data.String(), firstHeaderByte, nil
			}
		case typ == 0x3 && stream == 1: // RST_STREAM
			return data.String(), firstHeaderByte, fmt.Errorf("RST_STREAM %x", payload)
		case typ == 0x7: // GOAWAY
			return data.String(), firstHeaderByte, fmt.Errorf("GOAWAY %q", payload)
		}
	}
}

func spRule() map[string][]proxy.GammaRoute {
	return map[string][]proxy.GammaRoute{ppSvc: {{
		Matches:        []proxy.GammaMatch{{Prefix: "/"}},
		Backends:       []proxy.GammaBackend{{Service: ppSvc, Cluster: ppMesh, Weight: 1, Port: 9090}},
		HeaderMutation: &proxy.GammaHeaderMutation{SetRequest: []proxy.GammaHeaderKV{{Name: "x-rule", Value: "on"}}},
	}}}
}

// TestPerPort_LiveParkedRequestAndTheRoutesOwnRules: demo/multi has a
// service-wide filter AND an HTTPRoute (parentRef without a port): every
// request goes to port 9090 of the service, with a request header set. The
// node holds no endpoint of it when the first request arrives.
//
// What the first (parked) request gets, on each entry path, against what the
// second gets once the node holds the endpoints. The parked request must run
// the service-wide filter; and it should get what the route says.
func TestPerPort_LiveParkedRequestAndTheRoutesOwnRules(t *testing.T) {
	for _, path := range []string{"loopback", "captured"} {
		for _, host := range []string{ppMesh + ":8080", ppMesh + ":9090"} {
			t.Run(path+" "+host, func(t *testing.T) {
				r := spStart(t, "cap_http_"+host[len(host)-4:], func(c *cache.SnapshotCache) { c.SetServiceRoutes(spRule()) })
				addr := r.loopAddr
				if path == "captured" {
					addr = r.capAddr
				}
				published := make(chan struct{})
				var once sync.Once
				r.mu.Lock()
				r.onClusterSubscribe = func(string) {
					once.Do(func() {
						time.Sleep(300 * time.Millisecond)
						r.reg.set(ppSvc, "10.0.1.2", 8080, spHTTP())
						r.load()
						r.publish()
						close(published)
					})
				}
				r.mu.Unlock()
				first := spGet(addr, host)
				t.Logf("MEASURED %s, no endpoint held (parked), Host %s -> %s", path, host, first)
				select {
				case <-published:
				case <-time.After(10 * time.Second):
					t.Fatal("the proxy never asked for a cluster by name: the request was not parked")
				}
				time.Sleep(2 * time.Second)
				second := spGet(addr, host)
				t.Logf("MEASURED %s, endpoints held,              Host %s -> %s", path, host, second)
				if !second.filterRan() || !second.ruleRan() || second.cluster() != ppMesh+":9090" {
					t.Fatalf("control: the second request must run the filter and the rule, and reach port 9090: %s", second)
				}
				if first.status == 200 && !first.filterRan() {
					t.Errorf("the parked request was delivered WITHOUT the service-wide filter: %s", first)
				}
				if first.status == 200 && (!first.ruleRan() || first.cluster() != second.cluster()) {
					t.Errorf("the parked request was delivered (200) but not as the route says: rule ran=%v, cluster %q; the next request: rule ran=%v, cluster %q",
						first.ruleRan(), first.cluster(), second.ruleRan(), second.cluster())
				}
			})
		}
	}
}

// TestPerPort_LiveParkedRequestAndARoutesExtensionFilter: the same, for a
// filter the ROUTE carries (an HTTPRoute's ExtensionRef filter, rendered as
// the route's typed_per_filter_config). The node holds no endpoint of the
// service when the first request arrives; the request is parked, and when it
// resumes it must have gone through the route's filter and reach the port the
// route names, on both entry paths.
//
// The route's filter here is the same filter type as the service-wide one,
// configured to set the "x-rule" request header: a route's configuration of a
// filter replaces the virtual host's for that route, so on these requests the
// service-wide stamp is not expected and is not asserted.
func TestPerPort_LiveParkedRequestAndARoutesExtensionFilter(t *testing.T) {
	routeFilter, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		RequestMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: "x-rule", Value: "on"},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}}}},
	}})
	require.NoError(t, err)
	rule := map[string][]proxy.GammaRoute{ppSvc: {{
		Matches:          []proxy.GammaMatch{{Prefix: "/"}},
		Backends:         []proxy.GammaBackend{{Service: ppSvc, Cluster: ppMesh, Weight: 1, Port: 9090}},
		ExtensionFilters: []proxy.ExtensionFilter{{Name: "envoy.filters.http.header_mutation", Config: routeFilter}},
	}}}
	host := ppMesh + ":8080"
	for _, path := range []string{"loopback", "captured"} {
		t.Run(path, func(t *testing.T) {
			r := spStart(t, "cap_http_8080", func(c *cache.SnapshotCache) { c.SetServiceRoutes(rule) })
			addr := r.loopAddr
			if path == "captured" {
				addr = r.capAddr
			}
			published := make(chan struct{})
			var once sync.Once
			r.mu.Lock()
			r.onClusterSubscribe = func(string) {
				once.Do(func() {
					time.Sleep(300 * time.Millisecond)
					r.reg.set(ppSvc, "10.0.1.2", 8080, spHTTP())
					r.load()
					r.publish()
					close(published)
				})
			}
			r.mu.Unlock()
			first := spGet(addr, host)
			t.Logf("MEASURED %s, no endpoint held (parked), Host %s -> %s", path, host, first)
			select {
			case <-published:
			case <-time.After(10 * time.Second):
				t.Fatal("the proxy never asked for a cluster by name: the request was not parked")
			}
			time.Sleep(2 * time.Second)
			second := spGet(addr, host)
			t.Logf("MEASURED %s, endpoints held,              Host %s -> %s", path, host, second)
			if !second.ruleRan() || second.cluster() != ppMesh+":9090" {
				t.Fatalf("control: the second request must go through the route's filter and reach port 9090: %s", second)
			}
			if first.status == 200 && (!first.ruleRan() || first.cluster() != second.cluster()) {
				t.Errorf("the parked request was delivered (200) with the route's filter skipped or to another port: route filter ran=%v, cluster %q", first.ruleRan(), first.cluster())
			}
		})
	}
}

// TestPerPort_LiveParkedWhileOnlyTheRawTCPRowIsListed: demo/multi serves HTTP
// on 8080 (its declared port) and raw TCP on 9000, and has the service-wide
// filter. The node has its raw-TCP row and not yet its HTTP row (the listings
// of a registration do not arrive together). Requests arrive on the loopback
// listener in that state, then the HTTP row is listed.
//
// The property: no request is delivered without the filter in either state.
// While only the raw-TCP row is listed the node KNOWS the service and none of
// its HTTP ports, so every authority is refused by name, at once; nothing is
// parked on a route that names a cluster. (When this was first measured the
// port-less authority fell to the on-demand catch-all, was parked, and was
// delivered with the filter skipped once the HTTP row arrived.)
func TestPerPort_LiveParkedWhileOnlyTheRawTCPRowIsListed(t *testing.T) {
	mixed := map[uint32]registryv1.PortProtocol{
		8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
		9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
	}
	r := spStart(t, "cap_http_8080", nil)
	r.reg.set(ppSvc, "10.0.1.2", 8080, mixed, registryv1.Service_PROTOCOL_TCP)
	r.load()
	r.publish()
	time.Sleep(2 * time.Second)

	asked := make(chan string, 16)
	r.mu.Lock()
	r.onClusterSubscribe = func(name string) {
		select {
		case asked <- name:
		default:
		}
	}
	r.mu.Unlock()
	for _, host := range []string{ppMesh, ppMesh + ":8080", ppMesh + ":18081", ppMesh + ":9000"} {
		got := spGet(r.loopAddr, host)
		t.Logf("MEASURED loopback, only the raw-TCP row listed, Host %s -> %s", host, got)
		if got.status == 200 {
			t.Errorf("Host %s: delivered (200, by %s) while the node holds only the service's raw-TCP row; filter ran: %v", host, got.cluster(), got.filterRan())
		}
		if got.status != 421 {
			t.Errorf("Host %s: want a refusal by name (421) while no HTTP port of the service is known, got %s", host, got)
		}
	}
	select {
	case name := <-asked:
		t.Errorf("a request was parked for cluster %q while only the raw-TCP row is listed", name)
	case <-time.After(time.Second):
	}

	// The HTTP row is listed: the service's HTTP port answers, with the filter.
	r.reg.set(ppSvc, "10.0.1.2", 8080, mixed)
	r.load()
	r.publish()
	time.Sleep(2 * time.Second)
	second := spGet(r.loopAddr, ppMesh+":8080")
	t.Logf("MEASURED loopback, both rows listed, Host %s -> %s", ppMesh+":8080", second)
	if second.status != 200 || !second.filterRan() {
		t.Fatalf("control: with both rows listed the HTTP port must answer with the filter: %s", second)
	}
}

// TestPerPort_LiveAuthorityForms: steady state, both services held; demo/multi
// has the service-wide filter, demo/other has none, both serve 8080. Every way
// of spelling the authority this test could think of, on both listeners. The
// property: a response that a cluster of demo/multi produced must show the
// filter ran.
func TestPerPort_LiveAuthorityForms(t *testing.T) {
	r := spStart(t, "cap_http_8080", nil)
	r.reg.set(ppSvc, "10.0.1.2", 8080, spHTTP())
	r.load()
	r.publish()
	time.Sleep(2 * time.Second)

	multi, other := ppMesh+":8080", spOther+":8080"
	check := func(what string, got spResp) {
		t.Helper()
		t.Logf("MEASURED %-92s -> %s", what, got)
		if strings.HasPrefix(got.cluster(), ppMesh) && !got.filterRan() {
			t.Errorf("%s: delivered by cluster %s of demo/multi WITHOUT its service-wide filter", what, got.cluster())
		}
	}
	for _, l := range []struct{ name, addr string }{{"captured :8080", r.capAddr}, {"loopback", r.loopAddr}} {
		check(l.name+": origin-form, Host multi:8080 (control: filter)", spGet(l.addr, multi))
		check(l.name+": origin-form, Host other:8080 (control: no filter)", spGet(l.addr, other))
		check(l.name+": absolute-form multi:8080, Host other:8080", spRaw(l.addr, "GET http://"+multi+"/ HTTP/1.1", "Host: "+other))
		check(l.name+": absolute-form other:8080, Host multi:8080", spRaw(l.addr, "GET http://"+other+"/ HTTP/1.1", "Host: "+multi))
		check(l.name+": absolute-form multi:8080, no Host", spRaw(l.addr, "GET http://"+multi+"/ HTTP/1.1"))
		check(l.name+": absolute-form https://multi:8080", spRaw(l.addr, "GET https://"+multi+"/ HTTP/1.1", "Host: "+other))
		check(l.name+": absolute-form with userinfo other@multi:8080", spRaw(l.addr, "GET http://other@"+multi+"/ HTTP/1.1", "Host: "+other))
		check(l.name+": upper-case Host MULTI...:8080", spGet(l.addr, strings.ToUpper(ppMesh)+":8080"))
		check(l.name+": mixed-case Host Multi...:8080", spGet(l.addr, "Multi.demo.aether.internal:8080"))
		check(l.name+": trailing dot multi.demo.aether.internal.:8080", spGet(l.addr, ppMesh+".:8080"))
		check(l.name+": Host multi:08080", spGet(l.addr, ppMesh+":08080"))
		check(l.name+": two Host headers, multi then other", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+multi, "Host: "+other))
		check(l.name+": two Host headers, other then multi", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+other, "Host: "+multi))
		check(l.name+": Host other, X-Forwarded-Host multi", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+other, "X-Forwarded-Host: "+multi))
		check(l.name+": Host other, :authority-like header x-envoy-original-host multi", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+other, "x-envoy-original-host: "+multi))
		check(l.name+": Host other, the client sends x-chain-filter itself (what the oracle shows then)", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+other, "x-chain-filter: ran"))
		check(l.name+": HTTP/1.0, Host multi:8080", spRaw(l.addr, "GET / HTTP/1.0", "Host: "+multi))
		check(l.name+": HTTP/1.0, no Host", spRaw(l.addr, "GET / HTTP/1.0"))
		check(l.name+": h2c upgrade offered, Host multi:8080", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+multi, "Connection: Upgrade, HTTP2-Settings", "Upgrade: h2c", "HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA"))
		check(l.name+": h2c upgrade offered, Host other:8080", spRaw(l.addr, "GET / HTTP/1.1", "Host: "+other, "Connection: Upgrade, HTTP2-Settings", "Upgrade: h2c", "HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA"))
		for _, tc := range []struct {
			what   string
			fields [][2]string
		}{
			{"prior-knowledge h2, :authority multi (control)", [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", multi}}},
			{"prior-knowledge h2, :authority other (control)", [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", other}}},
			{"prior-knowledge h2, :authority multi AND host other", [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", multi}, {"host", other}}},
			{"prior-knowledge h2, :authority other AND host multi", [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", other}, {"host", multi}}},
			{"prior-knowledge h2, host multi, no :authority", [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {"host", multi}}},
			{"prior-knowledge h2, host other, no :authority", [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {"host", other}}},
		} {
			body, first, err := spH2(l.addr, tc.fields)
			got := spResp{body: strings.TrimSpace(body)}
			if err != nil {
				got.err = err.Error()
			}
			t.Logf("MEASURED %-92s -> first header byte 0x%02x, %s", l.name+": "+tc.what, first, got)
			if strings.Contains(body, "cluster="+ppMesh) && !strings.Contains(body, `chain-filter="ran"`) {
				t.Errorf("%s: %s: delivered by a cluster of demo/multi WITHOUT its service-wide filter: %q", l.name, tc.what, body)
			}
		}
	}
}
