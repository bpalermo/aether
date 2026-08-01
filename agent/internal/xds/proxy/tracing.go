package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tracev3 "github.com/envoyproxy/go-control-plane/envoy/config/trace/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

const (
	// otelTracerName is the Envoy OpenTelemetry tracer extension.
	otelTracerName = "envoy.tracers.opentelemetry"
	// proxyTraceServiceName is the service.name the proxy's spans carry in Tempo.
	proxyTraceServiceName = "aether-proxy"
)

// TracingConfig is the global proxy data-plane tracing configuration. Set ONCE at
// agent startup (SetTracingConfig) before the snapshot cache builds any listener,
// then read without locking — same set-once pattern as AccessLogConfig.
type TracingConfig struct {
	// Enabled adds an OpenTelemetry tracer to every HCM so the proxy generates and
	// propagates W3C trace context (traceparent) and exports spans.
	Enabled bool
	// CollectorCluster is the OTLP gRPC cluster spans are exported to (the proxy
	// bootstrap's "otel_collector"). Empty defaults to defaultCollectorName.
	CollectorCluster string
	// SampleRate is the fraction (0.0-1.0) of requests traced (Envoy random
	// sampling). At high data-plane QPS keep this low.
	SampleRate float64
}

var tracingConfig TracingConfig

// SetTracingConfig sets the global proxy tracing configuration. Call once before
// the manager starts.
func SetTracingConfig(c TracingConfig) { tracingConfig = c }

// buildTracing returns the HCM tracing config — an OpenTelemetry tracer exporting
// to the collector cluster with W3C propagation, sampled at SampleRate — or nil
// when proxy tracing is disabled. A traced HCM stamps traceparent (so access logs
// populate it) and emits spans that join the same trace across source/dest hops.
func buildTracing() *hcmv3.HttpConnectionManager_Tracing {
	if !tracingConfig.Enabled {
		return nil
	}
	cluster := tracingConfig.CollectorCluster
	if cluster == "" {
		cluster = defaultCollectorName
	}

	provider := &tracev3.OpenTelemetryConfig{
		GrpcService: &corev3.GrpcService{
			TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: cluster},
			},
		},
		ServiceName: proxyTraceServiceName,
	}

	return &hcmv3.HttpConnectionManager_Tracing{
		// Envoy's Percent is 0-100; SampleRate is a 0.0-1.0 fraction.
		RandomSampling: &typev3.Percent{Value: tracingConfig.SampleRate * 100},
		Provider: &tracev3.Tracing_Http{
			Name:       otelTracerName,
			ConfigType: &tracev3.Tracing_Http_TypedConfig{TypedConfig: config.TypedConfig(provider)},
		},
	}
}
