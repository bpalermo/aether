package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"aethermesh.dev/common/telemetry/setup"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// Result holds the bootstrapped manager and an optional telemetry shutdown function.
type Result struct {
	Manager  ctrl.Manager
	Shutdown func(context.Context) error // nil if neither OTel metrics nor tracing enabled
}

// Bootstrap sets up telemetry (best-effort), creates a controller-runtime Manager,
// and registers the standard health and readiness probes. Optional opts mutate
// the ctrl.Options before the manager is built (e.g. to supply a custom webhook
// server backed by SPIRE).
//
// Telemetry never fails the bootstrap: see setupTelemetry.
func Bootstrap(ctx context.Context, cfg Config, serviceName, serviceVersion string, l *slog.Logger, optFns ...func(*ctrl.Options)) (*Result, error) {
	shutdown := setupTelemetry(ctx, cfg, serviceName, serviceVersion, l)

	opts := ctrl.Options{
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		Metrics:                setup.ManagerMetricsOptions(cfg.MetricsEnabled, cfg.MetricsBindAddress),
		LeaderElection:         cfg.LeaderElection,
		LeaderElectionID:       cfg.LeaderElectionID,
	}
	if cfg.CacheOptions != nil {
		opts.Cache = *cfg.CacheOptions
	}
	for _, opt := range optFns {
		opt(&opts)
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
	if err = m.AddReadyzCheck("cache-sync", setup.CacheSyncChecker(m)); err != nil {
		return nil, fmt.Errorf("failed to set up cache sync ready check: %w", err)
	}

	return &Result{
		Manager:  m,
		Shutdown: shutdown,
	}, nil
}

// setupTelemetry installs the MeterProvider (when OTel is enabled) and the
// always-on TracerProvider, and returns a combined shutdown (nil when nothing
// was installed).
//
// Telemetry is best-effort by construction — it CANNOT fail the caller's startup
// and it CANNOT touch the caller's context (issue #662):
//
//   - Provider construction runs on a detached, bounded context, so a wedged
//     collector or resource detector can neither consume the caller's startup
//     budget (the binary's health probes do not answer until initialization
//     finishes, so spending it means a kubelet liveness kill and a crash loop)
//     nor observe a cancellation of the caller's context.
//   - A setup error degrades the component to no telemetry with a WARN instead
//     of returning an error that exits the process. An agent that programs Envoy
//     but cannot export metrics is degraded, not dead; making an observability
//     backend a hard dependency of the data plane also makes the failure
//     fleet-correlated, since every instance exports to the same collector.
//   - Each shutdown flushes on a detached, bounded context, so the final flush
//     still runs after SIGTERM has cancelled the caller's context.
func setupTelemetry(ctx context.Context, cfg Config, serviceName, serviceVersion string, l *slog.Logger) func(context.Context) error {
	// Degrading gracefully is this function's whole job, so it must not itself
	// nil-panic on the very path that reports the degradation.
	if l == nil {
		l = slog.Default()
	}

	telemetryCfg := setup.Config{
		ServiceName:     serviceName,
		ServiceVersion:  serviceVersion,
		OTLPEndpoint:    cfg.OTLPEndpoint,
		TraceSampleRate: cfg.TraceSampleRate,
		TraceExport:     cfg.TracingExport,
	}

	setupCtx, cancel := setup.DetachedTimeout(ctx, setup.SetupTimeout)
	defer cancel()

	var shutdowns []func(context.Context) error
	if cfg.OTelEnabled {
		metricsShutdown, err := setup.Setup(setupCtx, telemetryCfg)
		if err != nil {
			l.WarnContext(ctx, "failed to set up OTel metrics; continuing without them", "error", err)
		} else {
			shutdowns = append(shutdowns, setup.BestEffortShutdown(metricsShutdown))
		}
	}
	// Tracing is always installed: a TracerProvider issues the span contexts that
	// give logs their trace_id/span_id (the slog/otelslog correlation), which is
	// useful regardless of other telemetry. Span EXPORT stays opt-in (TraceExport)
	// — see SetupTracing.
	tracingShutdown, err := setup.SetupTracing(setupCtx, telemetryCfg)
	if err != nil {
		l.WarnContext(ctx, "failed to set up OTel tracing; continuing without it", "error", err)
	} else {
		shutdowns = append(shutdowns, setup.BestEffortShutdown(tracingShutdown))
	}

	if len(shutdowns) == 0 {
		return nil
	}
	return func(ctx context.Context) error {
		var errs []error
		for _, fn := range shutdowns {
			errs = append(errs, fn(ctx))
		}
		return errors.Join(errs...)
	}
}
