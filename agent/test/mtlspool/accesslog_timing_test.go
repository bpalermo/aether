package mtlspool

// The live half of the #1333 access-log fields.
//
// agent/internal/xds/proxy/accesslog.go adds proxy_epoch, connection_id,
// ds_cx_age_ms, ds_hs_ms and us_tx_beg_ms to every HTTP access-log line. Three
// of them are %COMMON_DURATION(A:B:ms)% over named time points, and the pinned
// Envoy does not reject a name it does not know: it treats it as a dynamic
// time point that nothing sets and renders "-" on every line. So
// `envoy --mode validate` (//agent/test/envoy_validate) proves the format
// PARSES and nothing more.
//
// This test proves it RENDERS. It runs a destination Envoy whose HTTP
// connection managers carry production's own access logger, taken unmodified
// out of the listeners proxy.NewInboundListener and proxy.NewInboundQUICListener
// build, behind an mTLS (h2) listener and an HTTP/3 listener; drives requests
// at it through a source Envoy (the production h2 cluster, then a production
// `quic:` twin); and reads the rows from an in-process OTLP logs receiver
// standing where the collector stands. What the test owns is the listener
// around the logger (loopback address, file certificates, one static "app"
// cluster) -- the same substitution the rest of this package makes.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlpcommonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// restartEpochEnv is the variable the proxy supervisor exports to each
	// Envoy child and proxy_epoch reads. Spelled out, not imported: this test
	// is the check that the two spellings agree with what Envoy reads.
	restartEpochEnv = "AETHER_RESTART_EPOCH"

	// otelCollectorCluster is the cluster production's access logger pushes
	// to (proxy.AccessLogConfig.CollectorCluster's default).
	otelCollectorCluster = "otel_collector"

	appClusterName = "app"

	// timingGap separates the two measured requests on one connection. It is
	// what distinguishes a real connection start from Envoy's fallback: with
	// the connection start unknown, DS_CX_BEG is the REQUEST's own start and
	// ds_cx_age_ms reads 0 on every line.
	timingGap = 400 * time.Millisecond
)

// logRow is one access-log record as the collector would receive it: every
// attribute rendered to a string, plus the OTLP value kind each arrived as.
type logRow struct {
	attrs map[string]string
	kinds map[string]string
}

// logSink is an OTLP logs receiver (the collector's side of Envoy's
// envoy.access_loggers.open_telemetry).
type logSink struct {
	collogsv1.UnimplementedLogsServiceServer
	addr string
	mu   sync.Mutex
	rows []logRow
}

func (s *logSink) Export(_ context.Context, req *collogsv1.ExportLogsServiceRequest) (*collogsv1.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				row := logRow{attrs: map[string]string{}, kinds: map[string]string{}}
				for _, kv := range rec.GetAttributes() {
					row.attrs[kv.GetKey()], row.kinds[kv.GetKey()] = renderAnyValue(kv.GetValue())
				}
				s.rows = append(s.rows, row)
			}
		}
	}
	return &collogsv1.ExportLogsServiceResponse{}, nil
}

// renderAnyValue returns an OTLP attribute value as text and the kind it was
// sent as, so a field that arrives as a number is reported as such.
func renderAnyValue(v *otlpcommonv1.AnyValue) (text, kind string) {
	switch x := v.GetValue().(type) {
	case *otlpcommonv1.AnyValue_StringValue:
		return x.StringValue, "string"
	case *otlpcommonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10), "int"
	case *otlpcommonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64), "double"
	case *otlpcommonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue), "bool"
	default:
		return fmt.Sprintf("%v", v), fmt.Sprintf("%T", x)
	}
}

func startLogSink(t *testing.T) *logSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &logSink{addr: ln.Addr().String()}
	gs := grpc.NewServer()
	collogsv1.RegisterLogsServiceServer(gs, s)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	return s
}

// rowFor returns the one destination-reporter row for a request path, waiting
// for it (Envoy's logger batches and flushes on a timer).
func (s *logSink) rowFor(t *testing.T, path string) logRow {
	t.Helper()
	var got []logRow
	require.Eventuallyf(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		got = got[:0]
		for _, r := range s.rows {
			if r.attrs["path"] == path && r.attrs["reporter"] == proxy.ReporterDestination {
				got = append(got, r)
			}
		}
		return len(got) > 0
	}, 20*time.Second, 100*time.Millisecond, "no destination access-log row for %s reached the OTLP receiver", path)
	require.Lenf(t, got, 1, "%s was requested once and must be logged once", path)
	return got[0]
}

// productionAccessLog returns the access logger production attaches to the
// first HTTP connection manager of l, exactly as built.
func productionAccessLog(t *testing.T, l *listenerv3.Listener) []*accesslogv3.AccessLog {
	t.Helper()
	require.NotNil(t, l)
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			var hcm hcmv3.HttpConnectionManager
			if !f.GetTypedConfig().MessageIs(&hcm) {
				continue
			}
			require.NoError(t, f.GetTypedConfig().UnmarshalTo(&hcm))
			require.NotEmptyf(t, hcm.GetAccessLog(), "listener %s: the production HCM carries no access log", l.GetName())
			return hcm.GetAccessLog()
		}
	}
	t.Fatalf("listener %s has no HTTP connection manager", l.GetName())
	return nil
}

// inboundAccessLogs builds production's per-pod inbound listeners (mTLS TCP
// and HTTP/3) for a throwaway pod and returns the access logger of each.
func inboundAccessLogs(t *testing.T) (tcp, quic []*accesslogv3.AccessLog) {
	t.Helper()
	proxy.SetAccessLogConfig(proxy.AccessLogConfig{Enabled: true, SuccessSampleRate: 100})
	defer proxy.SetAccessLogConfig(proxy.AccessLogConfig{})
	pod := &cniv1.CNIPod{
		Name: "echo-0", Namespace: "demo", ServiceAccount: "echo",
		NetworkNamespace: "/var/run/netns/echo-0", ContainerId: "abc123", Ips: []string{"10.0.0.7"},
	}
	tl, err := proxy.NewInboundListener(pod, trustDomain, false, false, nil, nil)
	require.NoError(t, err)
	ql, err := proxy.NewInboundQUICListener(pod, trustDomain, "mesh.internal", false, false, nil, nil)
	require.NoError(t, err)
	return productionAccessLog(t, tl), productionAccessLog(t, ql)
}

// appHCM routes everything to the "app" cluster, as the inbound listener
// routes to the pod's application, and carries the given access logger.
func appHCM(name string, codec hcmv3.HttpConnectionManager_CodecType, accessLog []*accesslogv3.AccessLog) *hcmv3.HttpConnectionManager {
	return &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  codec,
		AccessLog:  accessLog,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name: name,
			VirtualHosts: []*routev3.VirtualHost{{
				Name: "all", Domains: []string{"*"},
				Routes: []*routev3.Route{{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_Route{Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: appClusterName},
						Timeout:          durationpb.New(10 * time.Second),
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

// startLoggingDestination runs a destination Envoy: an mTLS h2 listener and an
// HTTP/3 listener on one port number, both routing to appAddr and both logging
// with production's inbound access logger to the OTLP receiver at sinkAddr.
func startLoggingDestination(t *testing.T, p *pki, appAddr, sinkAddr string) (addr string) {
	t.Helper()
	tcpLog, quicLog := inboundAccessLogs(t)
	port := freePort(t)
	hcmFilter := func(h *hcmv3.HttpConnectionManager) []*listenerv3.Filter {
		return []*listenerv3.Filter{{
			Name:       "envoy.filters.network.http_connection_manager",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(h)},
		}}
	}
	h3HCM := appHCM("dest_h3", hcmv3.HttpConnectionManager_HTTP3, quicLog)
	h3HCM.Http3ProtocolOptions = &corev3.Http3ProtocolOptions{}
	tcp := &listenerv3.Listener{
		Name:    "dest_h2",
		Address: socketAddress("127.0.0.1", port),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: hcmFilter(appHCM("dest_h2", hcmv3.HttpConnectionManager_AUTO, tcpLog)),
			TransportSocket: &corev3.TransportSocket{
				Name:       "envoy.transport_sockets.tls",
				ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(destTLS(t, p, "h2"))},
			},
		}},
	}
	h3 := &listenerv3.Listener{
		Name: "dest_h3",
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Protocol: corev3.SocketAddress_UDP, Address: "127.0.0.1",
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(port)},
		}}},
		EnableReusePort:   wrapperspb.Bool(true),
		UdpListenerConfig: proxy.InboundQUICUDPListenerConfig(),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: hcmFilter(h3HCM),
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
	static := func(name, hostPort string, h2 bool) *clusterv3.Cluster {
		c := &clusterv3.Cluster{
			Name:                 name,
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			ConnectTimeout:       durationpb.New(5 * time.Second),
			LoadAssignment:       staticEndpoint(name, hostPort),
		}
		if h2 {
			c.TypedExtensionProtocolOptions = map[string]*anypb.Any{
				config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
			}
		}
		return c
	}
	bs := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: "dest", Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Listeners: []*listenerv3.Listener{tcp, h3},
			Clusters:  []*clusterv3.Cluster{static(appClusterName, appAddr, false), static(otelCollectorCluster, sinkAddr, true)},
		},
	}
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	runEnvoy(t, "dest", bs, 1, map[string]string{"dest_h2": addr})
	return addr
}

// getPath sends one request for path through the source proxy.
func getPath(t *testing.T, c *http.Client, base, path string) (status int, err error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	require.NoError(t, err)
	req.Host = quicDestFQDN
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// numeric asserts an access-log field rendered as a non-negative integer and
// returns it. "-" is what a mistyped or unset time point renders.
//
// Unsigned 64-bit on purpose: a QUIC connection's %CONNECTION_ID% is a hash
// that uses the whole range (8708995993204622487 in one run), unlike a TCP
// connection's small counter.
func numeric(t *testing.T, label string, row logRow, key string) uint64 {
	t.Helper()
	raw, ok := row.attrs[key]
	require.Truef(t, ok, "%s: the row has no %s attribute (attributes: %v)", label, key, sortedKeys(row.attrs))
	n, err := strconv.ParseUint(raw, 10, 64)
	require.NoErrorf(t, err, "%s: %s rendered %q, not a number -- a mistyped or never-set time point renders \"-\"", label, key, raw)
	return n
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestAccessLogConnectionTiming: on a destination row, proxy_epoch is the
// restart epoch the process was started with ("-" without one), and
// connection_id, ds_cx_age_ms, ds_hs_ms and us_tx_beg_ms are numbers -- on an
// mTLS h2 downstream connection and on an HTTP/3 one.
//
// Two requests ride ONE connection timingGap apart, and the assertions that
// give the test its power are the relations between their rows:
//
//   - same connection_id (the source proxy pooled them onto one connection);
//   - ds_cx_age_ms grew by about timingGap. Envoy falls back to the request's
//     own start when it has no connection start, which would read 0 on both
//     rows: this is what shows the HTTP/3 connection manager really does see
//     the QUIC session's start;
//   - the same ds_hs_ms on both rows: it is a property of the connection.
func TestAccessLogConnectionTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real Envoys; skipped under -test.short")
	}
	for _, tc := range []struct {
		name     string
		viaH3    bool
		epoch    string // "" = the variable is not set
		protocol string
	}{
		{name: "tls", viaH3: false, epoch: "7", protocol: "HTTP/2"},
		{name: "quic", viaH3: true, epoch: "3", protocol: "HTTP/3"},
		// An agent that ships this format to a proxy whose supervisor does not
		// export the variable yet: the field must read "-" and nothing else
		// may change.
		{name: "tls/epoch-unset", viaH3: false, epoch: "", protocol: "HTTP/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The Envoys inherit this process's environment. t.Setenv first so
			// the original value is restored either way.
			t.Setenv(restartEpochEnv, tc.epoch)
			wantEpoch := tc.epoch
			if tc.epoch == "" {
				require.NoError(t, os.Unsetenv(restartEpochEnv))
				wantEpoch = "-"
			}

			p := newPKI(t)
			sink := startLogSink(t)
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			t.Cleanup(app.Close)
			dest := startLoggingDestination(t, p, strings.TrimPrefix(app.URL, "http://"), sink.addr)
			src := startCostSource(t, p, dest, dest, 1, tc.viaH3, nil)

			client := &http.Client{Timeout: 10 * time.Second}
			base := "http://" + src.addrs["source_a"]
			// Warm-up: until the source has its secrets and one request made it
			// end to end, so the two measured requests reuse that connection.
			require.Eventually(t, func() bool {
				status, err := getPath(t, client, base, "/warmup")
				return err == nil && status == http.StatusOK
			}, 20*time.Second, 200*time.Millisecond, "no 200 through the source proxy")

			for _, path := range []string{"/first", "/second"} {
				status, err := getPath(t, client, base, path)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status)
				if path == "/first" {
					time.Sleep(timingGap)
				}
			}

			first, second := sink.rowFor(t, "/first"), sink.rowFor(t, "/second")
			for _, key := range []string{"proxy_epoch", "connection_id", "ds_cx_age_ms", "ds_hs_ms", "us_tx_beg_ms", "protocol"} {
				t.Logf("%s: %-13s first=%q second=%q (sent as OTLP %s)", tc.name, key, first.attrs[key], second.attrs[key], first.kinds[key])
			}

			for label, row := range map[string]logRow{"/first": first, "/second": second} {
				assert.Equalf(t, tc.protocol, row.attrs["protocol"], "%s did not ride the path under test", label)
				assert.Equalf(t, wantEpoch, row.attrs["proxy_epoch"], "%s: proxy_epoch", label)
				assert.Positivef(t, numeric(t, label, row, "connection_id"), "%s: connection_id 0 is Envoy's \"no connection id\"", label)
				numeric(t, label, row, "us_tx_beg_ms")
			}
			assert.Equal(t, first.attrs["connection_id"], second.attrs["connection_id"],
				"both requests must ride one downstream connection, or the age comparison below means nothing")

			age1, age2 := numeric(t, "/first", first, "ds_cx_age_ms"), numeric(t, "/second", second, "ds_cx_age_ms")
			// 50 ms of slack under the gap: the durations are truncated to
			// milliseconds and the gap is measured by this process, not Envoy.
			assert.GreaterOrEqualf(t, int64(age2)-int64(age1), (timingGap - 50*time.Millisecond).Milliseconds(),
				"ds_cx_age_ms must be anchored at the CONNECTION's start: %d ms then %d ms across a %s gap (0 and 0 = Envoy fell back to the request's own start)",
				age1, age2, timingGap)

			hs1, hs2 := numeric(t, "/first", first, "ds_hs_ms"), numeric(t, "/second", second, "ds_hs_ms")
			assert.Equal(t, hs1, hs2, "ds_hs_ms is a property of the connection: every row on one connection repeats it")
			assert.LessOrEqual(t, hs1, age1, "the handshake ended before the first request on the connection started")
		})
	}
}
