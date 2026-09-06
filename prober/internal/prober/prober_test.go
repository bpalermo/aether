package prober

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-00$`)

func TestNotSampledTraceparent(t *testing.T) {
	tp := notSampledTraceparent()
	if !traceparentRe.MatchString(tp) {
		t.Fatalf("traceparent %q does not match the W3C not-sampled form", tp)
	}
	// The trace-id must be nonzero or Envoy rejects it and re-enables sampling.
	if strings.HasPrefix(tp, "00-00000000000000000000000000000000-") {
		t.Fatalf("traceparent has an all-zero trace-id: %q", tp)
	}
	// Two calls should differ (random ids), not a constant.
	if tp == notSampledTraceparent() {
		t.Fatalf("traceparent is not randomized: %q", tp)
	}
}

func TestClassifyErr(t *testing.T) {
	t.Run("connection refused -> connection_error", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		if got := classifyErr(context.Background(), err); got != resultConnectionError {
			t.Fatalf("got %q, want %q", got, resultConnectionError)
		}
	})
	t.Run("deadline -> timeout", func(t *testing.T) {
		if got := classifyErr(context.Background(), context.DeadlineExceeded); got != resultTimeout {
			t.Fatalf("got %q, want %q", got, resultTimeout)
		}
	})
	t.Run("wrapped DNS not-found -> dns_nxdomain", func(t *testing.T) {
		// Wrap the *net.DNSError so classifyErr must unwrap it via errors.As, mirroring
		// how the http transport surfaces a resolution failure inside an *url.Error.
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "bogus.aether.internal", IsNotFound: true})
		if got := classifyErr(context.Background(), err); got != resultDNSNXDomain {
			t.Fatalf("got %q, want %q", got, resultDNSNXDomain)
		}
	})
	t.Run("wrapped DNS timeout -> dns_timeout", func(t *testing.T) {
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "i/o timeout", Name: "slow.aether.internal", IsTimeout: true})
		if got := classifyErr(context.Background(), err); got != resultDNSTimeout {
			t.Fatalf("got %q, want %q", got, resultDNSTimeout)
		}
	})
	t.Run("wrapped generic DNS error -> dns_error", func(t *testing.T) {
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "server misbehaving", Name: "echo.aether.internal"})
		if got := classifyErr(context.Background(), err); got != resultDNSError {
			t.Fatalf("got %q, want %q", got, resultDNSError)
		}
	})
	t.Run("post-resolution dial error stays connection_error", func(t *testing.T) {
		// A dial failure with no *net.DNSError in the chain means the name resolved
		// and the connect failed — this must remain connection_error, not a dns_* class.
		err := fmt.Errorf("dial tcp 10.0.0.1:18081: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
		if got := classifyErr(context.Background(), err); got != resultConnectionError {
			t.Fatalf("got %q, want %q", got, resultConnectionError)
		}
	})

	// The subtests above all pass a live context, which is precisely why the
	// misordering in #726 survived: in production the probe's own 2s deadline has
	// ALWAYS expired by the time a 5s resolver retransmit surfaces, so ctx.Err() is
	// DeadlineExceeded on every real dns_timeout. These pin the expired-context case.
	t.Run("DNS timeout with an ALREADY-EXPIRED probe context -> dns_timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("test setup: want an expired context, got %v", ctx.Err())
		}
		// What Go hands back when the probe deadline cancels a lookup in flight.
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "i/o timeout", Name: "echo.aether-test.aether.internal", IsTimeout: true})
		if got := classifyErr(ctx, err); got != resultDNSTimeout {
			t.Fatalf("a resolution stall must be attributed to DNS even once the probe deadline has expired: got %q, want %q", got, resultDNSTimeout)
		}
	})
	t.Run("DNS nxdomain with an ALREADY-EXPIRED probe context -> dns_nxdomain", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "bogus.aether.internal", IsNotFound: true})
		if got := classifyErr(ctx, err); got != resultDNSNXDomain {
			t.Fatalf("got %q, want %q", got, resultDNSNXDomain)
		}
	})
	t.Run("connect stall with an expired probe context stays timeout", func(t *testing.T) {
		// The other side of the reorder: a deadline that lands AFTER resolution carries
		// no *net.DNSError, so it must still be a transport timeout, never a dns_* class.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		err := fmt.Errorf("dial tcp 10.107.111.199:18081: %w", &net.OpError{Op: "dial", Err: context.DeadlineExceeded})
		if got := classifyErr(ctx, err); got != resultTimeout {
			t.Fatalf("got %q, want %q", got, resultTimeout)
		}
	})
}

func TestNewMeshDNSTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MeshDNSTargets = []string{"echo.aether-test.aether.internal:18081", "echo.aether-test.aether.internal"}
	p, err := New(context.Background(), cfg, logr.Discard(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 1 liveness + 0 reachability + 2 mesh_dns.
	if len(p.targets) != 3 {
		t.Fatalf("want 3 targets (1 liveness + 2 mesh_dns), got %d", len(p.targets))
	}
	md := p.targets[1]
	if md.tier != tierMeshDNS {
		t.Fatalf("mesh_dns target tier = %q, want %q", md.tier, tierMeshDNS)
	}
	// The URL must be the REAL FQDN so the transport resolves it — not the fixed egress.
	if want := "http://echo.aether-test.aether.internal:18081/"; md.url != want {
		t.Fatalf("mesh_dns url = %q, want %q", md.url, want)
	}
	// authority MUST be empty: probe() must not override Host, or the name would never resolve.
	if md.authority != "" {
		t.Fatalf("mesh_dns authority = %q, want empty (no Host override)", md.authority)
	}
	// A target without an explicit port gets the default appended.
	if want := "http://echo.aether-test.aether.internal:18081/"; p.targets[2].url != want {
		t.Fatalf("mesh_dns default-port url = %q, want %q", p.targets[2].url, want)
	}
}

func TestNewTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReachabilityTargets = []string{"svc-1", "svc-2"}
	p, err := New(context.Background(), cfg, logr.Discard(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(p.targets) != 3 {
		t.Fatalf("want 3 targets (1 liveness + 2 reachability), got %d", len(p.targets))
	}
	if p.targets[0].tier != tierLiveness {
		t.Fatalf("first target tier = %q, want liveness", p.targets[0].tier)
	}
	if want := "http://127.0.0.1:18081/-/-/live"; p.targets[0].url != want {
		t.Fatalf("liveness url = %q, want %q", p.targets[0].url, want)
	}
	if want := "svc-1.aether.internal"; p.targets[1].authority != want {
		t.Fatalf("reachability authority = %q, want %q", p.targets[1].authority, want)
	}
}

// TestDurationHistogramBuckets pins the seconds-oriented boundaries of
// aether_probe_request_duration_seconds (#732). Without them the SDK falls back to its
// millisecond-oriented defaults (0, 5, 10, ... 10000), whose first bucket is "<= 5 s":
// a 2 ms probe and a timed-out 2 s probe then land in the same bucket and every derived
// quantile reads flat. The assertion is made against the EXPORTED data point, so it
// covers the option actually reaching the SDK, not just the package variable.
func TestDurationHistogramBuckets(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	h, err := newDurationHistogram(mp.Meter(telemetryServiceName))
	if err != nil {
		t.Fatalf("newDurationHistogram: %v", err)
	}
	h.Record(ctx, 0.002) // a healthy sub-10 ms probe
	h.Record(ctx, 2.0)   // a probe that burned the full 2 s budget

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	m := findMetric(t, &rm, "aether_probe_request_duration_seconds")
	if m.Unit != "s" {
		t.Fatalf("unit = %q, want %q", m.Unit, "s")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("aggregation = %T, want metricdata.Histogram[float64]", m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("data points = %d, want 1", len(hist.DataPoints))
	}
	dp := hist.DataPoints[0]

	want := []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 2.5, 5}
	if !slices.Equal(dp.Bounds, want) {
		t.Fatalf("exported bounds = %v, want %v", dp.Bounds, want)
	}
	// The whole point of the fix: the 2 s observation must NOT be in le=1.5 and must be
	// in le=2. Under the OTel defaults both cumulative counts would have been 2.
	if got := cumulativeAt(t, dp, 1.5); got != 1 {
		t.Fatalf("le=1.5 cumulative count = %d, want 1 (only the 2 ms probe)", got)
	}
	if got := cumulativeAt(t, dp, 2); got != 2 {
		t.Fatalf("le=2 cumulative count = %d, want 2 (the 2 s probe lands here)", got)
	}
}

// findMetric returns the named metric from a collected ResourceMetrics.
func findMetric(t *testing.T, rm *metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not exported", name)
	return metricdata.Metrics{}
}

// cumulativeAt returns the Prometheus-style cumulative count for the le=bound bucket,
// i.e. the number of observations <= bound. OTel exports per-bucket counts, so this
// sums every bucket up to and including the one that bound closes.
func cumulativeAt(t *testing.T, dp metricdata.HistogramDataPoint[float64], bound float64) uint64 {
	t.Helper()
	idx := slices.Index(dp.Bounds, bound)
	if idx < 0 {
		t.Fatalf("bound %v is not one of the exported boundaries %v", bound, dp.Bounds)
	}
	var total uint64
	for _, c := range dp.BucketCounts[:idx+1] {
		total += c
	}
	return total
}

// TestTierClients pins which transport each tier probes with (#735). The mesh_dns tier
// MUST NOT reuse connections: Go resolves a name only when it dials, so a pooled
// connection would keep the tier reporting success straight through a mesh-DNS outage —
// the exact blind spot the tier exists to close (#574). The other tiers dial a fixed
// address and have nothing to re-resolve, so they keep the pool.
func TestTierClients(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReachabilityTargets = []string{"svc-1"}
	cfg.MeshDNSTargets = []string{"echo.aether-test.aether.internal:18081"}
	p, err := New(context.Background(), cfg, logr.Discard(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if transportOf(t, p.client).DisableKeepAlives {
		t.Fatalf("shared client DisableKeepAlives = true, want false")
	}
	if !transportOf(t, p.dnsClient).DisableKeepAlives {
		t.Fatalf("mesh_dns client DisableKeepAlives = false, want true")
	}
	for _, tgt := range p.targets {
		if tgt.client == nil {
			t.Fatalf("target %s/%s has no client", tgt.tier, tgt.name)
		}
		want := tgt.tier == tierMeshDNS
		if got := transportOf(t, tgt.client).DisableKeepAlives; got != want {
			t.Fatalf("target %s/%s DisableKeepAlives = %v, want %v", tgt.tier, tgt.name, got, want)
		}
	}
}

func transportOf(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", c.Transport)
	}
	return tr
}

// TestProbeConnectionReuse is the behavioural half of TestTierClients: what the wire
// actually does over repeated probes against a server that answers WITH A BODY.
//
// The body matters. Before #735 probe() closed resp.Body without draining it, and Go's
// transport destroys rather than pools a connection whose body never reached EOF, so
// every tier was connect-per-probe — invisibly, because the liveness tier's
// direct_response carries no body and pooled anyway. On this mesh a new connection is
// not free: the node proxy's clusters set connection_pool_per_downstream_connection, so
// each one costs a fresh upstream mTLS handshake.
func TestProbeConnectionReuse(t *testing.T) {
	const probes = 4
	for _, tc := range []struct {
		name     string
		tier     string
		wantConn int
	}{
		{"keep-alive tier reuses one connection", tierLiveness, 1},
		{"mesh_dns tier dials every probe", tierMeshDNS, probes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			conns := make(map[net.Conn]struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// A non-empty body is the whole point: an empty one pools regardless.
				_, _ = io.WriteString(w, `{"echo":"body that must be drained"}`)
			}))
			srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
				if state != http.StateNew {
					return
				}
				mu.Lock()
				conns[c] = struct{}{}
				mu.Unlock()
			}
			t.Cleanup(srv.Close)

			cfg := DefaultConfig()
			cfg.Egress = strings.TrimPrefix(srv.URL, "http://")
			cfg.LivenessPath = "/"
			cfg.MeshDNSTargets = []string{cfg.Egress}
			p, err := New(context.Background(), cfg, logr.Discard(), "test")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tgt := findTarget(t, p, tc.tier)
			for range probes {
				p.probe(context.Background(), tgt)
			}

			mu.Lock()
			got := len(conns)
			mu.Unlock()
			if got != tc.wantConn {
				t.Fatalf("server saw %d connections over %d probes, want %d", got, probes, tc.wantConn)
			}
		})
	}
}

// TestDrainBody pins drainBody itself: it must leave the body at EOF, which is the
// precondition Go's transport checks before pooling a connection.
func TestDrainBody(t *testing.T) {
	body := io.NopCloser(strings.NewReader("a body the probe never reads"))
	drainBody(body)
	n, err := body.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after drainBody: read %d bytes, err %v; want 0, io.EOF", n, err)
	}
}

func findTarget(t *testing.T, p *Prober, tier string) target {
	t.Helper()
	for _, tgt := range p.targets {
		if tgt.tier == tier {
			return tgt
		}
	}
	t.Fatalf("no %s target among %d targets", tier, len(p.targets))
	return target{}
}
