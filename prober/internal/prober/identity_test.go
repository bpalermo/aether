package prober

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	testPod  = "prober-h2mzs"
	testNode = "main-worker-03"
)

// setProberEnv sets OTEL_RESOURCE_ATTRIBUTES exactly as charts/prober's DaemonSet does.
func setProberEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES",
		"k8s.node.name="+testNode+",k8s.namespace.name=aether-test,k8s.pod.name="+testPod)
}

// TestResourceIdentity pins the prober's node identity to k8s.node.name (#1041). The
// resource must carry k8s.node.name and k8s.pod.name from the chart's env, and must NOT
// carry host.name: on a non-hostNetwork pod host.name is the POD name, and the collector
// promoted it to the metric `node` label ahead of k8s.node.name, so every prober series
// said node="prober-xxxxx".
func TestResourceIdentity(t *testing.T) {
	setProberEnv(t)
	res, err := newResource(context.Background(), "test")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	for key, want := range map[attribute.Key]string{
		semconv.K8SNodeNameKey: testNode,
		semconv.K8SPodNameKey:  testPod,
	} {
		if got := resourceString(res, key); got != want {
			t.Errorf("resource %s = %q, want %q", key, got, want)
		}
	}
	if v, ok := res.Set().Value(semconv.HostNameKey); ok {
		t.Errorf("resource carries host.name=%q; the prober must not depend on host.name (it is the pod name)", v.AsString())
	}
}

// TestDatapointsCarryPod asserts the EXPORTED datapoints of both prober metrics carry
// `pod` alongside tier/target/result (#1041): once `node` is the Kubernetes node, the
// pod attribute is what keeps per-pod anomalies (and two prober generations on one
// node) apart.
func TestDatapointsCarryPod(t *testing.T) {
	setProberEnv(t)
	ctx := context.Background()
	res, err := newResource(ctx, "test")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	p, err := newProber(DefaultConfig(), slog.New(slog.DiscardHandler), res, mp.Meter(telemetryServiceName), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newProber: %v", err)
	}
	p.record(p.targets[0], resultSuccess, 0.002, nil, noPhase)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counter, ok := findMetric(t, &rm, "aether_probe_requests_total").Data.(metricdata.Sum[int64])
	if !ok || len(counter.DataPoints) != 1 {
		t.Fatalf("aether_probe_requests_total: want one Sum[int64] datapoint, got %#v", counter)
	}
	assertAttrs(t, "aether_probe_requests_total", counter.DataPoints[0].Attributes)

	hist, ok := findMetric(t, &rm, "aether_probe_request_duration_seconds").Data.(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("aether_probe_request_duration_seconds: want one histogram datapoint, got %#v", hist)
	}
	assertAttrs(t, "aether_probe_request_duration_seconds", hist.DataPoints[0].Attributes)
}

func assertAttrs(t *testing.T, metricName string, set attribute.Set) {
	t.Helper()
	for key, want := range map[attribute.Key]string{
		"tier":   tierLiveness,
		"target": "egress",
		"result": resultSuccess,
		"pod":    testPod,
	} {
		v, ok := set.Value(key)
		if !ok || v.AsString() != want {
			t.Errorf("%s datapoint %s = %q (present=%v), want %q", metricName, key, v.AsString(), ok, want)
		}
	}
	// `node` is the collector's to set from k8s.node.name; the prober must not stamp one.
	if v, ok := set.Value("node"); ok {
		t.Errorf("%s datapoint carries node=%q; node comes from the resource's k8s.node.name", metricName, v.AsString())
	}
}

// failLines parses every AETHER_PROBE_FAIL line in out into a generic map, failing the
// test on any line that is not marker + one JSON object.
func failLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		raw, ok := strings.CutPrefix(sc.Text(), failLinePrefix)
		if !ok {
			t.Fatalf("line without the %q marker: %q", failLinePrefix, sc.Text())
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("line is not one JSON object: %q: %v", raw, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// TestProbeFailureLine drives REAL failing probes through probe() and asserts the
// AETHER_PROBE_FAIL line (#1040): one JSON line per failure carrying the time, tier,
// target, result, error, elapsed_ms, pod and node — and that the cap bounds a burst to
// failLogCap detail lines per (tier, result), with the rest counted in one summary line.
func TestProbeFailureLine(t *testing.T) {
	setProberEnv(t)
	ctx := context.Background()

	// A port nothing listens on: every probe is a fast connection_error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refused := ln.Addr().String()
	_ = ln.Close()

	res, err := newResource(ctx, "test")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Egress = refused
	cfg.Timeout = time.Second
	var out bytes.Buffer
	p, err := newProber(cfg, slog.New(slog.DiscardHandler), res, noopMeter(), nil, &out)
	if err != nil {
		t.Fatalf("newProber: %v", err)
	}
	liveness := findTarget(t, p, tierLiveness)

	const burst = failLogCap + 7
	start := time.Now()
	for range burst {
		p.probe(ctx, liveness)
	}
	lines := failLines(t, out.String())
	if len(lines) != failLogCap {
		t.Fatalf("a burst of %d failures printed %d lines, want exactly the cap %d", burst, len(lines), failLogCap)
	}
	first := lines[0]
	for key, want := range map[string]any{
		"tier": tierLiveness, "target": "egress", "result": resultConnectionError,
		"pod": testPod, "node": testNode, "n": float64(1), "truncated": false,
	} {
		if first[key] != want {
			t.Errorf("first line %s = %#v, want %#v (line %v)", key, first[key], want, first)
		}
	}
	if e, _ := first["err"].(string); !strings.Contains(e, "refused") {
		t.Errorf("first line err = %q, want the dial error (connection refused)", e)
	}
	ts, _ := first["t"].(string)
	if at, err := time.Parse(time.RFC3339Nano, ts); err != nil || at.Before(start.Add(-time.Second)) {
		t.Errorf("first line t = %q (%v), want an RFC3339 timestamp of the failure", ts, err)
	}
	if ms, ok := first["elapsed_ms"].(float64); !ok || ms < 0 || ms >= 1000 {
		t.Errorf("first line elapsed_ms = %#v, want a fast (< 1000 ms) refusal", first["elapsed_ms"])
	}
	if last := lines[len(lines)-1]; last["n"] != float64(failLogCap) || last["truncated"] != true {
		t.Errorf("last detail line n=%v truncated=%v, want n=%d truncated=true", last["n"], last["truncated"], failLogCap)
	}

	// Closing the window prints ONE summary with the suppressed count.
	out.Reset()
	p.fails.flush(time.Now().Add(failLogWindow))
	summary := failLines(t, out.String())
	if len(summary) != 1 {
		t.Fatalf("window close printed %d lines, want 1 summary: %q", len(summary), out.String())
	}
	for key, want := range map[string]any{
		"tier": tierLiveness, "result": resultConnectionError, "suppressed": float64(burst - failLogCap),
		"window_s": failLogWindow.Seconds(), "pod": testPod, "node": testNode,
	} {
		if summary[0][key] != want {
			t.Errorf("summary %s = %#v, want %#v (line %v)", key, summary[0][key], want, summary[0])
		}
	}

	// A new window has a fresh budget: the next failure is logged in full again.
	out.Reset()
	p.probe(ctx, liveness)
	if got := failLines(t, out.String()); len(got) != 1 || got[0]["n"] != float64(1) {
		t.Fatalf("first failure of a new window: got %v, want one detail line with n=1", got)
	}
}

// TestProbeFailureLineTimeout is the other half of elapsed_ms: a probe that burns the
// whole budget is logged as result=timeout with elapsed_ms at (or just past) the budget,
// distinguishable at a glance from a fast refusal.
func TestProbeFailureLineTimeout(t *testing.T) {
	setProberEnv(t)
	ctx := context.Background()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	res, err := newResource(ctx, "test")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Egress = strings.TrimPrefix(srv.URL, "http://")
	cfg.Timeout = 150 * time.Millisecond
	var out bytes.Buffer
	p, err := newProber(cfg, slog.New(slog.DiscardHandler), res, noopMeter(), nil, &out)
	if err != nil {
		t.Fatalf("newProber: %v", err)
	}
	tgt := findTarget(t, p, tierLiveness)
	called := time.Now()
	p.probe(ctx, tgt)
	wall := time.Since(called)

	lines := failLines(t, out.String())
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), out.String())
	}
	if lines[0]["result"] != resultTimeout {
		t.Fatalf("result = %v, want %s", lines[0]["result"], resultTimeout)
	}
	// The probe starts its clock a moment AFTER it arms the context deadline (it
	// builds the request in between), so elapsed_ms can land a few ms under the
	// budget on a loaded host (#1189: 149.7 ms). The lower bound only has to tell a
	// spent budget from a fast refusal (~0 ms), so it leaves 10 ms of slack.
	ms, _ := lines[0]["elapsed_ms"].(float64)
	if ms < 140 {
		t.Fatalf("elapsed_ms = %v, want ~the 150 ms budget (>= 140 ms)", ms)
	}
	// Upper bounds: elapsed_ms is measured inside probe, so it cannot exceed the
	// call's own wall time; and it stays far below the 2 s default budget, so it was
	// this config's 150 ms deadline that fired. (+0.05 ms: the line rounds to 0.1 ms.)
	if ms > float64(wall.Microseconds())/1000+0.05 {
		t.Fatalf("elapsed_ms = %v exceeds the probe call's own wall time %v", ms, wall)
	}
	if ms >= 1000 {
		t.Fatalf("elapsed_ms = %v, want well under the 2 s default budget", ms)
	}
}

// TestFailLogCapIsPerKey pins that the cap is per (tier, result): a flood of one class
// must not silence another.
func TestFailLogCapIsPerKey(t *testing.T) {
	var out bytes.Buffer
	f := newFailLog(&out, testPod, testNode, 2, time.Minute)
	now := time.Date(2026, 9, 28, 4, 38, 0, 0, time.UTC)
	dns := target{tier: tierMeshDNS, name: "echo.aether-test.aether.internal:18081"}
	for range 5 {
		f.log(now, dns, resultTimeout, 2, context.DeadlineExceeded, noPhase)
	}
	f.log(now, dns, resultConnectionError, 0.001, nil, noPhase)
	f.log(now, target{tier: tierLiveness, name: "egress"}, resultTimeout, 2, nil, noPhase)

	lines := failLines(t, out.String())
	if len(lines) != 4 { // 2 capped mesh_dns/timeout + 1 mesh_dns/connection_error + 1 liveness/timeout
		t.Fatalf("got %d lines, want 4: %q", len(lines), out.String())
	}
	out.Reset()
	f.flush(now.Add(30 * time.Second)) // window still open: nothing yet
	if out.Len() != 0 {
		t.Fatalf("flush inside the window printed %q, want nothing", out.String())
	}
	f.flush(now.Add(time.Minute))
	summary := failLines(t, out.String())
	if len(summary) != 1 || summary[0]["suppressed"] != float64(3) || summary[0]["result"] != resultTimeout {
		t.Fatalf("summary = %v, want one mesh_dns/timeout summary with suppressed=3", summary)
	}
}

func noopMeter() metric.Meter { return noop.NewMeterProvider().Meter(telemetryServiceName) }
