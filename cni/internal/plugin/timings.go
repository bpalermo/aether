package plugin

import (
	"context"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The plugin exports no telemetry of its own (#1166). It used to link the OTel
// SDK and the OTLP exporters and flush them before every exit, which cost each
// pod ADD/DEL a collector round trip and, whenever the collector was
// unreachable (#950), the full 2 s export timeout. What the plugin knows that
// the agent cannot see is forwarded instead: its own timings on the AddPod and
// RemovePod requests, and the post-AddPod outcome (readiness probe, capture
// divert) on ReportAddResult. The agent records them on its spans and counts
// the capture divert as aether.cni.operations{operation="capture_divert"}.

// pluginTimings snapshots the plugin's timings at the moment an RPC is sent.
func (p *AetherPlugin) pluginTimings() *cniv1.PluginTimings {
	return &cniv1.PluginTimings{
		Started: timestamppb.New(p.started),
		PreCall: durationpb.New(time.Since(p.started)),
	}
}

// addReport accumulates the post-AddPod outcome of one managed-pod ADD.
type addReport struct {
	req *cniv1.ReportAddResultRequest
}

func (p *AetherPlugin) newAddReport(pod *cniv1.CNIPod) *addReport {
	return &addReport{req: &cniv1.ReportAddResultRequest{
		Name:          pod.GetName(),
		Namespace:     pod.GetNamespace(),
		ContainerId:   pod.GetContainerId(),
		PluginTimings: &cniv1.PluginTimings{Started: timestamppb.New(p.started)},
	}}
}

func (r *addReport) setReadinessProbe(d time.Duration, err error) {
	r.req.ReadinessProbe = durationpb.New(d)
	if err != nil {
		r.req.ReadinessProbeError = err.Error()
	}
}

func (r *addReport) setCaptureDivert(d time.Duration, err error) {
	r.req.CaptureDivert = durationpb.New(d)
	if err != nil {
		r.req.CaptureDivertError = err.Error()
	}
}

// reportAddResult sends the report, best-effort: a failure is logged and never
// fails the ADD. An agent that predates the RPC answers UNIMPLEMENTED during a
// rolling upgrade; that is expected and logged at DEBUG only.
func (p *AetherPlugin) reportAddResult(client *CNIClient, r *addReport) {
	if r == nil {
		return
	}
	r.req.Total = durationpb.New(time.Since(p.started))
	err := client.ReportAddResult(context.Background(), r.req)
	switch {
	case err == nil:
	case status.Code(err) == codes.Unimplemented:
		p.logger.Debug("agent predates ReportAddResult; ADD outcome not reported", zap.Error(err))
	default:
		p.logger.Warn("failed to report the ADD outcome to the agent; continuing", zap.Error(err))
	}
}
