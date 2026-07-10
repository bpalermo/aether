package proxy

import (
	"testing"

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
		"route_name", "traceparent", "source_netns",
		"rbac_shadow_result", "rbac_shadow_policy",
	} {
		assert.Contains(t, attrs, key, "missing access-log attribute %q", key)
	}
	assert.Equal(t, "%REQ(TRACEPARENT)%", attrs["traceparent"])
	// RBAC AUDIT visibility surfaces the filter's shadow dynamic metadata (proposal 025).
	assert.Equal(t, "%DYNAMIC_METADATA(envoy.filters.http.rbac:shadow_engine_result)%", attrs["rbac_shadow_result"])
}
