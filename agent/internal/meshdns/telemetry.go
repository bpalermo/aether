package meshdns

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
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

// SetupTelemetry installs a push-only OTel MeterProvider (OTLP gRPC, no Prometheus
// scrape endpoint) as the global provider, so the resolver's newMetrics() picks it
// up. The standalone daemon runs in the host netns with no controller-runtime
// manager, and a surge predecessor/successor would collide on a scrape port —
// push-only mirrors the proxy-supervisor. The returned shutdown flushes and stops
// the provider; it is a no-op when otlpEndpoint is empty. Call BEFORE NewServer so
// the global provider is set when meters are created.
func SetupTelemetry(ctx context.Context, otlpEndpoint, serviceVersion string) (func(), error) {
	if otlpEndpoint == "" {
		return func() {}, nil
	}
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

	exporter, err := otlpmetricgrpc.New(
		ctx,
		otlpmetricgrpc.WithEndpoint(otlpEndpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(otlpTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP gRPC exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	otel.SetMeterProvider(provider)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}, nil
}
