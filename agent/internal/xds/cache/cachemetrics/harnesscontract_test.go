package cachemetrics

import (
	"context"
	"testing"

	"aethermesh.dev/test/harnesscontract"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestExternalHarnessContract holds the pin instruments to the external-harness
// contract (test/harnesscontract/external-harness.yaml): a harness outside
// this repository queries them by the names, labels and label values listed
// there, and branches on the closed `reason` set. What is compared is what the
// real instruments emit after every series has been recorded once, so a
// renamed instrument, a renamed attribute and a cause added to or removed from
// UnpinnedCauses all fail here.
func TestExternalHarnessContract(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	c.Owns(t, "//agent/internal/xds/cache/cachemetrics:cachemetrics_test",
		"agent.snapshot_tls_clusters", "agent.xds_acked_tls_clusters", "agent.xds_acked_tls_clusters_unknown",
		"agent.identity_cluster_unpinned")

	m, reader := newTestMetrics(t)
	// Every series of the two gauges: recordPins writes them all, zeros
	// included. The counter's are seeded at registration.
	m.TLSClusterPins(context.Background(), PinCounts{})
	m.TLSClusterPinsAcked(context.Background(), PinCounts{})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, id := range []string{
		"agent.snapshot_tls_clusters", "agent.xds_acked_tls_clusters", "agent.xds_acked_tls_clusters_unknown",
		"agent.identity_cluster_unpinned",
	} {
		entry := c.Metric(t, id)
		kind, series := collected(rm, entry.OTelName)
		entry.CheckMetric(t, kind, series)
	}
}

// collected returns the contract type and the series of the instrument
// registered as name, or "" when there is none.
func collected(rm metricdata.ResourceMetrics, name string) (string, []harnesscontract.Series) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var kind string
			var points []metricdata.DataPoint[int64]
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				kind, points = "sum", data.DataPoints
				if data.IsMonotonic {
					kind = harnesscontract.TypeCounter
				}
			case metricdata.Gauge[int64]:
				kind, points = harnesscontract.TypeGauge, data.DataPoints
			default:
				return "another type", nil
			}
			series := make([]harnesscontract.Series, 0, len(points))
			for _, dp := range points {
				s := harnesscontract.Series{}
				for _, kv := range dp.Attributes.ToSlice() {
					s[string(kv.Key)] = kv.Value.Emit()
				}
				series = append(series, s)
			}
			return kind, series
		}
	}
	return "", nil
}
