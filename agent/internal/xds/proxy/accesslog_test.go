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
	assert.Nil(t, buildAccessLog(ReporterSource))
}

func TestBuildAccessLogEnabled(t *testing.T) {
	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	logs := buildAccessLog(ReporterDestination)
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

	var reporter string
	for _, kv := range cfg.GetAttributes().GetValues() {
		if kv.GetKey() == "reporter" {
			reporter = kv.GetValue().GetStringValue()
		}
	}
	assert.Equal(t, ReporterDestination, reporter)
}
