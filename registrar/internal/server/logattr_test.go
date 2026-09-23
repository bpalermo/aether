package server

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// unhandledPrefix is what the otelslog bridge emits for a value it cannot
// convert (issue #904). otelslog's convertValue matches only the predeclared
// scalar types in its fast-path type switch, and its reflect fallback handles
// only struct/slice/array/map/pointer/interface kinds. A protobuf enum is a
// NAMED int32, so it matches neither and renders as
// "unhandled: (registryv1.Service_Protocol) PROTOCOL_TCP" — a value no
// equality log filter can match, and one that returns empty rather than
// erroring.
const unhandledPrefix = "unhandled: "

// capturingProcessor is an sdklog.Processor that keeps every attribute of
// every record the bridge produced, i.e. exactly what the OTLP exporter would
// have shipped.
type capturingProcessor struct {
	mu    sync.Mutex
	attrs []attribute.KeyValue
}

func (p *capturingProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (p *capturingProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		p.attrs = append(p.attrs, kv)
		return true
	})
	return nil
}

func (p *capturingProcessor) Shutdown(context.Context) error   { return nil }
func (p *capturingProcessor) ForceFlush(context.Context) error { return nil }

func (p *capturingProcessor) snapshot() []attribute.KeyValue {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]attribute.KeyValue(nil), p.attrs...)
}

// values returns every emitted value recorded under key, after asserting that
// no captured attribute at all was left unconverted by the bridge.
func (p *capturingProcessor) values(t *testing.T, key string) []string {
	t.Helper()
	var out []string
	for _, kv := range p.snapshot() {
		emitted := kv.Value.Emit()
		assert.NotContains(t, emitted, unhandledPrefix,
			"attribute %q reached the exporter unconverted by the otelslog bridge", kv.Key)
		if string(kv.Key) == key {
			out = append(out, emitted)
		}
	}
	return out
}

// otelBridgeLogger returns a logger wired through the REAL otelslog bridge the
// registrar runs in production (common/manager.SetupManagerLogging), together
// with the processor that captured what the bridge produced.
//
// The bridge is the point of the exercise. A plain slog test handler proves
// nothing about this bug: slog's own handlers never produce the mangled string
// (TextHandler renders the enum's String method, JSONHandler renders its
// number), so a test over slog output passes identically before and after the
// fix. The corruption happens only in otelslog's slog.Value → attribute.Value
// conversion, so the test has to run that conversion.
func otelBridgeLogger(t *testing.T) (*slog.Logger, *capturingProcessor) {
	t.Helper()

	proc := &capturingProcessor{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	return slog.New(otelslog.NewHandler(t.Name(), otelslog.WithLoggerProvider(provider))), proc
}

// TestListAllEndpoints_ProtocolLogAttributeSurvivesOTelBridge pins issue #904
// for the registrar's ListAllEndpoints RPC: the protocol must reach the
// exporter as the bare enum name so a `protocol:PROTOCOL_HTTP` log filter can
// match it.
func TestListAllEndpoints_ProtocolLogAttributeSurvivesOTelBridge(t *testing.T) {
	logger, proc := otelBridgeLogger(t)

	snap := NewSnapshot()
	snap.Apply([]*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-a",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.1"},
	}})
	s := NewRegistrarServer(nil, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", logger, nil)

	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)

	_, err := s.ListAllEndpoints(context.Background(), &registrarv1.ListAllEndpointsRequest{
		Protocol: registryv1.Service_PROTOCOL_HTTP,
	})
	require.NoError(t, err)

	protocols := proc.values(t, "protocol")
	require.NotEmpty(t, protocols, "ListAllEndpoints must log the protocol it served")
	for _, got := range protocols {
		assert.Equal(t, registryv1.Service_PROTOCOL_HTTP.String(), got,
			"protocol attribute must be the bare enum name so a log filter can match it")
	}
}

// TestBroadcaster_EventTypeLogAttributeSurvivesOTelBridge pins issue #904 for
// the broadcaster's slow-watcher overflow line. That line is the staleness
// alarm an operator grep for, and its sibling metric already records
// GetType().String() — the log attribute must agree with it.
func TestBroadcaster_EventTypeLogAttributeSurvivesOTelBridge(t *testing.T) {
	logger, proc := otelBridgeLogger(t)

	b := NewBroadcaster(logger, nil)
	ch := b.Subscribe("slow-node", nil)

	event := &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-fill",
	}
	for range defaultChannelBuffer {
		b.Broadcast([]*registrarv1.WatchEndpointsResponse{event})
	}
	require.Equal(t, defaultChannelBuffer, len(ch), "channel must be full before the overflow")

	// One more event overflows the watcher and logs the force-resync line.
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{event})
	require.Equal(t, 0, b.WatcherCount(), "overflowed watcher must be removed")

	eventTypes := proc.values(t, "eventType")
	require.NotEmpty(t, eventTypes, "the force-resync line must log the event type")
	for _, got := range eventTypes {
		assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED.String(), got,
			"eventType attribute must be the bare enum name so a log filter can match it")
	}
}
