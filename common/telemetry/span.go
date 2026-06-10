package telemetry

import (
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// EndSpan records the operation outcome on the span and ends it. Designed for
// use in a defer alongside a named error return:
//
//	ctx, span := otel.Tracer(name).Start(ctx, "op")
//	defer func() { telemetry.EndSpan(span, retErr) }()
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
