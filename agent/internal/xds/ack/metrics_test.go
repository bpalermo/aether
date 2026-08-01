package ack

import (
	"context"
	"log/slog"
	"testing"
	"time"

	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestTracker builds a Tracker whose instruments record into a manual
// reader, bypassing the global MeterProvider.
func newTestTracker(t *testing.T) (*Tracker, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := newTrackerMetrics(provider.Meter("test"))
	require.NoError(t, err)

	tr := NewTracker(slog.New(slog.DiscardHandler))
	tr.metrics = metrics
	return tr, reader
}

// counterValue returns the summed value of the named counter, and whether it
// was found, optionally requiring an attribute value to be present on the
// data point.
func counterValue(t *testing.T, reader *sdkmetric.ManualReader, name, attrKey, attrVal string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %s is not an int64 sum", name)
			var total int64
			found := false
			for _, dp := range sum.DataPoints {
				if attrKey != "" {
					if v, has := dp.Attributes.Value(attribute.Key(attrKey)); !has || v.AsString() != attrVal {
						continue
					}
				}
				total += dp.Value
				found = true
			}
			return total, found
		}
	}
	return 0, false
}

func TestMetrics_Nack(t *testing.T) {
	tr, reader := newTestTracker(t)
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	ackDelta(tr, 1, "n1", "bad config")

	v, ok := counterValue(t, reader, "aether.agent.xds.nacks", "aether.xds.type_url", resourcev3.ListenerType)
	require.True(t, ok, "nack counter not recorded")
	assert.Equal(t, int64(1), v)
}

func TestMetrics_WaitFailures(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		tr, reader := newTestTracker(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		require.Error(t, tr.WaitListenerPresent(ctx, testListener))

		v, ok := counterValue(t, reader, "aether.agent.xds.ack_wait_failures", "aether.xds.reason", "timeout")
		require.True(t, ok, "wait failure counter not recorded")
		assert.Equal(t, int64(1), v)
	})

	t.Run("nack", func(t *testing.T) {
		tr, reader := newTestTracker(t)
		sendDelta(tr, 1, "n1", []string{testListener}, nil)
		ackDelta(tr, 1, "n1", "cannot bind")

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.Error(t, tr.WaitListenerPresent(ctx, testListener))

		v, ok := counterValue(t, reader, "aether.agent.xds.ack_wait_failures", "aether.xds.reason", "nack")
		require.True(t, ok, "wait failure counter not recorded")
		assert.Equal(t, int64(1), v)
	})
}

func TestMetrics_NilSafe(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	tr.metrics = nil

	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	ackDelta(tr, 1, "n1", "bad config")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.Error(t, tr.WaitListenerAbsent(ctx, testListener))
}
