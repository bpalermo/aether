package server

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/durationpb"
)

// pluginOperationCounts returns aether.cni.operations by "<operation>/<result>".
func pluginOperationCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aether.cni.operations" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				op, _ := dp.Attributes.Value(attrCNIOperation)
				result, _ := dp.Attributes.Value(attrCNIResult)
				counts[op.AsString()+"/"+result.AsString()] += dp.Value
			}
		}
	}
	return counts
}

// spanCtx starts a recording span, standing in for the otelgrpc RPC span.
func spanCtx(t *testing.T) (context.Context, func() []attribute.KeyValue) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(context.Background(), "rpc")
	return ctx, func() []attribute.KeyValue {
		span.End()
		ended := rec.Ended()
		require.Len(t, ended, 1)
		return ended[0].Attributes()
	}
}

func attrMap(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

// #1166: the CNI plugin no longer exports telemetry, so the capture-divert
// outcome it reports is counted here under the name and attributes the plugin
// used to export. A failure is the soak gate's "pod running UNCAPTURED" and must
// also reach the agent's log at WARN.
func TestReportAddResult_CountsCaptureDivert(t *testing.T) {
	m, reader := newTestCNIMetrics(t)
	var logBuf bytes.Buffer
	s := &CNIServer{log: slog.New(slog.NewTextHandler(&logBuf, nil)), metrics: m}

	ctx, attrs := spanCtx(t)
	_, err := s.ReportAddResult(ctx, &cniv1.ReportAddResultRequest{
		Name: "p", Namespace: "ns", ContainerId: "c1",
		CaptureDivert:      durationpb.New(3 * time.Millisecond),
		CaptureDivertError: "nft_tproxy missing",
		ReadinessProbe:     durationpb.New(40 * time.Millisecond),
		Total:              durationpb.New(90 * time.Millisecond),
	})
	require.NoError(t, err)
	_, err = s.ReportAddResult(context.Background(), &cniv1.ReportAddResultRequest{
		Name: "q", Namespace: "ns", ContainerId: "c2",
		CaptureDivert: durationpb.New(2 * time.Millisecond),
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{"capture_divert/error": 1, "capture_divert/success": 1}, pluginOperationCounts(t, reader))
	assert.Contains(t, logBuf.String(), "level=WARN")
	assert.Contains(t, logBuf.String(), "POD IS RUNNING UNCAPTURED")

	got := attrMap(attrs())
	assert.InDelta(t, 0.003, got[attrPluginCaptureDivert].AsFloat64(), 1e-9)
	assert.Equal(t, "nft_tproxy missing", got[attrPluginCaptureError].AsString())
	assert.InDelta(t, 0.04, got[attrPluginReadinessProbe].AsFloat64(), 1e-9)
	assert.InDelta(t, 0.09, got[attrPluginTotal].AsFloat64(), 1e-9)
}

// A report without a capture-divert duration (nothing was attempted) counts
// nothing, so the error series can only ever mean a real failure.
func TestReportAddResult_NoDivertCountsNothing(t *testing.T) {
	m, reader := newTestCNIMetrics(t)
	s := &CNIServer{log: slog.New(slog.DiscardHandler), metrics: m}

	_, err := s.ReportAddResult(context.Background(), &cniv1.ReportAddResultRequest{Name: "p", Namespace: "ns", ContainerId: "c1"})
	require.NoError(t, err)
	assert.Empty(t, pluginOperationCounts(t, reader))
}

// The forwarded pre-call duration lands on the RPC span; a plugin that
// predates the field sends none and nothing is recorded.
func TestRecordPluginTimings(t *testing.T) {
	ctx, attrs := spanCtx(t)
	recordPluginTimings(ctx, &cniv1.PluginTimings{PreCall: durationpb.New(12 * time.Millisecond)})
	assert.InDelta(t, 0.012, attrMap(attrs())[attrPluginPreCall].AsFloat64(), 1e-9)

	ctx, attrs = spanCtx(t)
	recordPluginTimings(ctx, nil)
	_, ok := attrMap(attrs())[attrPluginPreCall]
	assert.False(t, ok)
}

func TestCNIMetrics_PluginOperationNilReceiverSafe(t *testing.T) {
	var m *cniMetrics
	m.pluginOperation(context.Background(), opCaptureDivert, "boom")
}
