package mtlspool

// The per-request cost arm of the HTTP/3 harness (aether#1021).
//
// #1006 attributed QUIC's +0.78 fleet cores to the HTTP/3 request path: ~11 ms
// of proxy CPU per h3 mesh request against ~3.3 ms for h2, with 128 QUIC
// connections carrying 100 rps (~0.8 rps per connection). Two questions follow,
// and this file answers each against the pinned aether-proxy binary:
//
//  1. WHY that many upstream connections. TestQUICTwinUpstreamConnections
//     counts, at the destination and in the twin's own upstream_cx_total, the
//     QUIC connections N downstream connections from ONE source open to ONE
//     destination. The pool key of a twin is (worker, host, source identity):
//     downstream connections are not in it, because a twin does not set
//     connection_pool_per_downstream_connection (#842 took it off the h2 base
//     it is cloned from, and proxy.QUICClusterFrom forces it off). The
//     counterfactual case turns the option ON and shows what it would cost:
//     one QUIC connection per downstream connection.
//
//  2. WHAT the inbound listener's UDP offload options buy.
//     TestQUICRequestCPU (opt-in: AETHER_QUIC_COST=1) drives a fixed number of
//     paced requests through a source Envoy and a destination Envoy -- h2
//     through the production h2 cluster, h3 through a production twin into a
//     QUIC listener carrying proxy.InboundQUICUDPListenerConfig() or a variant
//     of it -- and reports the CPU both processes spent (/proc/<pid>/stat
//     utime+stime) per request.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	udpwriterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/udp_packet_writer/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// quicTwinAStat is twin A's stats key: proxy.QUICAltStatName over the h2
// cluster's alt_stat_name (meshClusterName).
var quicTwinAStat = proxy.QUICAltStatName(meshClusterName, "demo/source-a")

// costProxy is a running Envoy plus what the cost tests read from it.
type costProxy struct {
	pid   int
	admin string
	addrs map[string]string // listener name -> address
}

// runEnvoy runs the pinned proxy on bs with the given worker count and an
// admin endpoint, and reads back the address each named listener bound (for a
// TCP listener, once it accepts).
func runEnvoy(t *testing.T, label string, bs *bootstrapv3.Bootstrap, concurrency int, listeners ...string) *costProxy {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}
	e := launchEnvoy(t, bin, label, bs, nil, "--concurrency", strconv.Itoa(concurrency))
	p := &costProxy{pid: e.pid, admin: e.admin, addrs: map[string]string{}}
	for _, name := range listeners {
		p.addrs[name] = e.listenerAddr(t, name)
	}
	return p
}

// counter reads one counter/gauge from the admin /stats endpoint.
func (p *costProxy) counter(t *testing.T, name string) int {
	t.Helper()
	resp, err := http.Get("http://" + p.admin + "/stats?usedonly&filter=" + strings.ReplaceAll(name, ".", `\.`) + "$")
	require.NoError(t, err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if ok && k == name {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			require.NoError(t, err)
			return n
		}
	}
	return 0
}

// cpuTicks is utime+stime of pid in clock ticks (/proc/<pid>/stat fields 14,
// 15). USER_HZ is 100 on every Linux the harness runs on.
func cpuTicks(t *testing.T, pid int) int64 {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	// comm (field 2) may contain spaces; fields restart after the last ')'.
	s := string(raw)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	require.NoError(t, err)
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	require.NoError(t, err)
	return utime + stime
}

// startCostSource runs a source Envoy for source-a: the production h2 mesh
// cluster (pointed at h2Addr) and source-a's production twin (pointed at
// h3Addr). With viaTwin the listener's route selects the twin exactly as the
// cache programs it (proxy.ApplyQUICClusterSelection); without, it routes to
// the h2 cluster. mutateTwin, when set, edits the twin before it is written.
func startCostSource(t *testing.T, p *pki, h2Addr, h3Addr string, concurrency int, viaTwin bool, mutateTwin func(*clusterv3.Cluster)) *costProxy {
	t.Helper()
	sdsAddr := startSDS(t, p, []string{spiffeSourceA, spiffeSourceB, spiffeNode})
	twin := quicTwin(t, quicTwinA, spiffeSourceA, h3Addr)
	if mutateTwin != nil {
		mutateTwin(twin)
	}
	var l *listenerv3.Listener
	if viaTwin {
		l = selectingSourceListener(t, "source_a", spiffeSourceA, envoyPicksPort, map[string]string{spiffeSourceA: quicTwinA})
	} else {
		l = sourceListener("source_a", spiffeSourceA, envoyPicksPort, true)
	}
	bs := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters:  []*clusterv3.Cluster{meshCluster(t, h2Addr), sdsCluster(sdsAddr), twin},
			Listeners: []*listenerv3.Listener{l},
		},
	}
	return runEnvoy(t, "source", bs, concurrency, "source_a")
}

// TestQUICTwinUpstreamConnections: N keep-alive downstream connections from ONE
// source to ONE destination, every connection used for several requests, and
// the number of upstream QUIC connections counted twice -- by the destination
// (distinct QUIC connections it served) and by the twin (upstream_cx_total).
//
//   - production twin, 1 worker: ONE connection, whatever N is. The downstream
//     connection is not in the pool key.
//   - production twin, 4 workers: at most 4 -- one pool per worker, and the
//     kernel spreads the N downstream connections across the workers' accept
//     sockets. THIS is the fan-out term production has: per node, per source
//     ServiceAccount, per destination endpoint, one connection per worker the
//     source's downstream connections landed on.
//   - counterfactual, option ON, 1 worker: N connections. What
//     connection_pool_per_downstream_connection would cost a twin.
//
// Every request must still present source-a's SVID over HTTP/3.
func TestQUICTwinUpstreamConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	const downstream = 12
	const rounds = 3
	for _, tc := range []struct {
		name          string
		concurrency   int
		perDownstream bool
		check         func(t *testing.T, conns int)
	}{
		{"production/1-worker", 1, false, func(t *testing.T, conns int) {
			assert.Equal(t, 1, conns, "one source, one destination, one worker: the twin must pool to ONE QUIC connection whatever the downstream count")
		}},
		{"production/4-workers", 4, false, func(t *testing.T, conns int) {
			assert.GreaterOrEqual(t, conns, 1)
			assert.LessOrEqual(t, conns, 4, "at most one QUIC connection per worker")
		}},
		{"counterfactual-per-downstream/1-worker", 1, true, func(t *testing.T, conns int) {
			assert.Equal(t, downstream, conns, "connection_pool_per_downstream_connection gives every downstream connection its own QUIC connection")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPKI(t)
			h2 := startDestination(t, p)
			h3 := startDestinationH3(t, p)
			var mutate func(*clusterv3.Cluster)
			if tc.perDownstream {
				mutate = func(c *clusterv3.Cluster) { c.ConnectionPoolPerDownstreamConnection = true }
			} else {
				mutate = func(c *clusterv3.Cluster) {
					require.False(t, c.GetConnectionPoolPerDownstreamConnection(), "production twin pools per downstream connection")
				}
			}
			src := startCostSource(t, p, h2.addr, h3.addr, tc.concurrency, true, mutate)
			clients := make([]*sourceClient, downstream)
			for i := range clients {
				clients[i] = newSourceClient(fmt.Sprintf("source-a#%d", i), src.addrs["source_a"])
			}
			seen := map[uint64]bool{}
			for r := 0; r < rounds; r++ {
				for _, c := range clients {
					o := c.callProto(t)
					require.Equalf(t, spiffeSourceA, o.peerURISAN, "%s was verified as %q", c.name, o.peerURISAN)
					require.Equal(t, "HTTP/3.0", o.proto)
					seen[o.connID] = true
				}
			}
			cx := src.counter(t, "cluster."+quicTwinAStat+".upstream_cx_total")
			t.Logf("%s: %d downstream connections x %d rounds -> %d QUIC connections at the destination, twin upstream_cx_total=%d",
				tc.name, downstream, rounds, len(seen), cx)
			tc.check(t, len(seen))
			tc.check(t, cx)
		})
	}
}

// ---------------------------------------------------------------------------
// CPU per request (opt-in)
// ---------------------------------------------------------------------------

// responseBody is the soak's svc-3/4 response size (#1006: 92 B in, 903 B out).
var responseBody = strings.Repeat("x", 903)

// directResponseHCM is an HCM answering every request 200 with responseBody.
func directResponseHCM(name string, codec hcmv3.HttpConnectionManager_CodecType) *hcmv3.HttpConnectionManager {
	return &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  codec,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name: name,
			VirtualHosts: []*routev3.VirtualHost{{
				Name: "all", Domains: []string{"*"},
				Routes: []*routev3.Route{{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
						Status: 200,
						Body:   &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: responseBody}},
					}},
				}},
			}},
		}},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&routerv3.Router{})},
		}},
	}
}

// destTLS is the destination's mTLS context: the echo SVID with the path-B DNS
// SANs, client certificate required and verified against the harness CA.
func destTLS(t *testing.T, p *pki, alpn string) *tlsv3.DownstreamTlsContext {
	t.Helper()
	certPath, keyPath := p.leafDNS(t, "echo-envoy", spiffeDest, []string{quicDestFQDN, "*." + quicDestFQDN})
	caPath := filepath.Join(p.dir, "ca-bundle.pem")
	writeFile(t, caPath, p.caPEM)
	return &tlsv3.DownstreamTlsContext{
		RequireClientCertificate: wrapperspb.Bool(true),
		CommonTlsContext: &tlsv3.CommonTlsContext{
			AlpnProtocols:   []string{alpn},
			TlsCertificates: []*tlsv3.TlsCertificate{{CertificateChain: fileDataSource(certPath), PrivateKey: fileDataSource(keyPath)}},
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{ValidationContext: &tlsv3.CertificateValidationContext{
				TrustedCa: fileDataSource(caPath),
			}},
		},
	}
}

// quicListenerAddress is where the harness's HTTP/3 listeners bind: loopback
// UDP, on a port the kernel picks.
func quicListenerAddress() *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Protocol: corev3.SocketAddress_UDP, Address: "127.0.0.1",
		PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: envoyPicksPort},
	}}}
}

// startCostDestination runs a destination Envoy with an h2 mTLS TCP listener
// and an HTTP/3 listener, the latter carrying udp as its udp_listener_config.
//
// Production serves both on one port number (18008). Here each listener gets
// its own from the kernel: the two are separate sockets either way, and
// nothing in the upstream clusters depends on the numbers being equal (the
// h2 cluster and the QUIC twin each take their own address).
func startCostDestination(t *testing.T, p *pki, udp *listenerv3.UdpListenerConfig) (h2Addr, h3Addr string, proxyHandle *costProxy) {
	t.Helper()
	tcpHCM := directResponseHCM("dest_h2", hcmv3.HttpConnectionManager_AUTO)
	h3HCM := directResponseHCM("dest_h3", hcmv3.HttpConnectionManager_HTTP3)
	h3HCM.Http3ProtocolOptions = &corev3.Http3ProtocolOptions{}
	tcp := &listenerv3.Listener{
		Name:    "dest_h2",
		Address: socketAddress("127.0.0.1", envoyPicksPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{Name: "envoy.filters.network.http_connection_manager", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(tcpHCM)}}},
			TransportSocket: &corev3.TransportSocket{
				Name:       "envoy.transport_sockets.tls",
				ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(destTLS(t, p, "h2"))},
			},
		}},
	}
	h3 := &listenerv3.Listener{
		Name:    "dest_h3",
		Address: quicListenerAddress(),
		// Off so the kernel-assigned UDP port is this listener's alone; see
		// requireExclusiveUDPBinds. This Envoy runs one worker.
		EnableReusePort:   wrapperspb.Bool(false),
		UdpListenerConfig: udp,
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{Name: "envoy.filters.network.http_connection_manager", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(h3HCM)}}},
			TransportSocket: &corev3.TransportSocket{
				Name: "envoy.transport_sockets.quic",
				ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(&quicv3.QuicDownstreamTransport{
					DownstreamTlsContext: destTLS(t, p, "h3"),
					EnableResumption:     wrapperspb.Bool(false),
					EnableEarlyData:      wrapperspb.Bool(false),
				})},
			},
		}},
	}
	bs := &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Id: "dest", Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Listeners: []*listenerv3.Listener{tcp, h3}},
	}
	dst := runEnvoy(t, "dest", bs, 1, "dest_h2", "dest_h3")
	return dst.addrs["dest_h2"], dst.addrs["dest_h3"], dst
}

// udpVariant is one udp_listener_config under measurement.
type udpVariant struct {
	name  string
	viaH3 bool
	udp   func() *listenerv3.UdpListenerConfig
}

func withWriter(name string, cfg proto.Message) func() *listenerv3.UdpListenerConfig {
	return func() *listenerv3.UdpListenerConfig {
		u := proxy.InboundQUICUDPListenerConfig()
		u.UdpPacketPacketWriterConfig = &corev3.TypedExtensionConfig{Name: name, TypedConfig: config.TypedConfig(cfg)}
		return u
	}
}

func withGRO(on bool) func() *listenerv3.UdpListenerConfig {
	return func() *listenerv3.UdpListenerConfig {
		u := proxy.InboundQUICUDPListenerConfig()
		u.DownstreamSocketConfig = &corev3.UdpSocketConfig{PreferGro: wrapperspb.Bool(on)}
		return u
	}
}

// costRun is one measured run.
type costRun struct {
	requests, failures int
	// srcTicks/dstTicks are loaded MINUS idle; the idle share is kept for the log.
	srcTicks, dstTicks int64
	srcIdle, dstIdle   int64
}

func (r costRun) msPerRequest() (src, dst, total float64) {
	per := func(ticks int64) float64 { return float64(ticks) * 10 / float64(r.requests) } // 1 tick = 10 ms
	return per(r.srcTicks), per(r.dstTicks), per(r.srcTicks + r.dstTicks)
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// measure drives `requests` paced requests at `rps` over `conns` keep-alive
// downstream connections (each connection strictly sequential, so every
// request is its own flight -- the soak's shape) and returns the CPU both
// Envoys spent doing it, after a warm-up that establishes every connection.
func measure(t *testing.T, v udpVariant, requests, rps, conns int) costRun {
	t.Helper()
	p := newPKI(t)
	var udp *listenerv3.UdpListenerConfig
	if v.udp != nil {
		udp = v.udp()
	} else {
		udp = proxy.InboundQUICUDPListenerConfig()
	}
	h2Addr, h3Addr, dst := startCostDestination(t, p, udp)
	src := startCostSource(t, p, h2Addr, h3Addr, 1, v.viaH3, nil)
	clients := make([]*http.Client, conns)
	for i := range clients {
		clients[i] = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Minute}}
	}
	url := "http://" + src.addrs["source_a"] + "/"
	do := func(c *http.Client) bool {
		req, _ := http.NewRequest(http.MethodGet, url, strings.NewReader(""))
		req.Host = quicDestFQDN
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		n, _ := drain(resp)
		return resp.StatusCode == http.StatusOK && n == len(responseBody)
	}
	// Warm-up: every connection answers, so the handshakes are outside the window.
	deadline := time.Now().Add(20 * time.Second)
	for _, c := range clients {
		for !do(c) {
			require.True(t, time.Now().Before(deadline), "%s: warm-up never succeeded", v.name)
			time.Sleep(100 * time.Millisecond)
		}
	}
	time.Sleep(500 * time.Millisecond)
	perConn := requests / conns
	interval := time.Duration(float64(time.Second) * float64(conns) / float64(rps))
	// Idle baseline: both processes, same connections open, no requests, for
	// the loaded window's length (capped), so the result is loaded-minus-idle
	// -- the #1006 method -- and not Envoy's background timers.
	window := interval * time.Duration(perConn)
	idleWindow := min(window, 10*time.Second)
	si, di := cpuTicks(t, src.pid), cpuTicks(t, dst.pid)
	time.Sleep(idleWindow)
	idleScale := float64(window) / float64(idleWindow)
	srcIdle := int64(float64(cpuTicks(t, src.pid)-si) * idleScale)
	dstIdle := int64(float64(cpuTicks(t, dst.pid)-di) * idleScale)
	s0, d0 := cpuTicks(t, src.pid), cpuTicks(t, dst.pid)
	var failures atomic.Int64
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *http.Client) {
			defer wg.Done()
			// Stagger the connections across the interval.
			time.Sleep(interval * time.Duration(i) / time.Duration(conns))
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for n := 0; n < perConn; n++ {
				<-tick.C
				if !do(c) {
					failures.Add(1)
				}
			}
		}(i, c)
	}
	wg.Wait()
	run := costRun{
		requests: perConn * conns, failures: int(failures.Load()),
		srcTicks: cpuTicks(t, src.pid) - s0 - srcIdle, dstTicks: cpuTicks(t, dst.pid) - d0 - dstIdle,
		srcIdle: srcIdle, dstIdle: dstIdle,
	}
	if v.viaH3 {
		// The load really rode the twin.
		require.GreaterOrEqual(t, src.counter(t, "cluster."+quicTwinAStat+".upstream_rq_total"), run.requests)
	}
	return run
}

// drain reads and closes a response body, returning its length.
func drain(resp *http.Response) (int, error) {
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return int(n), err
}

// udpVariants are the configurations TestQUICRequestCPU compares.
func udpVariants() []udpVariant {
	return []udpVariant{
		{name: "h2", viaH3: false},
		{name: "h3/production", viaH3: true},
		{name: "h3/no-gso(default-writer)", viaH3: true, udp: withWriter("envoy.udp_packet_writer.default", &udpwriterv3.UdpDefaultWriterFactory{})},
		{name: "h3/gso(explicit)", viaH3: true, udp: withWriter("envoy.udp_packet_writer.gso", &udpwriterv3.UdpGsoBatchWriterFactory{})},
		{name: "h3/gro-off", viaH3: true, udp: withGRO(false)},
		{name: "h3/gro-on", viaH3: true, udp: withGRO(true)},
	}
}

// TestQUICRequestCPU measures proxy CPU per request for h2 and for h3 under
// each udp_listener_config variant. It is a MEASUREMENT, not a gate: loopback
// on a shared workstation is noisy, so it runs only with AETHER_QUIC_COST=1
// and only fails if requests fail. Knobs (env): AETHER_QUIC_COST_REQUESTS
// (10000), AETHER_QUIC_COST_RPS (250), AETHER_QUIC_COST_CONNS (8),
// AETHER_QUIC_COST_REPS (2; variants are interleaved per repetition).
//
//	bazel test //agent/test/mtlspool:mtlspool_test --test_filter=TestQUICRequestCPU \
//	  --test_env=AETHER_QUIC_COST=1 --test_output=all --test_timeout=3600
func TestQUICRequestCPU(t *testing.T) {
	if os.Getenv("AETHER_QUIC_COST") == "" {
		t.Skip("measurement; set AETHER_QUIC_COST=1 to run")
	}
	requests := envInt("AETHER_QUIC_COST_REQUESTS", 10000)
	rps := envInt("AETHER_QUIC_COST_RPS", 250)
	conns := envInt("AETHER_QUIC_COST_CONNS", 8)
	reps := envInt("AETHER_QUIC_COST_REPS", 2)
	only := os.Getenv("AETHER_QUIC_COST_ONLY")
	type total struct {
		src, dst, all float64
		n             int
	}
	sums := map[string]*total{}
	var order []string
	for rep := 0; rep < reps; rep++ {
		for _, v := range udpVariants() {
			if only != "" && !strings.Contains(","+only+",", ","+v.name+",") {
				continue
			}
			var r costRun
			t.Run(fmt.Sprintf("%s/rep%d", v.name, rep), func(t *testing.T) { r = measure(t, v, requests, rps, conns) })
			require.Zerof(t, r.failures, "%s: %d of %d requests failed", v.name, r.failures, r.requests)
			s, d, a := r.msPerRequest()
			t.Logf("MEASURE %-28s rep=%d requests=%d src=%d ticks dst=%d ticks (idle subtracted: %d/%d) -> %.3f + %.3f = %.3f ms CPU/request",
				v.name, rep, r.requests, r.srcTicks, r.dstTicks, r.srcIdle, r.dstIdle, s, d, a)
			if sums[v.name] == nil {
				sums[v.name] = &total{}
				order = append(order, v.name)
			}
			sums[v.name].src += s
			sums[v.name].dst += d
			sums[v.name].all += a
			sums[v.name].n++
		}
	}
	h2 := sums["h2"]
	for _, name := range order {
		s := sums[name]
		ratio := ""
		if h2 != nil && h2.all > 0 {
			ratio = fmt.Sprintf(" (%.2fx h2)", s.all/h2.all)
		}
		t.Logf("SUMMARY %-28s mean over %d: src %.3f + dst %.3f = %.3f ms CPU/request%s",
			name, s.n, s.src/float64(s.n), s.dst/float64(s.n), s.all/float64(s.n), ratio)
	}
}
