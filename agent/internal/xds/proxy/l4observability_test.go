package proxy

import (
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	otelaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/open_telemetry/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestL4StatKeys pins the per-kind L4 stat keys (aether#1023): the kind prefix,
// the bare "<ns>/<svc>" service key, and a "_<port>" suffix only on a
// port-qualified cluster. None of them may contain a dot, or the chart's
// aether.cluster tag regex (`^cluster\.(([^.]+)\.)`) would cut the key short,
// nor a colon, which Envoy's stat-name sanitizer rewrites to "_" -- the label
// would then differ from the key every query and gate is written against.
func TestL4StatKeys(t *testing.T) {
	assert.Equal(t, "tcp_aether-test/tcp-echo", TCPStatKey("aether-test/tcp-echo"))
	assert.Equal(t, "tcp_aether-test/tcp-echo_9000", TCPPortStatKey("aether-test/tcp-echo", 9000))
	assert.Equal(t, "tcp_aether-test/tcp-echo", TCPPortStatKey("aether-test/tcp-echo", 0),
		"port 0 is the floor, mirroring TCPPortClusterName")
	assert.Equal(t, "udp_aether-test/udp-echo", UDPStatKey("aether-test/udp-echo"))

	assert.Empty(t, TCPStatKey(""), "an empty key keeps Envoy's per-name stats")
	assert.Empty(t, TCPPortStatKey("", 9000))
	assert.Empty(t, UDPStatKey(""))

	for _, k := range []string{
		TCPStatKey("aether-test/mixed-svc"),
		TCPPortStatKey("aether-test/mixed-svc", 9000),
		UDPStatKey("aether-test/udp-echo"),
	} {
		assert.NotContains(t, k, ".", "%q: a dot splits the aether_cluster tag", k)
		assert.NotContains(t, k, ":", "%q: Envoy sanitizes ':' to '_', so the label would not be the key", k)
		assert.NotEqual(t, "aether-test/mixed-svc", k, "an L4 key must never equal the HTTP cluster's key")
	}
}

// l4CaptureFixture returns a capture listener carrying every L4 chain kind:
// the TCP-primary floor (mesh-port, primary-port and any-port spellings), a
// non-primary per-port chain, a TCPRoute-weighted floor, TLSRoute SNI chains
// and the scoped-mode blackhole.
func l4CaptureFixture(t *testing.T, withPassthrough bool) *listenerv3.Listener {
	t.Helper()
	pod := &cniv1.CNIPod{
		Name: "client-0", Namespace: "aether-test",
		NetworkNamespace: "/var/run/netns/cni-client-0",
	}
	tcpEcho := TCPClusterName("aether-test/tcp-echo", "aether.internal")
	tlsA := TCPClusterName("aether-test/tls-a", "aether.internal")
	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/aether-test/sa/client", 18001, "aether.internal", false,
		[]CaptureTCPService{
			{
				ClusterName: tcpEcho, ClusterIP: "10.96.0.10", PrimaryIsTCP: true, PrimaryPort: 9000,
				TCPPorts: []uint32{9001},
				TLSRouteRules: []L4ServiceRoute{{
					SNIHostnames: []string{"a.example.com"},
					Backends:     []L4Backend{{Service: "aether-test/tls-a", Cluster: tlsA, Weight: 1}},
				}},
			},
			{
				ClusterName: TCPClusterName("aether-test/weighted", "aether.internal"), ClusterIP: "10.96.0.11", PrimaryIsTCP: true,
				TCPRouteRules: []L4ServiceRoute{{Backends: []L4Backend{
					{Service: "aether-test/tcp-echo", Cluster: tcpEcho, Weight: 1},
					{Service: "aether-test/tls-a", Cluster: tlsA, Weight: 1},
				}}},
			},
		}, withPassthrough, nil)
	require.NoError(t, err)
	return l
}

// tcpProxyOf returns the chain's tcp_proxy, or nil when it carries none.
func tcpProxyOf(t *testing.T, fc *listenerv3.FilterChain) *tcp_proxyv3.TcpProxy {
	t.Helper()
	for _, f := range fc.GetFilters() {
		if f.GetName() != "envoy.filters.network.tcp_proxy" {
			continue
		}
		tc := &tcp_proxyv3.TcpProxy{}
		require.NoError(t, f.GetTypedConfig().UnmarshalTo(tc))
		return tc
	}
	return nil
}

// TestCaptureL4ChainsCarryTheL4AccessLog is the unit half of the aether#1023
// access-log gate: with access logging on, every tcp_proxy chain on the capture
// listener carries exactly one OTel logger on the aether_l4_access_logs stream
// with the attribution fields, and the passthrough default chain carries none.
func TestCaptureL4ChainsCarryTheL4AccessLog(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	for _, withPassthrough := range []bool{true, false} {
		l := l4CaptureFixture(t, withPassthrough)
		kinds := map[string]bool{}
		for _, fc := range l.GetFilterChains() {
			tc := tcpProxyOf(t, fc)
			if tc == nil {
				continue
			}
			switch name := fc.GetName(); {
			case strings.HasPrefix(name, "cap_tls_"):
				kinds["tls"] = true
			case strings.HasPrefix(name, "cap_tcp_anyport_"):
				kinds["anyport"] = true
			case name == "cap_tcp_blackhole":
				kinds["blackhole"] = true
			case strings.HasSuffix(name, "_9001"):
				kinds["port"] = true
			case len(tc.GetWeightedClusters().GetClusters()) > 1:
				kinds["weighted"] = true
			}
			require.Len(t, tc.GetAccessLog(), 1, "chain %s: no L4 access log", fc.GetName())
			var cfg otelaccesslogv3.OpenTelemetryAccessLogConfig
			require.NoError(t, proto.Unmarshal(tc.GetAccessLog()[0].GetTypedConfig().GetValue(), &cfg))
			assert.Equal(t, L4AccessLogName, cfg.GetLogName(), "chain %s", fc.GetName())
			assert.Equal(t, defaultCollectorName, cfg.GetGrpcService().GetEnvoyGrpc().GetClusterName())

			attrs := map[string]string{}
			for _, kv := range cfg.GetAttributes().GetValues() {
				attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			assert.Equal(t, "client-0", attrs["pod_name"])
			assert.Equal(t, "aether-test", attrs["pod_namespace"])
			for key, op := range map[string]string{
				"upstream_cluster":                  "%UPSTREAM_CLUSTER%",
				"upstream_host":                     "%UPSTREAM_HOST%",
				"requested_server_name":             "%REQUESTED_SERVER_NAME%",
				"response_flags":                    "%RESPONSE_FLAGS%",
				"upstream_transport_failure_reason": "%UPSTREAM_TRANSPORT_FAILURE_REASON%",
				"bytes_sent":                        "%BYTES_SENT%",
				"bytes_received":                    "%BYTES_RECEIVED%",
				"duration_ms":                       "%DURATION%",
				"filter_chain_name":                 "%FILTER_CHAIN_NAME%",
				"downstream_local_address":          "%DOWNSTREAM_LOCAL_ADDRESS%",
			} {
				assert.Equal(t, op, attrs[key], "chain %s: field %s", fc.GetName(), key)
			}
			assert.Contains(t, attrs["source_netns"], "aether.network.network_namespace:PLAIN")
			assert.Contains(t, attrs["source_spiffe_id"], SourceIdentityCertMapperFilterStateKey+":PLAIN")
			_, hasReporter := attrs["reporter"]
			assert.False(t, hasReporter, "an L4 record must not carry `reporter`: the collector's HTTP identity counters key on it")
			// Connection-level only: no periodic flush, one record at close.
			assert.Nil(t, tc.GetAccessLogOptions(), "chain %s: a flush interval makes the log per-interval, not per-connection", fc.GetName())
		}
		want := map[string]bool{"tls": true, "anyport": true, "port": true, "weighted": true}
		if !withPassthrough {
			want["blackhole"] = true
		}
		assert.Equal(t, want, kinds, "withPassthrough=%v: the fixture must carry every L4 chain kind", withPassthrough)

		if withPassthrough {
			require.NotNil(t, l.GetDefaultFilterChain())
			tc := tcpProxyOf(t, l.GetDefaultFilterChain())
			require.NotNil(t, tc)
			assert.Empty(t, tc.GetAccessLog(), "the passthrough (all non-mesh egress) must not be logged")
		}
	}
}

// TestCaptureL4AccessLogOffWhenAccessLogsOff: the L4 log rides the existing
// access-log switch; off means the capture listener is unchanged.
func TestCaptureL4AccessLogOffWhenAccessLogsOff(t *testing.T) {
	SetAccessLogConfig(AccessLogConfig{})
	off := l4CaptureFixture(t, true)
	for _, fc := range off.GetFilterChains() {
		if tc := tcpProxyOf(t, fc); tc != nil {
			assert.Empty(t, tc.GetAccessLog(), "chain %s", fc.GetName())
		}
	}
}
