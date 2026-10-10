// Package setup wires OpenTelemetry providers for long-running binaries:
// an SDK MeterProvider bridged into controller-runtime's Prometheus registry,
// a TracerProvider, and a LoggerProvider, each with optional OTLP gRPC export.
//
// Only binary mains (agent, registrar, controller — via common/manager) may
// import this package: it drags in the OTel SDK and every exporter. Packages
// that merely instrument (spans, meters, attribute keys, gRPC stats handlers)
// must import the parent common/telemetry package instead.
package setup

import (
	"context"
	"fmt"
	"time"

	"aethermesh.dev/common/telemetry/serviceresource"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// otlpMetricTimeout bounds each OTLP metric export so a missing or slow
// collector cannot back up the periodic reader indefinitely.
const otlpMetricTimeout = 10 * time.Second

// Config holds telemetry setup parameters.
type Config struct {
	// ServiceName identifies the service (e.g. "aether-agent", "aether-registrar").
	ServiceName string
	// ServiceVersion is the build version of the service.
	ServiceVersion string
	// OTLPEndpoint is the OTLP gRPC collector endpoint (e.g. "localhost:4317").
	// Empty disables OTLP export.
	OTLPEndpoint string
	// TraceSampleRate is the head-sampling ratio for traces (0.0–1.0). Only
	// used by SetupTracing.
	TraceSampleRate float64
	// TraceExport attaches the OTLP span exporter. When false, SetupTracing still
	// installs a TracerProvider (so logs get trace_id) but exports no spans.
	TraceExport bool
	// SchedulerLatency adds the Go runtime's scheduler-latency histogram
	// (go.schedule.duration: how long runnable goroutines waited to run) to
	// every metric reader. It is one series of ~160 fixed buckets read from
	// runtime/metrics at collection time, so it costs no timer; it is opt-in per
	// component because only the node agent needs it (issue #1131).
	SchedulerLatency bool
	// WithoutHostName leaves host.name off the resource. This package is shared
	// by components that run in the host's network namespace (the node agent,
	// whose host.name is the node's) and components that do not (the registrar,
	// the controller, the edge control plane, whose host.name would be their POD
	// name, #1596), so the package cannot choose: the zero value keeps host.name,
	// and each component that is not hostNetwork says so. See
	// serviceresource.WithoutHost.
	WithoutHostName bool
}

// NewResource builds the OTel Resource shared by the meter, tracer and logger
// providers so every signal carries identical service identity attributes. Who
// decides each attribute is serviceresource's rule, shared with the binaries
// that do not link this package (mesh-dns, the proxy supervisor, the prober).
//
// It is exported so that each component can pin, in its own package, what the
// resource built from its own configuration carries (#1596).
func NewResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	var opts []serviceresource.Option
	if cfg.WithoutHostName {
		opts = append(opts, serviceresource.WithoutHost())
	}
	return serviceresource.New(ctx, cfg.ServiceName, cfg.ServiceVersion, opts...)
}

// readerProducers is one external metric producer per reader: a runtime
// Producer keeps per-collection state, so the two readers do not share one.
type readerProducers struct {
	prom, periodic sdkmetric.Producer
}

// runtimeProducers returns the external producers cfg asks for: the Go
// scheduler-latency histogram when cfg.SchedulerLatency is set.
func runtimeProducers(cfg Config) []readerProducers {
	if !cfg.SchedulerLatency {
		return nil
	}
	return []readerProducers{{prom: runtime.NewProducer(), periodic: runtime.NewProducer()}}
}

// Setup creates an OTel MeterProvider with a Prometheus exporter registered
// against controller-runtime's metrics registry. When OTLPEndpoint is non-empty,
// an OTLP gRPC periodic exporter is added as a second reader. A Resource with
// semantic convention attributes (service.name, service.version, deployment.environment.name)
// is attached to the provider. It sets the provider as the global OTel MeterProvider
// so any package can create meters via otel.Meter().
// The returned shutdown function flushes and stops the provider.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	res, err := NewResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	promOpts := []prometheus.Option{prometheus.WithRegisterer(ctrlmetrics.Registry)}
	var periodicOpts []sdkmetric.PeriodicReaderOption
	for _, p := range runtimeProducers(cfg) {
		promOpts = append(promOpts, prometheus.WithProducer(p.prom))
		periodicOpts = append(periodicOpts, sdkmetric.WithProducer(p.periodic))
	}

	promExporter, err := prometheus.New(promOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus exporter: %w", err)
	}

	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExporter),
	}

	if cfg.OTLPEndpoint != "" {
		grpcExporter, grpcErr := otlpmetricgrpc.New(
			ctx,
			otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlpmetricgrpc.WithInsecure(),
			otlpmetricgrpc.WithTimeout(otlpMetricTimeout),
		)
		if grpcErr != nil {
			return nil, fmt.Errorf("failed to create OTLP gRPC exporter: %w", grpcErr)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(grpcExporter, periodicOpts...)))
	}

	provider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(provider)

	if err := runtime.Start(); err != nil {
		return nil, fmt.Errorf("failed to start runtime metrics: %w", err)
	}

	return provider.Shutdown, nil
}
