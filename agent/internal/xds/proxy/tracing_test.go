package proxy

import (
	"testing"

	tracev3 "github.com/envoyproxy/go-control-plane/envoy/config/trace/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestBuildTracingDisabled(t *testing.T) {
	t.Cleanup(func() { SetTracingConfig(TracingConfig{}) })
	SetTracingConfig(TracingConfig{Enabled: false})
	assert.Nil(t, buildTracing())
}

func TestBuildTracingEnabled(t *testing.T) {
	t.Cleanup(func() { SetTracingConfig(TracingConfig{}) })
	SetTracingConfig(TracingConfig{Enabled: true, SampleRate: 1.0})

	tr := buildTracing()
	require.NotNil(t, tr)
	// Fraction 1.0 -> Envoy Percent 100.
	assert.InDelta(t, 100.0, tr.GetRandomSampling().GetValue(), 1e-9)
	assert.Equal(t, otelTracerName, tr.GetProvider().GetName())

	var otel tracev3.OpenTelemetryConfig
	require.NoError(t, proto.Unmarshal(tr.GetProvider().GetTypedConfig().GetValue(), &otel))
	assert.Equal(t, defaultCollectorName, otel.GetGrpcService().GetEnvoyGrpc().GetClusterName())
	assert.Equal(t, proxyTraceServiceName, otel.GetServiceName())
}
