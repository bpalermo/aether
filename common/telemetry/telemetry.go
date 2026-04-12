// Package telemetry provides OpenTelemetry instrumentation bridged into
// controller-runtime's Prometheus registry, with optional OTLP gRPC export.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Config holds telemetry setup parameters.
type Config struct {
	// ServiceName identifies the service (e.g. "aether-agent", "aether-registrar").
	ServiceName string
	// ServiceVersion is the build version of the service.
	ServiceVersion string
	// OTLPEndpoint is the OTLP gRPC collector endpoint (e.g. "localhost:4317").
	// Empty disables OTLP export.
	OTLPEndpoint string
}

// Setup creates an OTel MeterProvider with a Prometheus exporter registered
// against controller-runtime's metrics registry. When OTLPEndpoint is non-empty,
// an OTLP gRPC periodic exporter is added as a second reader. A Resource with
// semantic convention attributes (service.name, service.version, deployment.environment.name)
// is attached to the provider. It sets the provider as the global OTel MeterProvider
// so any package can create meters via otel.Meter().
// The returned shutdown function flushes and stops the provider.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
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

	promExporter, err := prometheus.New(prometheus.WithRegisterer(ctrlmetrics.Registry))
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus exporter: %w", err)
	}

	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExporter),
	}

	if cfg.OTLPEndpoint != "" {
		grpcExporter, grpcErr := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlpmetricgrpc.WithInsecure(),
		)
		if grpcErr != nil {
			return nil, fmt.Errorf("failed to create OTLP gRPC exporter: %w", grpcErr)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(grpcExporter)))
	}

	provider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(provider)

	if err := runtime.Start(); err != nil {
		return nil, fmt.Errorf("failed to start runtime metrics: %w", err)
	}

	return provider.Shutdown, nil
}
