package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/telemetry/setup"
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
func SetupManagerLogging(ctx context.Context, cfg Config, name, version string) (*slog.Logger, func(context.Context) error, error) {
	if !cfg.LogsEnabled || cfg.OTLPEndpoint == "" {
		return SetupLogging(cfg.Debug, name), nil, nil
	}

	provider, shutdown, err := setup.SetupLogs(ctx, setup.Config{
		ServiceName:    name,
		ServiceVersion: version,
		OTLPEndpoint:   cfg.OTLPEndpoint,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to setup OTel logs: %w", err)
	}

	otelHandler := otelslog.NewHandler(name, otelslog.WithLoggerProvider(provider))
	l := log.Named(log.NewLoggerWithHandler(cfg.Debug, otelHandler), name)
	ctrl.SetLogger(logr.FromSlogHandler(l.Handler()))
	return l, shutdown, nil
}
