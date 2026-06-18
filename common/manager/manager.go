package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/bpalermo/aether/common/telemetry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// defaultTraceSampleRate is the default head-sampling ratio for traces.
const defaultTraceSampleRate = 0.1

// Result holds the bootstrapped manager and an optional telemetry shutdown function.
type Result struct {
	Manager  ctrl.Manager
	Shutdown func(context.Context) error // nil if neither OTel metrics nor tracing enabled
}

// Bootstrap sets up telemetry (if enabled), creates a controller-runtime Manager,
// and registers the standard health and readiness probes.
func Bootstrap(ctx context.Context, cfg Config, serviceName, serviceVersion string) (*Result, error) {
	telemetryCfg := telemetry.Config{
		ServiceName:     serviceName,
		ServiceVersion:  serviceVersion,
		OTLPEndpoint:    cfg.OTLPEndpoint,
		TraceSampleRate: cfg.TraceSampleRate,
		TraceExport:     cfg.TracingExport,
	}

	var shutdowns []func(context.Context) error
	if cfg.OTelEnabled {
		metricsShutdown, err := telemetry.Setup(ctx, telemetryCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to setup telemetry: %w", err)
		}
		shutdowns = append(shutdowns, metricsShutdown)
	}
	// Tracing is always installed: a TracerProvider issues the span contexts that
	// give logs their trace_id/span_id (the slog/otelslog correlation), which is
	// useful regardless of other telemetry. Span EXPORT stays opt-in (TraceExport)
	// — see SetupTracing.
	tracingShutdown, err := telemetry.SetupTracing(ctx, telemetryCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to setup tracing: %w", err)
	}
	shutdowns = append(shutdowns, tracingShutdown)

	var shutdown func(context.Context) error
	if len(shutdowns) > 0 {
		shutdown = func(ctx context.Context) error {
			var errs []error
			for _, fn := range shutdowns {
				errs = append(errs, fn(ctx))
			}
			return errors.Join(errs...)
		}
	}

	opts := ctrl.Options{
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		Metrics:                telemetry.ManagerMetricsOptions(cfg.MetricsEnabled, cfg.MetricsBindAddress),
	}
	if cfg.CacheOptions != nil {
		opts.Cache = *cfg.CacheOptions
	}
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	if err = m.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("failed to set up health check: %w", err)
	}
	if err = m.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("failed to set up ready check: %w", err)
	}
	if err = m.AddReadyzCheck("cache-sync", telemetry.CacheSyncChecker(m)); err != nil {
		return nil, fmt.Errorf("failed to set up cache sync ready check: %w", err)
	}

	return &Result{
		Manager:  m,
		Shutdown: shutdown,
	}, nil
}
