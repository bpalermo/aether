package prober

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// syncBuffer is a goroutine-safe io.Writer for the AETHER_PROBE_FAIL sink: the
// fail log is written from probe goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingServer accepts a request and holds it until the test ends, and
// reports each arrival on started.
func blockingServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 64)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv, started
}

func newTestProber(t *testing.T, cfg Config, failOut *syncBuffer) (*Prober, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p, err := newProber(cfg, slog.New(slog.DiscardHandler), resource.Empty(), provider.Meter("test"), nil, failOut)
	if err != nil {
		t.Fatalf("newProber: %v", err)
	}
	return p, reader
}

// resultCounts sums aether_probe_requests_total by result.
func resultCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aether_probe_requests_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("aether_probe_requests_total is %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				r, _ := dp.Attributes.Value("result")
				out[r.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestRun_ShutdownCancelIsNotAFailure is #1209: a probe in flight when the
// prober itself is stopped fails with context.Canceled. That is the prober
// going away, not the data plane failing, so it must not be counted (under any
// result) nor logged as AETHER_PROBE_FAIL.
func TestRun_ShutdownCancelIsNotAFailure(t *testing.T) {
	srv, started := blockingServer(t)
	cfg := DefaultConfig()
	cfg.Egress = strings.TrimPrefix(srv.URL, "http://")
	cfg.LivenessPath = "/"
	cfg.Rate = 50
	cfg.Timeout = time.Minute // the probe's own deadline must not be what ends it
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no probe reached the server")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// Run must not return while a probe can still record: once it has, the final
	// flush and the provider shutdown are behind us. Give a straggler time to
	// show itself so a probe recorded AFTER Run returned also fails the test.
	time.Sleep(200 * time.Millisecond)

	for result, n := range resultCounts(t, reader) {
		t.Errorf("result=%q moved by %d on shutdown; want no series at all", result, n)
	}
	if s := failOut.String(); strings.Contains(s, "AETHER_PROBE_FAIL") {
		t.Errorf("shutdown logged a probe failure:\n%s", s)
	}
}

// TestProbe_OwnTimeoutStillCountsAsTimeout pins the other half of #1209: a probe
// that blows its OWN deadline while the prober is running is a real timeout.
func TestProbe_OwnTimeoutStillCountsAsTimeout(t *testing.T) {
	srv, _ := blockingServer(t)
	cfg := DefaultConfig()
	cfg.Egress = strings.TrimPrefix(srv.URL, "http://")
	cfg.LivenessPath = "/"
	cfg.Timeout = 50 * time.Millisecond
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	got := resultCounts(t, reader)
	if got[resultTimeout] != 1 || len(got) != 1 {
		t.Fatalf("results = %v, want exactly {timeout:1}", got)
	}
	if s := failOut.String(); !strings.Contains(s, "AETHER_PROBE_FAIL") {
		t.Errorf("a real timeout must log AETHER_PROBE_FAIL; got:\n%s", s)
	}
}

// TestProbe_CancelWithoutShutdownStillCounted: a cancel the prober did not
// cause (its run context still live) keeps today's classification.
func TestProbe_CancelWithoutShutdownStillCounted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Egress = "127.0.0.1:1"
	cfg.LivenessPath = "/"
	p, reader := newTestProber(t, cfg, &syncBuffer{})
	tgt := findTarget(t, p, tierLiveness)
	tgt.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})}

	p.probe(context.Background(), tgt)

	if got := resultCounts(t, reader); got[resultConnectionError] != 1 {
		t.Fatalf("results = %v, want connection_error:1", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
