package meshdns

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	// telemetryServiceName identifies the standalone resolver in metric backends.
	telemetryServiceName = "aether-mesh-dns"
	// telemetryShutdownTimeout bounds the final flush on shutdown.
	telemetryShutdownTimeout = 5 * time.Second
	// otlpTimeout bounds each OTLP export.
	otlpTimeout = 10 * time.Second
)

// Telemetry is the daemon's wired-up OTel plumbing. It is always non-nil, even when
// telemetry is disabled or failed, so the caller can defer Shutdown unconditionally.
type Telemetry struct {
	// LogHandler bridges the daemon's slog records to the OTLP LoggerProvider. nil
	// when log export is disabled or failed — the caller then logs to stderr only.
	LogHandler slog.Handler
	// Shutdown flushes and stops every installed provider. Never nil.
	Shutdown func()
	// Warnings are non-fatal degradations (e.g. runtime metrics or log export
	// unavailable) for the caller to log once its logger exists.
	Warnings []error
}

// SetupTelemetry installs a push-only OTel MeterProvider (OTLP gRPC, no Prometheus
// scrape endpoint) as the global provider — so the resolver's newMetrics() picks it
// up — plus Go runtime metrics and an OTLP LoggerProvider bridged to slog. The
// standalone daemon runs in the host netns with no controller-runtime manager, and a
// surge predecessor/successor would collide on a scrape port; push-only mirrors the
// proxy-supervisor.
//
// It deliberately does NOT use common/telemetry/setup: that package transitively
// reaches controller-runtime, which would undo the slim mesh-dns binary (#583). Only
// the light exporter/SDK packages are used here.
//
// Everything is a no-op when otlpEndpoint is empty. Call BEFORE NewServer so the
// global provider is set when meters are created, and use the returned LogHandler
// when building the daemon's slog logger.
func SetupTelemetry(ctx context.Context, otlpEndpoint, serviceVersion string) (*Telemetry, error) {
	tel := &Telemetry{Shutdown: func() {}}
	if otlpEndpoint == "" {
		return tel, nil
	}

	res, err := newTelemetryResource(ctx, serviceVersion)
	if err != nil {
		return tel, err
	}

	meterProvider, err := newMeterProvider(ctx, otlpEndpoint, res)
	if err != nil {
		return tel, err
	}
	otel.SetMeterProvider(meterProvider)
	shutdowns := []func(context.Context) error{meterProvider.Shutdown}

	// Go runtime metrics (goroutines, GC, heap): the daemon had none, so a goroutine
	// leak on the TCP serve path was invisible. Non-fatal — metrics keep working.
	if err := runtime.Start(); err != nil {
		tel.Warnings = append(tel.Warnings, fmt.Errorf("go runtime metrics disabled: %w", err))
	}

	// Logs: the daemon shipped none, so its logs only ever existed as pod stdout and
	// were lost on every roll — and several failure modes' only signal is a log line.
	// Non-fatal: stderr logging continues regardless.
	loggerProvider, err := newLoggerProvider(ctx, otlpEndpoint, res)
	if err != nil {
		tel.Warnings = append(tel.Warnings, fmt.Errorf("OTLP log export disabled: %w", err))
	} else {
		tel.LogHandler = otelslog.NewHandler(telemetryServiceName, otelslog.WithLoggerProvider(loggerProvider))
		shutdowns = append(shutdowns, loggerProvider.Shutdown)
	}

	tel.Shutdown = shutdownAll(shutdowns)
	return tel, nil
}

// newTelemetryResource builds the Resource shared by metrics and logs, so both signals
// carry the same service.name/version and the pod's OTEL_RESOURCE_ATTRIBUTES
// (k8s.node.name / k8s.pod.name / k8s.namespace.name) via WithFromEnv.
func newTelemetryResource(ctx context.Context, serviceVersion string) (*resource.Resource, error) {
	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(telemetryServiceName),
			semconv.ServiceVersion(serviceVersion),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}
	return res, nil
}

// newMeterProvider builds the push-only (periodic OTLP gRPC reader) MeterProvider.
func newMeterProvider(ctx context.Context, otlpEndpoint string, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	exporter, err := otlpmetricgrpc.New(
		ctx,
		otlpmetricgrpc.WithEndpoint(otlpEndpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(otlpTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP gRPC metric exporter: %w", err)
	}
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	), nil
}

// newLoggerProvider builds the batching OTLP gRPC LoggerProvider. It is NOT installed
// globally: the caller bridges it into slog explicitly (via Telemetry.LogHandler),
// which is the only place that emits records.
func newLoggerProvider(ctx context.Context, otlpEndpoint string, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	exporter, err := otlploggrpc.New(
		ctx,
		otlploggrpc.WithEndpoint(otlpEndpoint),
		otlploggrpc.WithInsecure(),
		otlploggrpc.WithTimeout(otlpTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP gRPC log exporter: %w", err)
	}
	return sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	), nil
}

// shutdownAll returns a func that flushes and stops every provider under one bounded
// timeout, on a fresh context (the run context is already cancelled by then).
func shutdownAll(fns []func(context.Context) error) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		for _, fn := range fns {
			_ = fn(ctx)
		}
	}
}
