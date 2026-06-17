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

	// Filter: OR(response_flag, status>=500, runtime sample) so failures always log.
	require.NotNil(t, al.GetFilter().GetOrFilter())
	assert.Len(t, al.GetFilter().GetOrFilter().GetFilters(), 3)

	// Decode the OTel config: collector cluster default + reporter attribute.
	var cfg otelaccesslogv3.OpenTelemetryAccessLogConfig
	require.NoError(t, proto.Unmarshal(al.GetTypedConfig().GetValue(), &cfg))
	assert.Equal(t, defaultCollectorName, cfg.GetCommonConfig().GetGrpcService().GetEnvoyGrpc().GetClusterName())

	var reporter string
	for _, kv := range cfg.GetAttributes().GetValues() {
		if kv.GetKey() == "reporter" {
			reporter = kv.GetValue().GetStringValue()
		}
	}
	assert.Equal(t, ReporterDestination, reporter)
}
