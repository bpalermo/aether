package setup

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

// SetupTracing installs a global OTel TracerProvider (parent-based head sampling
// at cfg.TraceSampleRate) plus the W3C TraceContext + Baggage propagators, so
// instrumentation anywhere (e.g. otelgrpc stats handlers) issues valid span
// contexts. That alone is what lets otelslog stamp trace_id/span_id onto logs
// emitted within a span — log↔request correlation needs no trace backend.
//
// Span EXPORT is opt-in (cfg.TraceExport): only then is an OTLP batch exporter
// attached. Exporting to a collector with no traces pipeline just floods stderr
// with "Unimplemented TraceService" errors, so export stays off until a real
// trace backend (Tempo/VictoriaTraces) exists. The returned shutdown flushes and
// stops the provider.
func SetupTracing(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceSampleRate))),
	}

	if cfg.TraceExport {
		if cfg.OTLPEndpoint == "" {
			return nil, fmt.Errorf("trace export requires an OTLP endpoint")
		}
		exporter, expErr := otlptracegrpc.New(
			ctx,
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithTimeout(otlpTraceTimeout),
		)
		if expErr != nil {
			return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", expErr)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}

	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return provider.Shutdown, nil
}
