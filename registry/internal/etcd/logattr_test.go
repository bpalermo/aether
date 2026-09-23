package etcd_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry/internal/etcd"
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
// "unhandled: (registryv1.Service_Protocol) PROTOCOL_TCP".
//
// That string is what reaches the collector, and it is why a
// `protocol:PROTOCOL_TCP` log query matches nothing while returning an empty
// result rather than an error.
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

// otelBridgeLogger returns a logger wired through the REAL otelslog bridge the
// registrar and agent run in production (common/manager.SetupManagerLogging),
// together with the processor that captured what the bridge produced.
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

// TestEtcdRegistry_ProtocolLogAttributeSurvivesOTelBridge pins issue #904: the
// registry's protocol log attribute must reach the exporter as the bare enum
// name, so that a `protocol:PROTOCOL_TCP` log filter can match it.
//
// It exercises every protocol-carrying log site in the endpoint paths —
// RegisterEndpoint, both ListEndpoints sites, both ListAllEndpoints sites and
// the ListAllEndpoints error branch — through the real bridge, and fails if any
// of them regresses to passing the raw enum.
func TestEtcdRegistry_ProtocolLogAttributeSurvivesOTelBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	logger, proc := otelBridgeLogger(t)

	registry := etcd.NewEtcdRegistry(logger, etcd.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: 5 * time.Second,
		KeyPrefix:   keyPrefix(t),
	})
	require.NoError(t, registry.Initialize(ctx))
	t.Cleanup(func() { _ = registry.Close() })

	const service = "frontend"
	require.NoError(t, registry.RegisterEndpoint(ctx, service, registryv1.Service_PROTOCOL_TCP,
		&registryv1.ServiceEndpoint{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100}))

	_, err := registry.ListEndpoints(ctx, service, registryv1.Service_PROTOCOL_TCP)
	require.NoError(t, err)

	_, err = registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_TCP)
	require.NoError(t, err)

	// The failure branch of ListAllEndpoints logs the protocol too, and an
	// operator reading that line is precisely the person who would then filter
	// on it. A cancelled context is the cheapest way to reach it.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = registry.ListAllEndpoints(cancelled, registryv1.Service_PROTOCOL_TCP)
	require.Error(t, err)

	var protocols []string
	for _, kv := range proc.snapshot() {
		emitted := kv.Value.Emit()
		assert.NotContains(t, emitted, unhandledPrefix,
			"attribute %q reached the exporter unconverted by the otelslog bridge", kv.Key)
		if kv.Key == "protocol" {
			protocols = append(protocols, emitted)
		}
	}

	// RegisterEndpoint (1) + ListEndpoints (2) + ListAllEndpoints (2) +
	// ListAllEndpoints error branch (2: the entry log and the error log).
	require.GreaterOrEqual(t, len(protocols), 6,
		"expected every protocol-carrying log site to emit; got %v", protocols)
	for _, got := range protocols {
		assert.Equal(t, registryv1.Service_PROTOCOL_TCP.String(), got,
			"protocol attribute must be the bare enum name so a log filter can match it")
	}
}
