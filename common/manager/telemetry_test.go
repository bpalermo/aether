package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/common/telemetry/setup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shedddingCollector starts a gRPC server on loopback that answers every RPC the
// way a saturated otel-collector does — the exact status the memory_limiter
// processor returns when it sheds (issue #662). It needs no OTLP protos: the
// unknown-service handler covers every method.
func shedddingCollector(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
		return status.Error(codes.ResourceExhausted, "data refused due to high memory usage")
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// blackHoleCollector starts a listener that accepts connections and then says
// nothing at all — a collector that is reachable but never answers.
func blackHoleCollector(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		<-done
	})
	return lis.Addr().String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func telemetryConfig(endpoint string) Config {
	return Config{
		OTelEnabled:     true,
		LogsEnabled:     true,
		OTLPEndpoint:    endpoint,
		TraceSampleRate: 1.0,
		TracingExport:   true,
	}
}

// TestSetupTelemetry_SheddingCollectorDoesNotFailStartup is the core regression
// test for issue #662: a collector refusing data must leave the caller with a
// working (if degraded) component and an untouched startup context.
func TestSetupTelemetry_SheddingCollectorDoesNotFailStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	shutdown := setupTelemetry(ctx, telemetryConfig(shedddingCollector(t)), "test-service", "v0.0.1", discardLogger())
	elapsed := time.Since(start)

	if shutdown == nil {
		t.Fatal("setupTelemetry() returned no shutdown; telemetry was not installed")
	}
	if elapsed > setup.SetupTimeout {
		t.Fatalf("setup took %s, want < %s", elapsed, setup.SetupTimeout)
	}
	// The startup context must be untouched: SPIRE, xDS and CNI initialization
	// all hang off it, and a cancelled one is what killed the agent.
	if err := ctx.Err(); err != nil {
		t.Fatalf("telemetry setup disturbed the caller's context: %v", err)
	}

	// The final flush must still run after SIGTERM has cancelled the caller's
	// context: an export failure is fine here, "context canceled" is not.
	cancel()
	if err := shutdown(ctx); errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown() returned %v; the flush ran on the caller's dead context", err)
	}
}

func TestSetupTelemetry_BlackHoleCollectorDoesNotBlockStartup(t *testing.T) {
	ctx := context.Background()

	start := time.Now()
	shutdown := setupTelemetry(ctx, telemetryConfig(blackHoleCollector(t)), "test-service", "v0.0.1", discardLogger())
	elapsed := time.Since(start)

	if shutdown == nil {
		t.Fatal("setupTelemetry() returned no shutdown; telemetry was not installed")
	}
	if elapsed > setup.SetupTimeout {
		t.Fatalf("setup took %s, want < %s", elapsed, setup.SetupTimeout)
	}
}

// TestSetupTelemetry_DetachedFromCallerContext proves the two contexts are
// separated: telemetry setup completes even when the caller's context is already
// dead, so telemetry can neither be killed by nor kill the startup sequence.
func TestSetupTelemetry_DetachedFromCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	shutdown := setupTelemetry(ctx, telemetryConfig(shedddingCollector(t)), "test-service", "v0.0.1", discardLogger())
	if shutdown == nil {
		t.Fatal("setupTelemetry() returned no shutdown for an already-cancelled caller context")
	}
	if err := shutdown(ctx); errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown() returned %v; the flush ran on the caller's dead context", err)
	}
}

// TestSetupTelemetry_Disabled keeps --otel-enabled=false behaving exactly as
// before: no MeterProvider, but the always-on TracerProvider is still installed
// (it is what stamps trace_id onto logs), so a shutdown is still returned.
func TestSetupTelemetry_Disabled(t *testing.T) {
	cfg := Config{OTelEnabled: false, TraceSampleRate: 1.0}

	shutdown := setupTelemetry(context.Background(), cfg, "test-service", "v0.0.1", discardLogger())
	if shutdown == nil {
		t.Fatal("setupTelemetry() should still install the TracerProvider when OTel is disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

// badEndpoint is an OTLP endpoint the gRPC exporters cannot even parse, so
// provider construction fails outright — the case that used to abort Bootstrap
// with "failed to setup telemetry" and exit the process.
const badEndpoint = "%%%"

func TestSetupTelemetry_SetupFailureIsNotFatal(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdown := setupTelemetry(ctx, telemetryConfig(badEndpoint), "test-service", "v0.0.1", l)

	// No telemetry could be installed, so there is nothing to flush — but the
	// caller gets control back with a live context instead of a fatal error.
	if shutdown != nil {
		t.Fatal("setupTelemetry() installed telemetry against an unusable endpoint")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("telemetry setup disturbed the caller's context: %v", err)
	}
	for _, want := range []string{"failed to set up OTel metrics", "failed to set up OTel tracing"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log = %q, want it to contain %q", buf.String(), want)
		}
	}
}

func TestSetupManagerLogging_SetupFailureFallsBackToStderr(t *testing.T) {
	l, shutdown := SetupManagerLogging(context.Background(), telemetryConfig(badEndpoint), "test-service", "v0.0.1")
	if l == nil {
		t.Fatal("SetupManagerLogging() returned a nil logger for an unusable endpoint")
	}
	if shutdown != nil {
		t.Fatal("SetupManagerLogging() returned a shutdown though no provider was created")
	}
	// The logger must still work: stderr logging is unaffected by OTLP export.
	l.Info("component continues with stderr-only logging")
}

// TestSetupManagerLogging_SheddingCollector asserts the logging half of the same
// property: the logger is usable, emitting records does not block on the dead
// collector, and the final flush is not defeated by a cancelled caller context.
func TestSetupManagerLogging_SheddingCollector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l, shutdown := SetupManagerLogging(ctx, telemetryConfig(shedddingCollector(t)), "test-service", "v0.0.1")
	if l == nil {
		t.Fatal("SetupManagerLogging() returned a nil logger")
	}
	if shutdown == nil {
		t.Fatal("SetupManagerLogging() returned no shutdown; OTLP logging was not installed")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("log setup disturbed the caller's context: %v", err)
	}

	logged := make(chan struct{})
	go func() {
		defer close(logged)
		l.InfoContext(ctx, "hello from a component whose collector is shedding")
	}()
	select {
	case <-logged:
	case <-time.After(5 * time.Second):
		t.Fatal("emitting a log record blocked on the shedding collector")
	}

	cancel()
	if err := shutdown(ctx); errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown() returned %v; the flush ran on the caller's dead context", err)
	}
}

// TestSetupManagerLogging_Disabled keeps the OTLP-off path unchanged: a plain
// stderr logger and no shutdown.
func TestSetupManagerLogging_Disabled(t *testing.T) {
	l, shutdown := SetupManagerLogging(context.Background(), Config{}, "test-service", "v0.0.1")
	if l == nil {
		t.Fatal("SetupManagerLogging() returned a nil logger")
	}
	if shutdown != nil {
		t.Fatal("SetupManagerLogging() returned a shutdown with OTLP logging disabled")
	}
}
