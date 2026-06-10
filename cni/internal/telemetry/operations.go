package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Operation attribute keys; values come from small fixed sets (the five CNI
// verbs and success/error).
const (
	attrOperation = attribute.Key("aether.cni.operation")
	attrResult    = attribute.Key("aether.cni.result")
)

// RecordOperation records the outcome and duration of one CNI operation
// (add/del/check/gc/status). A no-op unless Init enabled telemetry. The
// instruments are created per call: the plugin process handles exactly one
// operation before exiting, so there is nothing to cache.
func RecordOperation(op string, duration time.Duration, opErr error) {
	if meterProvider == nil {
		return
	}
	meter := meterProvider.Meter("aether/cni-plugin")

	result := "success"
	if opErr != nil {
		result = "error"
	}
	attrs := metric.WithAttributes(attrOperation.String(op), attrResult.String(result))

	ctx := context.Background()
	if counter, err := meter.Int64Counter("aether.cni.operations",
		metric.WithDescription("CNI plugin operations, by verb and result")); err == nil {
		counter.Add(ctx, 1, attrs)
	}
	if hist, err := meter.Float64Histogram("aether.cni.operation.duration",
		metric.WithDescription("Duration of a CNI plugin operation"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)); err == nil {
		hist.Record(ctx, duration.Seconds(), attrs)
	}
}
