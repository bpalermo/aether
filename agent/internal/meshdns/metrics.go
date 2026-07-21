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
		metric.WithDescription("Mesh-DNS queries handled by the in-agent resolver, by result (answered=mesh hit, forwarded=relayed to upstream, forward_error=upstream failed, nxdomain=authoritative mesh miss, cold=SERVFAIL for a mesh name before records were ever populated)"),
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
	// resultNXDomain is an authoritative miss: a name under the mesh domain that
	// is not in the record table, answered NXDOMAIN once records are populated.
	resultNXDomain = "nxdomain"
	// resultCold is a mesh-domain query received before the record table was ever
	// populated (warm-start snapshot empty + no reconcile yet); answered SERVFAIL
	// so the client retries instead of caching a negative answer.
	resultCold = "cold"
)
