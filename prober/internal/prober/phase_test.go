package prober

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// phaseTestTimeout is the per-probe budget for the phase tests: long enough that the
// phase a stall sits in is unambiguous, short enough to keep the suite fast.
const phaseTestTimeout = 300 * time.Millisecond

// stallHost is resolved only through stallingResolver. The trailing dot makes it fully
// qualified, so no resolv.conf search domain multiplies the lookups.
const stallHost = "probe-stall.example."

// stallingResolver returns a pure-Go resolver whose nameserver reads every query and
// never answers: the shape of a mesh-DNS stall (a lost datagram, a wedged resolver). The
// nameserver end is closed at test cleanup, which ends the lookups still pending on the
// transport's detached dial goroutine.
func stallingResolver(t *testing.T) *net.Resolver {
	t.Helper()
	var mu sync.Mutex
	var servers []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range servers {
			_ = c.Close()
		}
	})
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			mu.Lock()
			servers = append(servers, server)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, server) }()
			return client, nil
		},
	}
}

// stallingDNSClient is the mesh_dns tier's client (no keep-alives) with its dialer
// resolving through stallingResolver.
func stallingDNSClient(t *testing.T) *http.Client {
	t.Helper()
	c := newClient(false)
	transportOf(t, c).DialContext = (&net.Dialer{Resolver: stallingResolver(t)}).DialContext
	return c
}

// TestTransportHidesResolutionStall pins the pinned Go SDK behaviour #1252 rests on:
// net/http's Transport.getConn dials on context.WithoutCancel(req.Context()) and, when
// the request context ends first, returns context.Cause of the REQUEST context. A
// resolution stall therefore never reaches the caller as a *net.DNSError — it comes back
// as a bare context deadline, which an error-only classifier can only call `timeout`.
// If a future Go release starts surfacing the DNSError this test fails, and the phase
// classification becomes a second opinion rather than the only one.
func TestTransportHidesResolutionStall(t *testing.T) {
	c := stallingDNSClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), phaseTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+stallHost+":18081/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a stalled lookup returned a response")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		t.Fatalf("Go now surfaces the resolution stall as a *net.DNSError (%v); revisit classifyErr's comment and #1252", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if got := classifyErr(ctx, err); got != resultTimeout {
		t.Fatalf("error-only classification = %q, want %q (the misclassification #1252 fixes)", got, resultTimeout)
	}
}

// failLine returns the single AETHER_PROBE_FAIL detail line in out.
func singleFailLine(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := failLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("got %d AETHER_PROBE_FAIL lines, want 1:\n%s", len(lines), out)
	}
	return lines[0]
}

// phaseMS reads a *_ms field from a fail line; it fails the test when it is absent, so
// the stable key set is part of every phase test.
func phaseMS(t *testing.T, line map[string]any, key string) float64 {
	t.Helper()
	v, ok := line[key].(float64)
	if !ok {
		t.Fatalf("fail line has no numeric %q: %v", key, line)
	}
	return v
}

// assertRan asserts a phase ran for at least min milliseconds; assertSkipped that it
// never started (-1).
func assertRan(t *testing.T, line map[string]any, key string, minMS float64) {
	t.Helper()
	if v := phaseMS(t, line, key); v < minMS {
		t.Errorf("%s = %v, want >= %v (line %v)", key, v, minMS, line)
	}
}

func assertSkipped(t *testing.T, line map[string]any, key string) {
	t.Helper()
	if v := phaseMS(t, line, key); v != -1 {
		t.Errorf("%s = %v, want -1 (phase never started; line %v)", key, v, line)
	}
}

// interrupted is the floor for the phase a deadline cut short: most of the budget.
var interrupted = float64(phaseTestTimeout.Milliseconds()) * 0.8

// TestProbePhaseDNSStall is #1252 itself, end to end through probe(): a mesh_dns probe
// whose lookup never returns must be counted dns_timeout, not timeout. Before the fix
// this is red — it counted `timeout`, which is why "dns_* is zero" never meant DNS was
// healthy.
func TestProbePhaseDNSStall(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.MeshDNSTargets = []string{stallHost + ":18081"}
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)
	tgt := findTarget(t, p, tierMeshDNS)
	tgt.client = stallingDNSClient(t)

	p.probe(context.Background(), tgt)

	if got := resultCounts(t, reader); got[resultDNSTimeout] != 1 || len(got) != 1 {
		t.Fatalf("results = %v, want exactly {%s:1}", got, resultDNSTimeout)
	}
	line := singleFailLine(t, failOut.String())
	if line["result"] != resultDNSTimeout || line["phase"] != "dns" {
		t.Fatalf("result/phase = %v/%v, want %s/dns: %v", line["result"], line["phase"], resultDNSTimeout, line)
	}
	assertRan(t, line, "dns_ms", interrupted)
	assertRan(t, line, "conn_ms", interrupted)
	for _, k := range []string{"connect_ms", "tls_ms", "write_ms", "ttfb_ms"} {
		assertSkipped(t, line, k)
	}
	if line["reused"] != false {
		t.Errorf("reused = %v, want false", line["reused"])
	}
}

// silentListener accepts connections and never reads or answers on them: the request
// is written into the socket buffer and nothing comes back.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// TestProbePhaseFirstByte: the connection is up and the request written, and the
// deadline runs out waiting for the first response byte. That stays `timeout` (it is
// not a resolution failure) and the line names the phase.
func TestProbePhaseFirstByte(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = silentListener(t)
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	if got := resultCounts(t, reader); got[resultTimeout] != 1 || len(got) != 1 {
		t.Fatalf("results = %v, want exactly {%s:1}", got, resultTimeout)
	}
	line := singleFailLine(t, failOut.String())
	if line["result"] != resultTimeout || line["phase"] != "first_byte" {
		t.Fatalf("result/phase = %v/%v, want %s/first_byte: %v", line["result"], line["phase"], resultTimeout, line)
	}
	assertSkipped(t, line, "dns_ms") // an IP literal: nothing to resolve
	assertSkipped(t, line, "tls_ms")
	assertRan(t, line, "connect_ms", 0)
	assertRan(t, line, "conn_ms", 0)
	assertRan(t, line, "write_ms", 0)
	assertRan(t, line, "ttfb_ms", interrupted)
	if line["reused"] != false {
		t.Errorf("reused = %v, want false (a fresh dial)", line["reused"])
	}
}

// TestProbePhaseConnectBlackhole: a SYN that is never answered. The dialer's control
// hook runs after the socket exists and before connect(2) returns, i.e. inside the
// connect phase, and holds it until the dial is abandoned — what a blackholed address
// does, without depending on the sandbox's routing.
func TestProbePhaseConnectBlackhole(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = "127.0.0.1:9" // never reached: the control hook holds the connect
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)
	tgt := findTarget(t, p, tierLiveness)
	tgt.client = newClient(true)
	transportOf(t, tgt.client).DialContext = (&net.Dialer{
		ControlContext: func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}).DialContext

	p.probe(context.Background(), tgt)

	if got := resultCounts(t, reader); got[resultTimeout] != 1 || len(got) != 1 {
		t.Fatalf("results = %v, want exactly {%s:1}", got, resultTimeout)
	}
	line := singleFailLine(t, failOut.String())
	if line["result"] != resultTimeout || line["phase"] != "connect" {
		t.Fatalf("result/phase = %v/%v, want %s/connect: %v", line["result"], line["phase"], resultTimeout, line)
	}
	assertSkipped(t, line, "dns_ms")
	assertRan(t, line, "connect_ms", interrupted)
	for _, k := range []string{"tls_ms", "write_ms", "ttfb_ms"} {
		assertSkipped(t, line, k)
	}
}

// TestProbePhaseReusedConn: the keep-alive tiers reuse a pooled connection, so a stall
// on the second probe has no dns or connect phase at all, and the line must say the
// connection was reused — a reused connection that goes silent is a different story
// from a fresh dial that does.
func TestProbePhaseReusedConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := make(chan struct{}, 4)
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func(c net.Conn) {
				defer c.Close()
				// Answer exactly one request, then read the next and stay silent.
				buf := make([]byte, 4096)
				if _, err := c.Read(buf); err != nil {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
				_, _ = c.Read(buf)
				<-done
			}(c)
		}
	}()

	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = ln.Addr().String()
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)
	tgt := findTarget(t, p, tierLiveness)

	p.probe(context.Background(), tgt) // succeeds, pools the connection
	p.probe(context.Background(), tgt) // reuses it; the server never answers

	if n := len(accepted); n != 1 {
		t.Fatalf("server accepted %d connections, want 1 (the second probe must reuse)", n)
	}
	if got := resultCounts(t, reader); got[resultSuccess] != 1 || got[resultTimeout] != 1 || len(got) != 2 {
		t.Fatalf("results = %v, want {success:1, timeout:1}", got)
	}
	line := singleFailLine(t, failOut.String())
	if line["phase"] != "first_byte" || line["reused"] != true {
		t.Fatalf("phase/reused = %v/%v, want first_byte/true: %v", line["phase"], line["reused"], line)
	}
	for _, k := range []string{"dns_ms", "connect_ms", "tls_ms"} {
		assertSkipped(t, line, k)
	}
	assertRan(t, line, "ttfb_ms", interrupted)
	if v := phaseMS(t, line, "conn_ms"); v > interrupted {
		t.Errorf("conn_ms = %v for a pooled connection, want well under the budget", v)
	}
}

// TestProbePhaseHTTPError: a probe that got its answer, and the answer was a 503, ran
// every phase to completion; the line says phase=response, not a stall.
func TestProbePhaseHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = strings.TrimPrefix(srv.URL, "http://")
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	if got := resultCounts(t, reader); got[resultHTTPError] != 1 || len(got) != 1 {
		t.Fatalf("results = %v, want exactly {%s:1}", got, resultHTTPError)
	}
	line := singleFailLine(t, failOut.String())
	if line["phase"] != phaseResponse {
		t.Fatalf("phase = %v, want %s: %v", line["phase"], phaseResponse, line)
	}
	for _, k := range []string{"conn_ms", "connect_ms", "write_ms", "ttfb_ms"} {
		assertRan(t, line, k, 0)
	}
}

// TestProbePhaseFastRefusal:a refusal is a connection_error, not a deadline; the line
// still says which phase failed (connect) and that it took a few ms, not the budget.
func TestProbePhaseFastRefusal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now: connect is refused

	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = addr
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, reader := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	if got := resultCounts(t, reader); got[resultConnectionError] != 1 || len(got) != 1 {
		t.Fatalf("results = %v, want exactly {%s:1}", got, resultConnectionError)
	}
	line := singleFailLine(t, failOut.String())
	if line["phase"] != "connect" {
		t.Fatalf("phase = %v, want connect: %v", line["phase"], line)
	}
	if v := phaseMS(t, line, "connect_ms"); v < 0 || v > interrupted {
		t.Errorf("connect_ms = %v, want a fast refusal", v)
	}
}
