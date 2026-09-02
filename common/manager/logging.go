package manager

import (
	"context"
	"log/slog"

	"aethermesh.dev/common/log"
	"aethermesh.dev/common/telemetry/setup"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupLogging creates a stderr slog logger named name and points
// controller-runtime at it (via the logr→slog bridge).
func SetupLogging(debug bool, name string) *slog.Logger {
	l := log.Named(log.NewLogger(debug), name)
	ctrl.SetLogger(logr.FromSlogHandler(l.Handler()))
	return l
}

// SetupManagerLogging is SetupLogging for manager-based components (agent,
// registrar). When cfg.LogsEnabled and an OTLP endpoint are set, it also creates
// an OTel LoggerProvider and fans the logger out to the otelslog bridge, so
// records go to both stderr (kubectl logs) and OTLP → collector. Because slog's
// Handle is context-aware, otelslog populates trace_id/span_id natively from the
// ctx passed to the *Context log methods. controller-runtime is wired via the
// logr→slog bridge over the same handler. The returned shutdown flushes and stops
// the LoggerProvider; it is nil when OTLP logging is disabled.
//
// It returns no error by design (issue #662): OTLP log export is best-effort, so
// a collector that is missing, unreachable or shedding degrades the component to
// stderr-only logging with a WARN rather than killing the process. Setup runs on
// a detached, bounded context so it can neither spend the caller's startup budget
// nor observe a cancellation of the caller's context, and the returned shutdown
// flushes on its own detached context so the final flush still runs after SIGTERM.
func SetupManagerLogging(ctx context.Context, cfg Config, name, version string) (*slog.Logger, func(context.Context) error) {
	if !cfg.LogsEnabled || cfg.OTLPEndpoint == "" {
		return SetupLogging(cfg.Debug, name), nil
	}

	setupCtx, cancel := setup.DetachedTimeout(ctx, setup.SetupTimeout)
	defer cancel()

	provider, shutdown, err := setup.SetupLogs(setupCtx, setup.Config{
		ServiceName:    name,
		ServiceVersion: version,
		OTLPEndpoint:   cfg.OTLPEndpoint,
	})
	if err != nil {
		l := SetupLogging(cfg.Debug, name)
		l.WarnContext(ctx, "failed to set up OTel log export; logging to stderr only", "error", err)
		return l, nil
	}

	otelHandler := otelslog.NewHandler(name, otelslog.WithLoggerProvider(provider))
	l := log.Named(log.NewLoggerWithHandler(cfg.Debug, otelHandler), name)
	ctrl.SetLogger(logr.FromSlogHandler(l.Handler()))
	return l, setup.BestEffortShutdown(shutdown)
}
