package hotrestart

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	// telemetryServiceName identifies the supervisor in metric backends.
	telemetryServiceName = "aether-proxy-supervisor"
	// telemetryShutdownTimeout bounds the final flush. The supervisor exits
	// non-zero on a wedge — the flush must reliably get the wedge counter out
	// before the process dies, but must never stall the restart.
	telemetryShutdownTimeout = 5 * time.Second
	// otlpTimeout bounds each OTLP export.
	otlpTimeout = 10 * time.Second
)

// TelemetryConfig configures the supervisor's metrics pipeline.
type TelemetryConfig struct {
	// OTLPEndpoint is the OTLP gRPC collector metrics are pushed to
	// (host:port, insecure). Required: the supervisor is push-only — it runs
	// outside the controller-runtime manager (no shared Prometheus registry to
	// bridge into) and in the host netns, where a scrape port would collide
	// between surge predecessor and successor pods.
	OTLPEndpoint string
	// ServiceVersion is the build version recorded on the OTel resource.
	ServiceVersion string
}

// Telemetry is the supervisor's metrics pipeline: a plain OTel SDK
// MeterProvider pushing to the collector over OTLP. No Prometheus registry,
// no scrape endpoint.
type Telemetry struct {
	provider *sdkmetric.MeterProvider
}

// NewTelemetry builds the supervisor metrics pipeline.
func NewTelemetry(ctx context.Context, cfg TelemetryConfig) (*Telemetry, error) {
	if cfg.OTLPEndpoint == "" {
		return nil, fmt.Errorf("supervisor telemetry requires an OTLP endpoint")
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(telemetryServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	exporter, err := otlpmetricgrpc.New(
		ctx,
		otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(otlpTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP gRPC exporter: %w", err)
	}

	return &Telemetry{
		provider: sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		),
	}, nil
}

// Meter returns the meter to register supervisor instruments on.
func (t *Telemetry) Meter() metric.Meter {
	return t.provider.Meter("aether/proxy-supervisor")
}

// Shutdown flushes and stops the provider. Called on every supervisor exit
// path — including the wedge watchdog's fatal exit, where the final flush is
// the crash forensics.
func (t *Telemetry) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
	defer cancel()
	return t.provider.Shutdown(ctx)
}
