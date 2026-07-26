package meshdns

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// queriesMetric is the counter every attribute assertion below reads.
const queriesMetric = "aether.mesh_dns.queries"

// meteredServer builds a resolver whose instruments are backed by a manual reader, so a
// test can assert on the ATTRIBUTES actually exported rather than on an internal return
// value. The global MeterProvider is swapped for the duration of the test (newMetrics
// reads it at construction time) and restored afterwards.
func meteredServer(t *testing.T, records map[string]string) (*Server, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	if records != nil {
		s.SetRecords(records)
	}
	return s, reader
}

// queryCounts collects the aether.mesh_dns.queries data points as
// "result/proto/qtype" -> value, which is the whole attribute set the counter carries.
func queryCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != queriesMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s should be an int64 sum", queriesMetric)
			for _, dp := range sum.DataPoints {
				counts[attrKey(t, dp.Attributes)] += dp.Value
			}
		}
	}
	return counts
}

// attrKey renders a data point's attributes as "result/proto/qtype".
func attrKey(t *testing.T, set attribute.Set) string {
	t.Helper()
	get := func(k string) string {
		v, ok := set.Value(attribute.Key(k))
		require.True(t, ok, "data point is missing the %q attribute", k)
		return v.Emit()
	}
	return get("result") + "/" + get("proto") + "/" + get("qtype")
}

// TestQueryMetricSplitsAnsweredFromNoData: an A hit is counted answered/a, while an AAAA
// query for the SAME known name — the NODATA reply that keeps the name existing for
// c-ares — is counted nodata/aaaa. Conflated (the pre-split behaviour) an IPv6 failover
// would be indistinguishable from healthy traffic.
func TestQueryMetricSplitsAnsweredFromNoData(t *testing.T) {
	s, reader := meteredServer(t, map[string]string{"default/echo": "10.111.0.6"})

	hit := serve(s, query("echo.default.aether.internal", dns.TypeA))
	require.NotNil(t, hit)
	require.Len(t, hit.Answer, 1, "the A query is a real hit")

	nodata := serve(s, query("echo.default.aether.internal", dns.TypeAAAA))
	require.NotNil(t, nodata)
	require.Equal(t, dns.RcodeSuccess, nodata.Rcode, "NODATA, not NXDOMAIN")
	require.Empty(t, nodata.Answer)

	assert.Equal(t, map[string]int64{
		"answered/udp/a":  1,
		"nodata/udp/aaaa": 1,
	}, queryCounts(t, reader))
}

// TestQueryMetricNoDataOnUnparseableRecord: an A query whose stored record is not a
// usable IPv4 produces an empty answer, and is counted nodata — a record-table defect
// showing up as nodata on qtype=a rather than hiding inside answered.
func TestQueryMetricNoDataOnUnparseableRecord(t *testing.T) {
	s, reader := meteredServer(t, map[string]string{"default/echo": "not-an-ip"})

	resp := serve(s, query("echo.default.aether.internal", dns.TypeA))
	require.NotNil(t, resp)
	require.Empty(t, resp.Answer)

	assert.Equal(t, map[string]int64{"nodata/udp/a": 1}, queryCounts(t, reader))
}

// TestQueryMetricBucketsExoticQType: an unrecognised query type collapses to "other".
// The CNI DNATs every managed pod's :53 here, so an arbitrary client can ask for any of
// DNS's ~90 types; the closed bucket set is what stops that inflating the series count.
func TestQueryMetricBucketsExoticQType(t *testing.T) {
	s, reader := meteredServer(t, map[string]string{"default/echo": "10.111.0.6"})

	for _, qtype := range []uint16{dns.TypeNAPTR, dns.TypeANY, dns.TypeCAA, 61234} {
		require.NotNil(t, serve(s, query("echo.default.aether.internal", qtype)))
	}

	assert.Equal(t, map[string]int64{"nodata/udp/other": 4},
		queryCounts(t, reader), "every exotic type collapses into one series")
}

// TestQueryMetricProtoStillSplits: the proto attribute keeps reporting the transport the
// query arrived on now that qtype sits beside it.
func TestQueryMetricProtoStillSplits(t *testing.T) {
	s, reader := meteredServer(t, map[string]string{"default/echo": "10.111.0.6"})

	require.NotNil(t, serve(s, query("echo.default.aether.internal", dns.TypeA)))
	require.NotNil(t, serveFrom(s, query("echo.default.aether.internal", dns.TypeA),
		&net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5555}))

	assert.Equal(t, map[string]int64{
		"answered/udp/a": 1,
		"answered/tcp/a": 1,
	}, queryCounts(t, reader))
}

// TestQueryMetricMissAndColdKeepQType: the qtype attribute is recorded on every result,
// not just on hits — an AAAA storm against names that do not exist is exactly the
// pattern worth seeing before it becomes a latency complaint.
func TestQueryMetricMissAndColdKeepQType(t *testing.T) {
	s, reader := meteredServer(t, nil)

	require.NotNil(t, serve(s, query("nope.default.aether.internal", dns.TypeAAAA)), "cold: SERVFAIL")
	s.SetRecords(map[string]string{"default/echo": "10.111.0.6"})
	require.NotNil(t, serve(s, query("nope.default.aether.internal", dns.TypeAAAA)), "ready: NXDOMAIN")

	assert.Equal(t, map[string]int64{
		"cold/udp/aaaa":     1,
		"nxdomain/udp/aaaa": 1,
	}, queryCounts(t, reader))
}

// TestQueryQTypeBuckets pins the closed bucket set: every recognised type maps to its own
// value and everything else — including a malformed multi-question message — is "other".
func TestQueryQTypeBuckets(t *testing.T) {
	for qtype, want := range map[uint16]string{
		dns.TypeA:     qtypeA,
		dns.TypeAAAA:  qtypeAAAA,
		dns.TypeHTTPS: qtypeHTTPS,
		dns.TypeSRV:   qtypeSRV,
		dns.TypePTR:   qtypePTR,
		dns.TypeTXT:   qtypeTXT,
		dns.TypeCNAME: qtypeCNAME,
		dns.TypeSOA:   qtypeOther,
		dns.TypeMX:    qtypeOther,
		dns.TypeANY:   qtypeOther,
		65535:         qtypeOther,
	} {
		assert.Equal(t, want, queryQType(query("echo.default.aether.internal", qtype)),
			"qtype %d", qtype)
	}

	assert.Equal(t, qtypeOther, queryQType(nil), "a nil message is never a mesh query")
	assert.Equal(t, qtypeOther, queryQType(new(dns.Msg)), "no question section")

	multi := query("echo.default.aether.internal", dns.TypeA)
	multi.Question = append(multi.Question, multi.Question[0])
	assert.Equal(t, qtypeOther, queryQType(multi), "more than one question has no single type")
}

// TestObserveQueryNilMetricsIsNoOp: metrics are optional (newMetrics returns nil when the
// instruments fail), and the resolver must keep serving regardless.
func TestObserveQueryNilMetricsIsNoOp(t *testing.T) {
	var m *metrics
	assert.NotPanics(t, func() { m.observeQuery(resultNoData, protoUDP, qtypeAAAA, 0) })
}
