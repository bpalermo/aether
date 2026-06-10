package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// otlpTraceTimeout bounds each OTLP trace export so a missing or slow
// collector cannot block the batch span processor's export goroutine.
const otlpTraceTimeout = 5 * time.Second

// SetupTracing creates an OTel TracerProvider exporting spans to the OTLP gRPC
// collector at cfg.OTLPEndpoint via a batching span processor, using parent-based
// head sampling at cfg.TraceSampleRate. It sets the provider as the global OTel
// TracerProvider and installs the W3C TraceContext + Baggage propagators, so
// instrumentation anywhere (e.g. otelgrpc stats handlers) picks it up via
// otel.Tracer(). The returned shutdown function flushes and stops the provider.
//
// cfg.OTLPEndpoint must be non-empty: unlike metrics (which fall back to the
// Prometheus bridge), traces have no export path without a collector.
func SetupTracing(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if cfg.OTLPEndpoint == "" {
		return nil, fmt.Errorf("tracing requires an OTLP endpoint")
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(otlpTraceTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceSampleRate))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return provider.Shutdown, nil
}
