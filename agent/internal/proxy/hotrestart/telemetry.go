package hotrestart

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	// telemetryServiceName identifies the supervisor in metric backends.
	telemetryServiceName = "aether-proxy-supervisor"
	// bindRetryInterval is how often a failed metrics listen is retried. The
	// supervisor shares the host network namespace, so during a surge upgrade
	// the predecessor pod still holds the metrics port; the successor must keep
	// retrying until the predecessor exits rather than treating the collision
	// as fatal.
	bindRetryInterval = 15 * time.Second
	// telemetryShutdownTimeout bounds the final flush. The supervisor exits
	// non-zero on a wedge — the flush must reliably get the wedge counter out
	// before the process dies, but must never stall the restart.
	telemetryShutdownTimeout = 5 * time.Second
	// otlpTimeout bounds each OTLP export.
	otlpTimeout = 10 * time.Second
)

// TelemetryConfig configures the supervisor's metrics outlet.
type TelemetryConfig struct {
	// BindAddress is the Prometheus scrape endpoint (host:port). The supervisor
	// is not under the controller-runtime manager, so it serves its own /metrics.
	BindAddress string
	// OTLPEndpoint optionally adds OTLP gRPC push export (e.g. "collector:4317").
	OTLPEndpoint string
	// ServiceVersion is the build version recorded on the OTel resource.
	ServiceVersion string
}

// Telemetry is the supervisor's standalone metrics pipeline: an OTel
// MeterProvider with a Prometheus exporter on a private registry (the
// supervisor must not share the agent's controller-runtime registry — it is a
// separate process) plus an optional OTLP push reader.
type Telemetry struct {
	provider *sdkmetric.MeterProvider
	registry *prometheus.Registry
	cfg      TelemetryConfig
	log      logr.Logger
}

// NewTelemetry builds the supervisor metrics pipeline. It does not start the
// HTTP endpoint; call Serve for that.
func NewTelemetry(ctx context.Context, cfg TelemetryConfig, log logr.Logger) (*Telemetry, error) {
	res, err := resource.New(ctx,
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

	registry := prometheus.NewRegistry()
	promExporter, err := otelprom.New(otelprom.WithRegisterer(registry))
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
			otlpmetricgrpc.WithTimeout(otlpTimeout),
		)
		if grpcErr != nil {
			return nil, fmt.Errorf("failed to create OTLP gRPC exporter: %w", grpcErr)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(grpcExporter)))
	}

	return &Telemetry{
		provider: sdkmetric.NewMeterProvider(opts...),
		registry: registry,
		cfg:      cfg,
		log:      log.WithName("supervisor-telemetry"),
	}, nil
}

// Meter returns the meter to register supervisor instruments on.
func (t *Telemetry) Meter() metric.Meter {
	return t.provider.Meter("aether/proxy-supervisor")
}

// Serve runs the Prometheus /metrics endpoint until ctx is canceled. A bind
// failure is retried, not fatal: during a surge upgrade the predecessor pod
// (same host netns) still owns the port, and the successor inherits it once
// the predecessor exits. Metrics export over OTLP is unaffected either way.
func (t *Telemetry) Serve(ctx context.Context) {
	if t.cfg.BindAddress == "" {
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{}))

	for {
		srv := &http.Server{Addr: t.cfg.BindAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()

		err := srv.ListenAndServe()
		if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		t.log.Info("metrics endpoint unavailable; retrying (predecessor pod may still hold the port)",
			"address", t.cfg.BindAddress, "retryIn", bindRetryInterval, "error", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(bindRetryInterval):
		}
	}
}

// Shutdown flushes and stops the provider. Called on every supervisor exit
// path — including the wedge watchdog's fatal exit, where the final flush is
// the crash forensics.
func (t *Telemetry) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
	defer cancel()
	return t.provider.Shutdown(ctx)
}
