package manager

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupLogging creates a logger with the given name and configures controller-runtime to use it.
func SetupLogging(debug bool, name string) logr.Logger {
	l := log.NewLogger(debug).WithName(name)
	ctrl.SetLogger(l)
	return l
}

// SetupManagerLogging is SetupLogging for manager-based components (agent,
// registrar). When cfg.LogsEnabled and an OTLP endpoint are set, it also creates an
// OTel LoggerProvider and tees it into the zap logger via an otelzap Core, so
// records go to both stderr (kubectl logs, unchanged) and OTLP → collector. The
// returned shutdown flushes and stops the LoggerProvider; it is nil when OTLP
// logging is disabled. name/version supply the logger name and OTel resource
// service.name/service.version.
func SetupManagerLogging(ctx context.Context, cfg Config, name, version string) (logr.Logger, func(context.Context) error, error) {
	if !cfg.LogsEnabled || cfg.OTLPEndpoint == "" {
		return SetupLogging(cfg.Debug, name), nil, nil
	}

	provider, shutdown, err := telemetry.SetupLogs(ctx, telemetry.Config{
		ServiceName:    name,
		ServiceVersion: version,
		OTLPEndpoint:   cfg.OTLPEndpoint,
	})
	if err != nil {
		return logr.Logger{}, nil, fmt.Errorf("failed to setup OTel logs: %w", err)
	}

	core := otelzap.NewCore(name, otelzap.WithLoggerProvider(provider))
	l := log.NewLoggerWithCore(cfg.Debug, core).WithName(name)
	ctrl.SetLogger(l)
	return l, shutdown, nil
}
