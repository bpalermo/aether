package meshdns

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// metrics holds the resolver's OTel instruments. nil-safe: record is a no-op when the
// meter failed to initialize.
type metrics struct {
	queries metric.Int64Counter
}

// newMetrics builds the resolver metrics on the global MeterProvider (a no-op meter
// when OTel is disabled, so this never errors fatally).
func newMetrics() *metrics {
	q, err := otel.Meter("aether/mesh-dns").Int64Counter(
		"aether.mesh_dns.queries",
		metric.WithDescription("Mesh-DNS queries handled by the in-agent resolver, by result (answered=mesh hit, forwarded=relayed to upstream, forward_error=upstream failed)"),
	)
	if err != nil {
		return nil
	}
	return &metrics{queries: q}
}

func (m *metrics) record(result string) {
	if m == nil {
		return
	}
	m.queries.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
}

const (
	resultAnswered     = "answered"
	resultForwarded    = "forwarded"
	resultForwardError = "forward_error"
)
