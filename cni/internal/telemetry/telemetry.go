// Package telemetry provides opt-in OTel tracing for the short-lived CNI
// plugin binary. Unlike the agent and registrar (which configure telemetry via
// flags on a long-running manager), the plugin is exec'd by the container
// runtime per CNI operation, so setup is gated on the standard
// OTEL_EXPORTER_OTLP_ENDPOINT environment variable and spans are force-flushed
// before the process exits.
package telemetry

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.uber.org/zap"
)

const (
	// serviceName identifies the CNI plugin in trace backends.
	serviceName = "aether-cni"
	// flushTimeout bounds both each OTLP export and the final flush so a
	// missing collector can never stall a CNI operation past pod-start budgets.
	flushTimeout = 2 * time.Second
)

// Setup initializes tracing when OTEL_EXPORTER_OTLP_ENDPOINT is set, and
// returns a flush function that must be called before the process exits (spans
// are batched; an unflushed exit loses them). When the variable is unset —
// the default — tracing stays a no-op and the returned flush does nothing.
//
// The exporter reads its endpoint and TLS mode from the OTEL_EXPORTER_OTLP_*
// environment variables (an http:// scheme implies insecure transport).
func Setup(ctx context.Context, logger *zap.Logger) func() {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func() {}
	}

	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithTimeout(flushTimeout))
	if err != nil {
		logger.Warn("failed to create OTLP trace exporter; tracing disabled", zap.Error(err))
		return func() {}
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		logger.Warn("failed to create OTel resource; tracing disabled", zap.Error(err))
		return func() {}
	}

	// CNI operations happen at pod-creation/deletion rate, so when tracing is
	// explicitly enabled every operation is worth a trace — no head sampling.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		defer cancel()
		if err := provider.Shutdown(shutdownCtx); err != nil {
			logger.Debug("failed to flush traces", zap.Error(err))
		}
	}
}
