package configimport

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// metrics holds the config-import OTel instruments (proposal 026 EM4: make
// cross-cluster propagation lag + drift observable). All methods are nil-receiver-safe
// so the importer runs unchanged when telemetry is disabled.
type metrics struct {
	services metric.Int64Gauge
	ageMax   metric.Float64Gauge
	errors   metric.Int64Counter
}

// newMetrics builds the instruments on the global MeterProvider (a no-op meter until a
// provider is installed). Returns nil on registration error (the importer stays
// functional; methods are nil-safe).
func newMetrics() *metrics {
	meter := otel.Meter("aether/config-import")
	m := &metrics{}
	var err error
	if m.services, err = meter.Int64Gauge("aether.config.import.services",
		metric.WithDescription("Cross-cluster config projections currently materialized into the proxy (proposal 026)")); err != nil {
		return nil
	}
	if m.ageMax, err = meter.Float64Gauge("aether.config.import.age_max",
		metric.WithUnit("s"),
		metric.WithDescription("Staleness of the OLDEST imported projection = now - its export timestamp; the cross-cluster propagation-lag/skew signal")); err != nil {
		return nil
	}
	if m.errors, err = meter.Int64Counter("aether.config.import.errors",
		metric.WithDescription("Config-import fetch failures (last-known imported routes retained; AP)")); err != nil {
		return nil
	}
	return m
}

// observe records the imported-service count and the oldest-projection age after a
// successful poll. A negative ageMax means no projection carried a parseable timestamp.
func (m *metrics) observe(ctx context.Context, services int, ageMaxSeconds float64) {
	if m == nil {
		return
	}
	m.services.Record(ctx, int64(services))
	if ageMaxSeconds >= 0 {
		m.ageMax.Record(ctx, ageMaxSeconds)
	}
}

func (m *metrics) fetchError(ctx context.Context) {
	if m == nil {
		return
	}
	m.errors.Add(ctx, 1)
}
