package manager

import (
	"context"
	"log/slog"
	"os"
)

// FlushLogs flushes and stops the OTLP log exporter, i.e. runs the shutdown
// returned by SetupManagerLogging. No-op when shutdown is nil (OTLP logging is
// disabled). A failure is logged on l, never returned.
func FlushLogs(ctx context.Context, l *slog.Logger, shutdown func(context.Context) error) {
	if shutdown == nil {
		return
	}
	if err := shutdown(ctx); err != nil {
		l.ErrorContext(ctx, "failed to flush OTel logs", "error", err)
	}
}

// ShutdownTelemetry runs the telemetry shutdown returned by Bootstrap
// (Result.Shutdown). No-op when shutdown is nil. A failure is logged on l, never
// returned.
func ShutdownTelemetry(ctx context.Context, l *slog.Logger, shutdown func(context.Context) error) {
	if shutdown == nil {
		return
	}
	if err := shutdown(ctx); err != nil {
		l.ErrorContext(ctx, "failed to shutdown telemetry", "error", err)
	}
}

// CurrentNamespace returns the namespace this pod runs in. It reads
// POD_NAMESPACE (set via the downward API by the chart), falls back to the
// service-account namespace file, and finally to "default".
func CurrentNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(data)
	}
	return "default"
}
