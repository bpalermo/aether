package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"aethermesh.dev/cni/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// timingsRecorder captures what the plugin forwards (#1166).
type timingsRecorder struct {
	cniv1.UnimplementedCNIServiceServer
	addResult *cniv1.AddPodResponse_Result
	add       *cniv1.AddPodRequest
	remove    *cniv1.RemovePodRequest
	report    *cniv1.ReportAddResultRequest
}

func (r *timingsRecorder) AddPod(_ context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	r.add = req
	result := cniv1.AddPodResponse_RESULT_SUCCESS
	if r.addResult != nil {
		result = *r.addResult
	}
	return &cniv1.AddPodResponse{Result: result}, nil
}

func (r *timingsRecorder) RemovePod(_ context.Context, req *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	r.remove = req
	return &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_RESULT_SUCCESS}, nil
}

func (r *timingsRecorder) ReportAddResult(_ context.Context, req *cniv1.ReportAddResultRequest) (*cniv1.ReportAddResultResponse, error) {
	r.report = req
	return &cniv1.ReportAddResultResponse{}, nil
}

func testPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{Name: "p", Namespace: "ns", ContainerId: "c1", NetworkNamespace: "/proc/1/ns/net"}
}

// The AddPod request carries the plugin's start time and its pre-call duration,
// measured from the plugin's start: the work the agent's span cannot see.
func TestSendAddPod_ForwardsPluginTimings(t *testing.T) {
	rec := &timingsRecorder{}
	client := newBufconnClient(t, startMockServer(t, rec))
	p := NewAetherPlugin(zap.NewNop())
	started := time.Now().Add(-250 * time.Millisecond)
	p.SetStarted(started)

	ignored, report, err := p.sendAddPod(context.Background(), config.AetherConf{ReadinessProbeDisabled: true}, client, testPod())
	require.NoError(t, err)
	assert.False(t, ignored)
	require.NotNil(t, report, "a managed pod's ADD must produce a report")

	require.NotNil(t, rec.add.GetPluginTimings())
	assert.True(t, rec.add.GetPluginTimings().GetStarted().AsTime().Equal(started))
	assert.GreaterOrEqual(t, rec.add.GetPluginTimings().GetPreCall().AsDuration(), 250*time.Millisecond)
	assert.Nil(t, report.req.GetReadinessProbe(), "a disabled probe reports no duration")
}

// An unmanaged pod gets no report: nothing ran after AddPod.
func TestSendAddPod_IgnoredHasNoReport(t *testing.T) {
	ignoredResult := cniv1.AddPodResponse_RESULT_IGNORED
	rec := &timingsRecorder{addResult: &ignoredResult}
	client := newBufconnClient(t, startMockServer(t, rec))

	ignored, report, err := NewAetherPlugin(zap.NewNop()).sendAddPod(context.Background(), config.AetherConf{}, client, testPod())
	require.NoError(t, err)
	assert.True(t, ignored)
	assert.Nil(t, report)
}

// The report carries the capture-divert outcome, including its error: a
// non-empty capture_divert_error is a pod running UNCAPTURED, and the agent's
// counter of it is the soak gate.
func TestReportAddResult_CarriesCaptureDivert(t *testing.T) {
	rec := &timingsRecorder{}
	client := newBufconnClient(t, startMockServer(t, rec))
	p := NewAetherPlugin(zap.NewNop())

	r := p.newAddReport(testPod())
	r.setReadinessProbe(30*time.Millisecond, errors.New("probe timed out"))
	r.setCaptureDivert(4*time.Millisecond, errors.New("nft_tproxy missing"))
	p.reportAddResult(client, r)

	require.NotNil(t, rec.report)
	assert.Equal(t, "p", rec.report.GetName())
	assert.Equal(t, "ns", rec.report.GetNamespace())
	assert.Equal(t, "c1", rec.report.GetContainerId())
	assert.Equal(t, 4*time.Millisecond, rec.report.GetCaptureDivert().AsDuration())
	assert.Equal(t, "nft_tproxy missing", rec.report.GetCaptureDivertError())
	assert.Equal(t, 30*time.Millisecond, rec.report.GetReadinessProbe().AsDuration())
	assert.Equal(t, "probe timed out", rec.report.GetReadinessProbeError())
	assert.NotNil(t, rec.report.GetTotal())
	assert.NotNil(t, rec.report.GetPluginTimings().GetStarted())
}

// An agent that predates ReportAddResult answers UNIMPLEMENTED during a
// rolling upgrade. That is expected: DEBUG, never WARN, and never an error.
func TestReportAddResult_OlderAgentIsQuiet(t *testing.T) {
	client := newBufconnClient(t, startMockServer(t, &mockCNIService{}))
	core, logs := observer.New(zap.DebugLevel)
	p := NewAetherPlugin(zap.New(core))

	p.reportAddResult(client, p.newAddReport(testPod()))

	assert.Zero(t, logs.FilterLevelExact(zap.WarnLevel).Len(), "an older agent must not produce a WARN per pod ADD")
	assert.Equal(t, 1, logs.FilterMessage("agent predates ReportAddResult; ADD outcome not reported").Len())
}

func TestSendRemovePod_ForwardsPluginTimings(t *testing.T) {
	rec := &timingsRecorder{}
	client := newBufconnClient(t, startMockServer(t, rec))

	_, err := client.RemovePod(context.Background(), "p", "ns", "c1", NewAetherPlugin(zap.NewNop()).pluginTimings())
	require.NoError(t, err)
	require.NotNil(t, rec.remove.GetPluginTimings())
	assert.NotNil(t, rec.remove.GetPluginTimings().GetPreCall())
}
