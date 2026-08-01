package configexport

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// metrics holds the config-export OTel instruments (proposal 026 EM4). All methods are
// nil-receiver-safe so the controller runs unchanged when telemetry is disabled.
type metrics struct {
	services metric.Int64Gauge
	errors   metric.Int64Counter
}

// newMetrics builds the instruments on the global MeterProvider (a no-op meter until a
// provider is installed). Returns nil on registration error (methods are nil-safe).
func newMetrics() *metrics {
	meter := otel.Meter("aether/config-export")
	m := &metrics{}
	var err error
	if m.services, err = meter.Int64Gauge("aether.config.export.services",
		metric.WithDescription("Services whose GAMMA config this cluster currently exports for peers to import (proposal 026)")); err != nil {
		return nil
	}
	if m.errors, err = meter.Int64Counter("aether.config.export.errors",
		metric.WithDescription("Config export/unexport write failures against the registry")); err != nil {
		return nil
	}
	return m
}

func (m *metrics) observe(ctx context.Context, services int) {
	if m == nil {
		return
	}
	m.services.Record(ctx, int64(services))
}

func (m *metrics) writeError(ctx context.Context) {
	if m == nil {
		return
	}
	m.errors.Add(ctx, 1)
}
