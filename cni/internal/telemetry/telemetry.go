// Package telemetry provides opt-in OTel tracing and metrics for the
// short-lived CNI plugin binary. Unlike the agent and registrar (which
// configure telemetry via flags on a long-running manager), the plugin is
// exec'd by the container runtime per CNI operation, so:
//
//   - the OTLP endpoint arrives in the CNI netconf (written by cni-install),
//     with the standard OTEL_EXPORTER_OTLP_* environment variables as a
//     fallback — the plugin's environment is the runtime's, not a pod's;
//   - initialization is lazy (Init is called once the netconf is parsed,
//     inside the CNI command handlers);
//   - everything batched must be force-flushed before the process exits
//     (Flush), on success and failure paths alike.
package telemetry

import (
	"context"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.uber.org/zap"
)

const (
	// serviceName identifies the CNI plugin in telemetry backends.
	serviceName = "aether-cni"
	// flushTimeout bounds both each OTLP export and the final flush so a
	// missing collector can never stall a CNI operation past pod-start budgets.
	flushTimeout = 2 * time.Second
)

var (
	initOnce      sync.Once
	traceProvider *sdktrace.TracerProvider
	meterProvider *sdkmetric.MeterProvider
)

// Init initializes tracing and metrics once per process. endpoint is the OTLP
// gRPC collector from the CNI netconf; when empty, the standard
// OTEL_EXPORTER_OTLP_ENDPOINT environment variable is honored instead (the
// exporters parse it natively, including the scheme's insecure semantics).
// When neither is set — the default — telemetry stays a no-op.
//
// Safe to call from every CNI command handler; only the first call wins.
func Init(ctx context.Context, logger *zap.Logger, endpoint string) {
	if endpoint == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return
	}
	initOnce.Do(func() { setup(ctx, logger, endpoint) })
}

func setup(ctx context.Context, logger *zap.Logger, endpoint string) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		logger.Warn("failed to create OTel resource; telemetry disabled", zap.Error(err))
		return
	}

	// A netconf-provided endpoint is host:port over insecure transport (the
	// collector is in-cluster), matching the agent's --otlp-endpoint flag.
	// Without it, the exporters read the OTEL_EXPORTER_OTLP_* env vars.
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithTimeout(flushTimeout)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithTimeout(flushTimeout)}
	if endpoint != "" {
		traceOpts = append(traceOpts, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		logger.Warn("failed to create OTLP trace exporter; telemetry disabled", zap.Error(err))
		return
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		logger.Warn("failed to create OTLP metric exporter; telemetry disabled", zap.Error(err))
		return
	}

	// CNI operations happen at pod-creation/deletion rate, so when telemetry
	// is explicitly enabled every operation is worth recording — no sampling.
	// The periodic reader never fires in this short-lived process; Flush's
	// Shutdown performs the one real export.
	traceProvider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)
	meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)
	otel.SetTracerProvider(traceProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// Flush exports any buffered spans and metrics and stops the providers. Must
// run before the process exits on every path; a no-op when Init never ran.
func Flush(logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if traceProvider != nil {
		if err := traceProvider.Shutdown(ctx); err != nil {
			logger.Debug("failed to flush traces", zap.Error(err))
		}
	}
	if meterProvider != nil {
		if err := meterProvider.Shutdown(ctx); err != nil {
			logger.Debug("failed to flush metrics", zap.Error(err))
		}
	}
}
