package meshdns

import (
	"context"
	"log/slog"
	"time"

	"github.com/miekg/dns"
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
	// lastAnswered is the unix second of the last query the resolver actually answered
	// (real traffic or a self-check); zero until it has answered anything at all.
	lastAnswered int64
}

// metrics holds the resolver's OTel instruments. nil-safe: every record method is a
// no-op when the meter failed to initialize.
type metrics struct {
	queries    metric.Int64Counter
	duration   metric.Float64Histogram
	reloads    metric.Int64Counter
	selfChecks metric.Int64Counter
	truncated  metric.Int64Counter
}

// newMetrics builds the resolver metrics on the global MeterProvider (a no-op meter
// when OTel is disabled, so this never errors fatally) and registers the callback that
// exports the observable gauges from state. On any instrument failure it logs a WARN
// and returns nil — metrics are then off, but the resolver keeps serving DNS.
func newMetrics(state func() resolverState, log *slog.Logger) *metrics {
	b := &meterBuilder{meter: otel.Meter(meterName)}

	queries := b.counter("aether.mesh_dns.queries",
		"Mesh-DNS queries handled by the resolver, by result (answered=mesh hit with an A record, nodata=known mesh name asked for a non-A type, forwarded=relayed to upstream, forward_error=upstream failed, nxdomain=authoritative mesh miss, cold=SERVFAIL for a mesh name before records were ever populated), by the transport the query arrived on (proto=udp|tcp; the CNI DNATs both), and by bucketed query type (qtype=a|aaaa|https|srv|ptr|txt|cname|other)")
	selfChecks := b.counter("aether.mesh_dns.self_check_total",
		"In-process handler self-checks run by the wedge watchdog, by result (ok, fail). Consecutive failures remove the pod-local ready marker so the readiness probe flips the pod NotReady")
	truncated := b.counter("aether.mesh_dns.responses_truncated_total",
		"Upstream replies that came back truncated (TC=1) over UDP and were re-fetched over TCP")
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
	lastAnswered := b.floatGauge("aether.mesh_dns.last_answered_timestamp_seconds",
		"Unix timestamp of the last query the resolver actually answered — real traffic or the self-check watchdog. It stops advancing on a resolver that is bound but blind, which the ready marker alone cannot show")

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
		if st.lastAnswered > 0 {
			o.ObserveFloat64(lastAnswered, float64(st.lastAnswered))
		}
		return nil
	}
	if _, err := b.meter.RegisterCallback(observe, age, writtenAt, records, generation, watchActive, upstreams, ready, lastAnswered); err != nil {
		log.Warn("mesh-DNS metrics disabled: failed to register the gauge callback", "error", err)
		return nil
	}

	return &metrics{
		queries:    queries,
		duration:   duration,
		reloads:    reloads,
		selfChecks: selfChecks,
		truncated:  truncated,
	}
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

// observeQuery counts a handled query (by result, by the transport it arrived on AND by
// bucketed query type) and records how long handling took.
//
// The duration histogram deliberately keeps the result dimension ONLY. Its 15 explicit
// buckets make every extra attribute expensive — proto would double the series count and
// qtype would multiply it by eight — and neither buys anything: latency here is decided
// by the PATH taken, which result already names. A mesh hit and an AAAA NODATA do the
// same map lookup under the same lock, and a forwarded SRV costs the same kube-dns round
// trip as a forwarded A. qtype and proto answer "what is being asked, over what", which
// is a counter question; result answers "how long did it take", which is the histogram's.
func (m *metrics) observeQuery(result, proto, qtype string, d time.Duration) {
	if m == nil {
		return
	}
	m.queries.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("result", result),
		attribute.String("proto", proto),
		attribute.String("qtype", qtype),
	))
	m.duration.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String("result", result),
	))
}

// recordSelfCheck counts one watchdog self-check by outcome (ok / fail).
func (m *metrics) recordSelfCheck(result string) {
	if m == nil {
		return
	}
	m.selfChecks.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
}

// recordTruncated counts an upstream reply that came back truncated over UDP and had
// to be re-fetched over TCP.
func (m *metrics) recordTruncated() {
	if m == nil {
		return
	}
	m.truncated.Add(context.Background(), 1)
}

// recordReload counts a snapshot read attempt by outcome.
func (m *metrics) recordReload(result string) {
	if m == nil {
		return
	}
	m.reloads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
}

const (
	// resultAnswered is a mesh hit that produced a real A record.
	resultAnswered = "answered"
	// resultNoData is a KNOWN mesh name asked for a type the resolver has no record
	// for (AAAA and friends): NOERROR with an empty answer section, so the name
	// consistently exists — see serveMesh. It is split out from resultAnswered
	// because the two are operationally different: a healthy mesh shows a steady
	// nodata trickle from the AAAA half of every dual-stack lookup, whereas nodata
	// RISING against answered means clients are failing over to IPv6 or the record
	// table has stopped producing usable A records. Conflated, that is invisible.
	resultNoData       = "nodata"
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
	// protoUDP / protoTCP are the transports a query can arrive on — the CNI DNATs
	// pod :53 for both — and double as the miekg/dns Client.Net values the forward
	// path relays over.
	protoUDP = "udp"
	protoTCP = "tcp"
)

// qtype attribute values. This set is CLOSED, and that is the point: DNS defines ~90
// query types, the CNI DNATs every managed pod's :53 straight here, and a buggy or
// hostile client can therefore ask this resolver for arbitrary types. Emitting the raw
// qtype would let any pod on the node inflate the counter's series count at will, so
// queryQType maps to these buckets and EVERYTHING unrecognised collapses to qtypeOther.
//
// The named buckets are what a mesh resolver realistically sees and would act on:
//
//   - a: the only type the mesh record table can answer — the hit path itself.
//   - aaaa: every dual-stack-capable resolver (glibc getaddrinfo, c-ares, Go) fires it
//     alongside A, so it is essentially the whole nodata trickle. Watching it rise
//     against a is the IPv6-failover signal the nodata split exists for.
//   - https: curl 8.x, browsers and systemd-resolved query the HTTPS RR (type 65) next
//     to A+AAAA. It is high volume and utterly routine, so leaving it in other would
//     bury the genuinely exotic queries other is meant to expose.
//   - srv: headless Services and gRPC/SIP-style discovery. Always forwarded (kube-dns
//     owns _svc._proto), so it is the forward path's main non-address type.
//   - ptr: reverse lookups from tooling and logging stacks; forwarded, and a burst of
//     them is a classic accidental source of upstream latency.
//   - txt: forwarded, normally near zero, and both a health-probe and an exfil channel —
//     cheap to keep separable.
//   - cname: what debugging tools (dig CNAME) and some client libraries ask directly,
//     which distinguishes an operator/tool pattern from real workload traffic.
//
// Worst case is therefore 8 qtype x 6 result x 2 proto = 96 series per node, bounded no
// matter what is asked; in practice only a handful are ever non-zero.
const (
	qtypeA     = "a"
	qtypeAAAA  = "aaaa"
	qtypeHTTPS = "https"
	qtypeSRV   = "srv"
	qtypePTR   = "ptr"
	qtypeTXT   = "txt"
	qtypeCNAME = "cname"
	qtypeOther = "other"
)

// qtypeBuckets is the recognised-type lookup behind queryQType. A miss is qtypeOther,
// which is what keeps the attribute's value set closed and its cardinality bounded.
var qtypeBuckets = map[uint16]string{
	dns.TypeA:     qtypeA,
	dns.TypeAAAA:  qtypeAAAA,
	dns.TypeHTTPS: qtypeHTTPS,
	dns.TypeSRV:   qtypeSRV,
	dns.TypePTR:   qtypePTR,
	dns.TypeTXT:   qtypeTXT,
	dns.TypeCNAME: qtypeCNAME,
}

// queryQType buckets the query's type for the qtype attribute (see qtypeBuckets). A
// message without exactly one question is reported as qtypeOther: it is never a mesh
// query (serve requires a single question) and has no single meaningful type.
func queryQType(r *dns.Msg) string {
	if r == nil || len(r.Question) != 1 {
		return qtypeOther
	}
	if bucket, ok := qtypeBuckets[r.Question[0].Qtype]; ok {
		return bucket
	}
	return qtypeOther
}

const (
	// selfCheckOK is a watchdog check whose synthetic query got a sane reply.
	selfCheckOK = "ok"
	// selfCheckFail is a watchdog check that timed out or got an unusable reply —
	// the resolver is bound but not answering.
	selfCheckFail = "fail"
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
