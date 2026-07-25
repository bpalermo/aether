package meshdns

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName scopes every mesh-DNS instrument.
const meterName = "aether/mesh-dns"

// queryDurationBuckets are the explicit histogram boundaries (seconds) for
// aether.mesh_dns.query.duration. A mesh hit is answered from an in-memory map in
// microseconds while a forwarded query costs a kube-dns round trip, so the default
// OTel boundaries (tuned for milliseconds-as-units) would collapse the whole mesh
// path into the first bucket.
var queryDurationBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// resolverState is the point-in-time Server state the observable gauges export. It is
// a plain value copied out from under the Server's RWMutex (see Server.observedState)
// so the OTel collect callback never holds a resolver lock while exporting.
type resolverState struct {
	records     int64
	writtenAt   int64
	generation  uint64
	ready       bool
	watchActive bool
	upstreams   int64
}

// metrics holds the resolver's OTel instruments. nil-safe: every record method is a
// no-op when the meter failed to initialize.
type metrics struct {
	queries  metric.Int64Counter
	duration metric.Float64Histogram
	reloads  metric.Int64Counter
}

// newMetrics builds the resolver metrics on the global MeterProvider (a no-op meter
// when OTel is disabled, so this never errors fatally) and registers the callback that
// exports the observable gauges from state. On any instrument failure it logs a WARN
// and returns nil — metrics are then off, but the resolver keeps serving DNS.
func newMetrics(state func() resolverState, log *slog.Logger) *metrics {
	b := &meterBuilder{meter: otel.Meter(meterName)}

	queries := b.counter("aether.mesh_dns.queries",
		"Mesh-DNS queries handled by the resolver, by result (answered=mesh hit, forwarded=relayed to upstream, forward_error=upstream failed, nxdomain=authoritative mesh miss, cold=SERVFAIL for a mesh name before records were ever populated)")
	reloads := b.counter("aether.mesh_dns.snapshot_reloads_total",
		"Attempts to read the record snapshot the agent writes, by result (success, missing=file absent, parse_error=corrupt, read_error=I/O failure)")
	duration := b.histogram("aether.mesh_dns.query.duration",
		"End-to-end time to handle a DNS query, by result")

	age := b.floatGauge("aether.mesh_dns.snapshot_age_seconds",
		"Seconds since the agent stamped the record snapshot. Grows without bound when the writing agent crashes, loses RBAC, or its capture reconciler wedges — the resolver then serves a frozen table and NXDOMAINs new services authoritatively")
	writtenAt := b.floatGauge("aether.mesh_dns.snapshot_written_at_seconds",
		"Unix timestamp the agent stamped on the record snapshot currently served")
	records := b.intGauge("aether.mesh_dns.records",
		"Mesh service records currently served. Zero means every mesh name NXDOMAINs")
	generation := b.intGauge("aether.mesh_dns.snapshot_generation",
		"Record-table version of the snapshot currently served; advances only when the record content changes, not on a freshness heartbeat")
	watchActive := b.intGauge("aether.mesh_dns.watch_active",
		"1 when the fsnotify snapshot watcher is running, 0 when it never started or died (records then freeze until restart)")
	upstreams := b.intGauge("aether.mesh_dns.upstreams_configured",
		"Number of forward upstreams. Zero means every non-mesh query (cluster.local and external) fails")
	ready := b.intGauge("aether.mesh_dns.ready",
		"1 once records have ever been populated (mesh misses answer NXDOMAIN), 0 while cold (mesh misses answer SERVFAIL)")

	if b.err != nil {
		log.Warn("mesh-DNS metrics disabled: failed to create instruments", "error", b.err)
		return nil
	}

	observe := func(_ context.Context, o metric.Observer) error {
		st := state()
		if st.writtenAt > 0 {
			o.ObserveFloat64(age, time.Since(time.Unix(st.writtenAt, 0)).Seconds())
			o.ObserveFloat64(writtenAt, float64(st.writtenAt))
		}
		o.ObserveInt64(records, st.records)
		o.ObserveInt64(generation, int64(st.generation))
		o.ObserveInt64(watchActive, boolGauge(st.watchActive))
		o.ObserveInt64(upstreams, st.upstreams)
		o.ObserveInt64(ready, boolGauge(st.ready))
		return nil
	}
	if _, err := b.meter.RegisterCallback(observe, age, writtenAt, records, generation, watchActive, upstreams, ready); err != nil {
		log.Warn("mesh-DNS metrics disabled: failed to register the gauge callback", "error", err)
		return nil
	}

	return &metrics{queries: queries, duration: duration, reloads: reloads}
}

// meterBuilder creates instruments while latching the first error, so newMetrics
// reads as a flat declaration list instead of a ladder of error checks.
type meterBuilder struct {
	meter metric.Meter
	err   error
}

func (b *meterBuilder) counter(name, desc string) metric.Int64Counter {
	i, err := b.meter.Int64Counter(name, metric.WithDescription(desc))
	b.latch(err)
	return i
}

func (b *meterBuilder) histogram(name, desc string) metric.Float64Histogram {
	i, err := b.meter.Float64Histogram(name,
		metric.WithDescription(desc),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(queryDurationBuckets...))
	b.latch(err)
	return i
}

func (b *meterBuilder) floatGauge(name, desc string) metric.Float64ObservableGauge {
	i, err := b.meter.Float64ObservableGauge(name, metric.WithDescription(desc))
	b.latch(err)
	return i
}

func (b *meterBuilder) intGauge(name, desc string) metric.Int64ObservableGauge {
	i, err := b.meter.Int64ObservableGauge(name, metric.WithDescription(desc))
	b.latch(err)
	return i
}

// latch keeps the FIRST error so a later success cannot mask an earlier failure.
func (b *meterBuilder) latch(err error) {
	if b.err == nil {
		b.err = err
	}
}

// boolGauge renders a boolean state as the 0/1 an observable gauge exports.
func boolGauge(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// observeQuery counts a handled query and records how long handling took.
func (m *metrics) observeQuery(result string, d time.Duration) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("result", result))
	m.queries.Add(context.Background(), 1, attrs)
	m.duration.Record(context.Background(), d.Seconds(), attrs)
}

// recordReload counts a snapshot read attempt by outcome.
func (m *metrics) recordReload(result string) {
	if m == nil {
		return
	}
	m.reloads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
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

const (
	// reloadSuccess is a snapshot that was read and decoded.
	reloadSuccess = "success"
	// reloadMissing is a snapshot file that does not exist (a cold node).
	reloadMissing = "missing"
	// reloadParseError is a snapshot that exists but could not be decoded.
	reloadParseError = "parse_error"
	// reloadReadError is any other I/O failure reading the snapshot.
	reloadReadError = "read_error"
)
