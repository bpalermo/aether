package server

import (
	"context"
	"fmt"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"aethermesh.dev/common/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The CNI plugin exports no telemetry of its own (#1166). What it measures that
// this server cannot see (its own pre-call work, the readiness probe and the
// capture divert it runs after AddPod answers) arrives on the CNI requests and
// is recorded here: as attributes on the otelgrpc RPC span, and the capture
// divert outcome as aether.cni.operations — the name and attributes the plugin
// itself exported before #1166, so the soak gate on
// aether_cni_operations_total{aether_cni_operation="capture_divert",aether_cni_result="error"}
// reads the same series, now with the agent's resource attributes.

// Plugin-operation attribute keys. Bounded cardinality: one operation, two results.
const (
	attrCNIOperation = attribute.Key("aether.cni.operation")
	attrCNIResult    = attribute.Key("aether.cni.result")
)

// Span attribute keys for the forwarded plugin timings, in seconds.
const (
	attrPluginPreCall        = attribute.Key("aether.cni.plugin.pre_call_seconds")
	attrPluginTotal          = attribute.Key("aether.cni.plugin.total_seconds")
	attrPluginReadinessProbe = attribute.Key("aether.cni.plugin.readiness_probe_seconds")
	attrPluginReadinessError = attribute.Key("aether.cni.plugin.readiness_probe_error")
	attrPluginCaptureDivert  = attribute.Key("aether.cni.plugin.capture_divert_seconds")
	attrPluginCaptureError   = attribute.Key("aether.cni.plugin.capture_divert_error")
)

// opCaptureDivert is the aether.cni.operation value for the capture divert.
const opCaptureDivert = "capture_divert"

// registerPluginInstruments registers the counter for operations the CNI plugin
// reports.
func (m *cniMetrics) registerPluginInstruments(meter metric.Meter) error {
	var err error
	if m.pluginOperations, err = meter.Int64Counter("aether.cni.operations",
		metric.WithDescription("CNI plugin operations reported to the agent, by operation and result. operation=capture_divert result=error is a pod running UNCAPTURED: the mesh does nothing for it")); err != nil {
		return fmt.Errorf("cni plugin operations: %w", err)
	}
	return nil
}

// pluginOperation counts one reported plugin operation. Not seeded: the soak
// gate reads "no error series" and proves the export with the success series.
func (m *cniMetrics) pluginOperation(ctx context.Context, op string, opErr string) {
	if m == nil {
		return
	}
	result := "success"
	if opErr != "" {
		result = "error"
	}
	m.pluginOperations.Add(ctx, 1, metric.WithAttributes(attrCNIOperation.String(op), attrCNIResult.String(result)))
}

// recordPluginTimings puts the plugin's forwarded timings on the RPC span in
// ctx. A plugin that predates them sends none, and nothing is recorded.
func recordPluginTimings(ctx context.Context, timings *cniv1.PluginTimings) {
	if timings == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	if d := timings.GetPreCall(); d != nil {
		span.SetAttributes(attrPluginPreCall.Float64(seconds(d)))
	}
}

// ReportAddResult records what the plugin did after AddPod answered. It never
// fails on content: the plugin treats it as best-effort and ignores the answer.
func (s *CNIServer) ReportAddResult(ctx context.Context, req *cniv1.ReportAddResultRequest) (*cniv1.ReportAddResultResponse, error) {
	log := s.log.With("pod", req.GetName(), "namespace", req.GetNamespace())

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			telemetry.AttrPodName.String(req.GetName()),
			telemetry.AttrPodNamespace.String(req.GetNamespace()),
			telemetry.AttrContainerID.String(req.GetContainerId()),
		)
		if d := req.GetTotal(); d != nil {
			span.SetAttributes(attrPluginTotal.Float64(seconds(d)))
		}
		if d := req.GetReadinessProbe(); d != nil {
			span.SetAttributes(attrPluginReadinessProbe.Float64(seconds(d)))
		}
		if e := req.GetReadinessProbeError(); e != "" {
			span.SetAttributes(attrPluginReadinessError.String(e))
		}
		if d := req.GetCaptureDivert(); d != nil {
			span.SetAttributes(attrPluginCaptureDivert.Float64(seconds(d)))
		}
		if e := req.GetCaptureDivertError(); e != "" {
			span.SetAttributes(attrPluginCaptureError.String(e))
		}
	}

	if req.GetCaptureDivert() != nil {
		s.metrics.pluginOperation(ctx, opCaptureDivert, req.GetCaptureDivertError())
	}
	if e := req.GetCaptureDivertError(); e != "" {
		// The plugin logs this too, but only to the node-local plugin.log; the
		// agent's log is the one that reaches the log backend.
		log.WarnContext(ctx, "CNI plugin failed to install the transparent-capture divert; POD IS RUNNING UNCAPTURED (mesh does nothing for it)",
			"containerID", req.GetContainerId(), "error", e)
	}
	if e := req.GetReadinessProbeError(); e != "" {
		log.InfoContext(ctx, "CNI plugin could not confirm the pod's data plane serving before pod start",
			"containerID", req.GetContainerId(), "error", e)
	}

	return &cniv1.ReportAddResultResponse{}, nil
}

func seconds(d *durationpb.Duration) float64 { return d.AsDuration().Seconds() }
