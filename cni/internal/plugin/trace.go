package plugin

import (
	"context"

	"github.com/bpalermo/aether/common/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies this instrumentation scope in trace backends.
const tracerName = "aether/cni-plugin"

// startPodSpan starts a root span for a CNI pod operation. The span is a no-op
// unless tracing was enabled via telemetry.Setup; the returned context carries
// it into the gRPC client, where the stats handler propagates it to the agent.
func startPodSpan(ctx context.Context, name, podName, namespace, containerID string) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(
		ctx, name,
		trace.WithAttributes(
			telemetry.AttrPodName.String(podName),
			telemetry.AttrPodNamespace.String(namespace),
			telemetry.AttrContainerID.String(containerID),
		),
	)
}
