package proxy

import (
	"testing"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	otelaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/open_telemetry/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestBuildAccessLogDisabled(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: false})
	assert.Nil(t, buildAccessLog(ReporterSource, "svc-1-abc", "aether-test"))
}

func TestBuildAccessLogEnabled(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	logs := buildAccessLog(ReporterDestination, "svc-2-xyz", "aether-test")
	require.Len(t, logs, 1)
	al := logs[0]
	assert.Equal(t, "envoy.access_loggers.open_telemetry", al.GetName())

	// Filter: AND(not health check, not probe path, OR(response_flag, status>=500,
	// runtime sample)) so probes are dropped and failures always log.
	and := al.GetFilter().GetAndFilter()
	require.NotNil(t, and)
	require.Len(t, and.GetFilters(), 3)
	assert.NotNil(t, and.GetFilters()[0].GetNotHealthCheckFilter())
	probe := and.GetFilters()[1].GetHeaderFilter()
	require.NotNil(t, probe)
	assert.Equal(t, ":path", probe.GetHeader().GetName())
	assert.Equal(t, meshProbePathPrefix, probe.GetHeader().GetStringMatch().GetPrefix())
	assert.True(t, probe.GetHeader().GetInvertMatch())
	assert.Len(t, and.GetFilters()[2].GetOrFilter().GetFilters(), 3)

	// Decode the OTel config: collector cluster default (current grpc_service field,
	// not deprecated common_config) + log name + reporter attribute.
	var cfg otelaccesslogv3.OpenTelemetryAccessLogConfig
	require.NoError(t, proto.Unmarshal(al.GetTypedConfig().GetValue(), &cfg))
	assert.Equal(t, defaultCollectorName, cfg.GetGrpcService().GetEnvoyGrpc().GetClusterName())
	assert.Equal(t, accessLogName, cfg.GetLogName())

	attrs := map[string]string{}
	for _, kv := range cfg.GetAttributes().GetValues() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	assert.Equal(t, ReporterDestination, attrs["reporter"])
	// Literal pod identity for the listener's local pod.
	assert.Equal(t, "svc-2-xyz", attrs["pod_name"])
	assert.Equal(t, "aether-test", attrs["pod_namespace"])

	// The full Istio default field set plus the W3C traceparent must be present as
	// structured attributes.
	for _, key := range []string{
		"start_time", "method", "path", "protocol", "response_code", "response_flags",
		"response_code_details", "connection_termination_details",
		"upstream_transport_failure_reason", "bytes_received", "bytes_sent", "duration_ms",
		"upstream_service_time", "x_forwarded_for", "user_agent", "x_request_id",
		"authority", "upstream_host", "upstream_cluster", "upstream_local_address",
		"downstream_local_address", "downstream_remote_address", "requested_server_name",
		"route_name", "traceparent", "source_netns", "source_spiffe_id",
		"rbac_shadow_result", "rbac_shadow_policy",
	} {
		assert.Contains(t, attrs, key, "missing access-log attribute %q", key)
	}
	assert.Equal(t, "%REQ(TRACEPARENT)%", attrs["traceparent"])
	// Upstream end-of-stream timing (#1009): both anchored at the first upstream
	// response byte, so a DC line shows whether the upstream FIN had landed
	// (upstream_rx_ms) and whether the response finished downstream
	// (downstream_tx_end_ms). The exact operator strings are the contract the
	// soak grader's benign-DC rule reads; //agent/test/envoy_validate proves the
	// pinned Envoy accepts them.
	assert.Equal(t, "%COMMON_DURATION(US_RX_BEG:US_RX_END:ms)%", attrs["upstream_rx_ms"])
	assert.Equal(t, "%COMMON_DURATION(US_RX_BEG:DS_TX_END:ms)%", attrs["downstream_tx_end_ms"])
	// RBAC shadow metadata keys must include the "aether_audit_" prefix — Envoy
	// prepends shadow_rules_stat_prefix to the metadata field name in
	// evaluateShadowEngine(), so a bare "shadow_engine_result" key always returns "-".
	assert.Equal(
		t,
		"%DYNAMIC_METADATA(envoy.filters.http.rbac:aether_audit_shadow_engine_result)%",
		attrs["rbac_shadow_result"],
		"rbac_shadow_result must use the aether_audit_-prefixed metadata key",
	)
	assert.Equal(
		t,
		"%DYNAMIC_METADATA(envoy.filters.http.rbac:aether_audit_shadow_effective_policy_id)%",
		attrs["rbac_shadow_policy"],
		"rbac_shadow_policy must use the aether_audit_-prefixed metadata key",
	)
}

// TestAccessLogCarriesClaimedSourceIdentity is issue #831's observability half:
// a log line already carries the VERIFIED peer identity (#824), but nothing said
// which identity this proxy INTENDED to present. Without both, a certificate
// leak between source workloads is invisible — the destination reports a
// perfectly valid mesh identity, just not the caller's.
//
// The two hard requirements are the key and the ":PLAIN" suffix. The key must be
// the one the originating chains actually stamp, and %FILTER_STATE(key)% with no
// format defaults to TYPED, which renders "-" for a HashableString / any
// Router::StringAccessorImpl (no serializeAsProto) — a silent, permanently
// empty field.
func TestAccessLogCarriesClaimedSourceIdentity(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	logs := buildAccessLog(ReporterSource, "pod-a", "aether-test")
	require.Len(t, logs, 1)
	var cfg otelaccesslogv3.OpenTelemetryAccessLogConfig
	require.NoError(t, proto.Unmarshal(logs[0].GetTypedConfig().GetValue(), &cfg))
	attrs := map[string]string{}
	for _, kv := range cfg.GetAttributes().GetValues() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}

	assert.Equal(t, "%FILTER_STATE("+SourceIdentityCertMapperFilterStateKey+":PLAIN)%", attrs["source_spiffe_id"],
		"source_spiffe_id must read the key the originating chains stamp, in PLAIN form")
	assert.NotContains(t, attrs["source_spiffe_id"], retiredSourceIdentityKey,
		"#1165 retired the aether.source.spiffe_id copy: no chain stamps it, so reading it renders \"-\" forever")
	assert.Contains(t, attrs["source_spiffe_id"], ":PLAIN)%",
		"without :PLAIN the formatter defaults to TYPED and renders \"-\" forever")

	// The claimed identity and the verified peer are complementary, never
	// substitutes: the point of the field is that the pair can disagree.
	assert.Equal(t, "%UPSTREAM_PEER_URI_SAN%", attrs["upstream_peer_uri_san"])
}

// TestAccessLogCarriesVerifiedPeerIdentity is issue #824: every other identity
// field in a log line is something the control plane baked into the emitting
// pod's own config, so a line can say who it THINKS it is but never who the
// other end actually proved to be. Each hop logs exactly the peer it verified:
// the inbound (destination) listener logs the caller's certificate, the egress
// (source) side logs the server certificate it validated.
func TestAccessLogCarriesVerifiedPeerIdentity(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	attrsFor := func(reporter string) map[string]string {
		logs := buildAccessLog(reporter, "pod-a", "aether-test")
		require.Len(t, logs, 1)
		var cfg otelaccesslogv3.OpenTelemetryAccessLogConfig
		require.NoError(t, proto.Unmarshal(logs[0].GetTypedConfig().GetValue(), &cfg))
		attrs := map[string]string{}
		for _, kv := range cfg.GetAttributes().GetValues() {
			attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		return attrs
	}

	dst := attrsFor(ReporterDestination)
	assert.Equal(t, "%DOWNSTREAM_PEER_URI_SAN%", dst["downstream_peer_uri_san"],
		"the inbound hop must log the CALLER's verified identity")
	assert.NotContains(t, dst, "upstream_peer_uri_san",
		"the upstream peer on an inbound listener is the local app over loopback: always empty")

	src := attrsFor(ReporterSource)
	assert.Equal(t, "%UPSTREAM_PEER_URI_SAN%", src["upstream_peer_uri_san"],
		"the egress hop must log the SERVER certificate it validated (#638's client-side view)")
	assert.NotContains(t, src, "downstream_peer_uri_san",
		"the downstream peer on an egress listener is the local app over loopback: always empty")

	// Neither operator may displace the identity fields a line already carries:
	// the peer identity is the VERIFIED counterpart to those, not a replacement.
	for _, key := range []string{"reporter", "pod_name", "pod_namespace"} {
		assert.Contains(t, dst, key)
		assert.Contains(t, src, key)
	}
}

// accessLogAttrs decodes an OTel access logger's attribute list, in order.
func accessLogAttrs(t *testing.T, logs []*accesslogv3.AccessLog) (keys []string, attrs map[string]string) {
	t.Helper()
	require.Len(t, logs, 1)
	var cfg otelaccesslogv3.OpenTelemetryAccessLogConfig
	require.NoError(t, proto.Unmarshal(logs[0].GetTypedConfig().GetValue(), &cfg))
	attrs = map[string]string{}
	for _, kv := range cfg.GetAttributes().GetValues() {
		keys = append(keys, kv.GetKey())
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return keys, attrs
}

// TestAccessLogCarriesGenerationAndConnectionTiming pins the exact format
// strings of the #1333 fields, on both reporters.
//
// The strings are the whole contract. `envoy --mode validate` accepts any
// time-point name (an unknown one is a dynamic time point nothing sets, and
// renders "-" forever), so the only two things that stand between a typo and a
// permanently empty column are this test, which says what the names ARE, and
// //agent/test/mtlspool TestAccessLogConnectionTiming, which shows the pinned
// Envoy renders these names as numbers on a TLS and on a QUIC connection.
func TestAccessLogCarriesGenerationAndConnectionTiming(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	want := map[string]string{
		// The supervisor's variable (hotrestart.RestartEpochEnv). Unset renders "-".
		"proxy_epoch":   "%ENVIRONMENT(AETHER_RESTART_EPOCH)%",
		"connection_id": "%CONNECTION_ID%",
		"ds_cx_age_ms":  "%COMMON_DURATION(DS_CX_BEG:DS_RX_BEG:ms)%",
		"ds_hs_ms":      "%COMMON_DURATION(DS_CX_BEG:DS_HS_END:ms)%",
		"us_tx_beg_ms":  "%COMMON_DURATION(DS_RX_BEG:US_TX_BEG:ms)%",
	}
	for _, reporter := range []string{ReporterSource, ReporterDestination} {
		keys, attrs := accessLogAttrs(t, buildAccessLog(reporter, "pod-a", "aether-test"))
		for key, op := range want {
			assert.Equal(t, op, attrs[key], "reporter %s: field %s", reporter, key)
		}
		assert.Len(t, attrs, len(keys), "reporter %s: an attribute key is repeated: %v", reporter, keys)
	}
}

// TestL4AccessLogCarriesGeneration: the connection-level log names the
// generation too, and only that. The request-anchored durations have no
// meaning on a tcp_proxy record (there is no request), and connection_id is
// left out with them rather than added to a stream where nothing joins on it.
func TestL4AccessLogCarriesGeneration(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	keys, attrs := accessLogAttrs(t, buildL4AccessLog("pod-a", "aether-test"))
	assert.Equal(t, "%ENVIRONMENT(AETHER_RESTART_EPOCH)%", attrs["proxy_epoch"])
	for _, key := range []string{"connection_id", "ds_cx_age_ms", "ds_hs_ms", "us_tx_beg_ms"} {
		assert.NotContains(t, attrs, key)
	}
	assert.Len(t, attrs, len(keys), "an attribute key is repeated: %v", keys)
}
