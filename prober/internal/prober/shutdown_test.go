package prober

import (
	"bytes"
	"context"
	"log/slog"
	"net"
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

// TestRun_ShutdownSummaryIsStampedWithTheFlushTime is #1463: the summary a
// stopping prober writes for the failures its cap suppressed is the last line
// of its log, and its `t` is when it was written. It used to be that time plus
// one window (the shutdown flush passed a future `now` to force every window
// closed), so the line was dated a minute after the pod had stopped and a log
// query bounded at the pod's stop time missed it.
func TestRun_ShutdownSummaryIsStampedWithTheFlushTime(t *testing.T) {
	// A port nothing listens on: every probe is a fast connection_error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refused := ln.Addr().String()
	_ = ln.Close()

	cfg := DefaultConfig()
	cfg.Egress = refused
	cfg.LivenessPath = "/"
	cfg.Rate = 200
	cfg.Timeout = time.Second
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	before := time.Now()
	go func() { done <- p.Run(ctx) }()

	// Past the cap: at least one failure is only counted.
	deadline := time.Now().Add(20 * time.Second)
	for resultCounts(t, reader)[resultConnectionError] <= failLogCap {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d failed probes in 20 s: %v", failLogCap+1, resultCounts(t, reader))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	after := time.Now()

	lines := failLines(t, failOut.String())
	if len(lines) == 0 {
		t.Fatal("no AETHER_PROBE_FAIL line")
	}
	last := lines[len(lines)-1]
	if n, _ := last["suppressed"].(float64); n < 1 {
		t.Fatalf("the last line is not a suppressed summary: %v", last)
	}
	at := parseFailTime(t, last, "t")
	if at.Before(before) || at.After(after) {
		t.Errorf("summary t = %s, want the time it was written, between %s and %s (the prober stopped at the latter)",
			at.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
	// The window's own start: the first failure the summary's window holds.
	from := parseFailTime(t, last, "window_start")
	if from.Before(before) || from.After(at) {
		t.Errorf("summary window_start = %s, want between the run's start %s and t %s",
			from.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	}
	if first := parseFailTime(t, lines[0], "t"); !from.Equal(first) {
		t.Errorf("summary window_start = %s, want the time of the window's first failure, %s",
			from.Format(time.RFC3339Nano), first.Format(time.RFC3339Nano))
	}
	if last["window_s"] != failLogWindow.Seconds() {
		t.Errorf("summary window_s = %v, want %v", last["window_s"], failLogWindow.Seconds())
	}
}

func parseFailTime(t *testing.T, line map[string]any, key string) time.Time {
	t.Helper()
	s, _ := line[key].(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("line %v: %s = %q is not an RFC3339 time: %v", line, key, line[key], err)
	}
	return at
}

// TestFailLogFlushAll pins the two flushes apart (#1463). flush closes the
// windows that have run their length; flushAll closes every open one, however
// young. Both stamp the summary with the time they were given: what time it is
// and which windows to close are separate questions.
func TestFailLogFlushAll(t *testing.T) {
	var out bytes.Buffer
	f := newFailLog(&out, testPod, testNode, 2, time.Minute)
	opened := time.Date(2026, 10, 8, 4, 38, 0, 0, time.UTC)
	dns := target{tier: tierMeshDNS, name: "echo.aether-test.aether.internal:18081"}
	for i := range 5 {
		f.log(opened.Add(time.Duration(i)*time.Second), dns, resultTimeout, 2, context.DeadlineExceeded, noPhase)
	}
	// Under the cap: its window closes without a summary.
	f.log(opened, target{tier: tierLiveness, name: "egress"}, resultTimeout, 2, nil, noPhase)
	out.Reset()

	stop := opened.Add(10 * time.Second) // the window is ten seconds old
	f.flush(stop)
	if out.Len() != 0 {
		t.Fatalf("flush of a ten-second-old window printed %q, want nothing", out.String())
	}
	f.flushAll(stop)
	summary := failLines(t, out.String())
	if len(summary) != 1 {
		t.Fatalf("flushAll printed %d lines, want one summary: %q", len(summary), out.String())
	}
	for key, want := range map[string]any{
		"t": "2026-10-08T04:38:10Z", "window_start": "2026-10-08T04:38:00Z",
		"tier": tierMeshDNS, "result": resultTimeout, "suppressed": float64(3), "window_s": float64(60),
		"pod": testPod, "node": testNode,
	} {
		if summary[0][key] != want {
			t.Errorf("summary %s = %#v, want %#v (line %v)", key, summary[0][key], want, summary[0])
		}
	}
	// Every window is closed: a second flush has nothing to say, and the next
	// failure opens a new window with a new budget.
	out.Reset()
	f.flushAll(stop.Add(time.Hour))
	if out.Len() != 0 {
		t.Fatalf("a second flushAll printed %q, want nothing", out.String())
	}
	f.log(stop, dns, resultTimeout, 2, nil, noPhase)
	if got := failLines(t, out.String()); len(got) != 1 || got[0]["n"] != float64(1) {
		t.Fatalf("after flushAll the next failure printed %v, want one detail line with n=1", got)
	}
}
