package prober

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The four fields of the AETHER_PROBE_FAIL line that say where a failed probe went
// (#1391): dial, remote, local, trace_id. Each test drives a real failing probe
// through probe() and reads the line that left the process.

var traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// whereStr reads one of the four as a string; it fails the test when the key is absent
// or not a string, so "every key is always present" is part of every test here.
func whereStr(t *testing.T, line map[string]any, key string) string {
	t.Helper()
	v, ok := line[key].(string)
	if !ok {
		t.Fatalf("fail line has no string %q: %v", key, line)
	}
	return v
}

// TestFailLineJoinsTheAccessLog is the property the fields exist for. The server here
// stands in for the node proxy: what it sees of the request is what the proxy's access
// log records. The line's trace_id must be the trace id of the traceparent header the
// server received, and its local address the peer address the server saw (the access
// log's downstream_remote_address), so that one failed probe finds its own rows.
func TestFailLineJoinsTheAccessLog(t *testing.T) {
	var mu sync.Mutex
	var gotTraceparent, gotPeer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotTraceparent, gotPeer = r.Header.Get("traceparent"), r.RemoteAddr
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = addr
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, _ := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	line := singleFailLine(t, failOut.String())
	mu.Lock()
	defer mu.Unlock()
	if !traceparentRe.MatchString(gotTraceparent) {
		t.Fatalf("the server received traceparent %q, want a well-formed one", gotTraceparent)
	}
	if id := whereStr(t, line, "trace_id"); id != strings.Split(gotTraceparent, "-")[1] {
		t.Errorf("trace_id = %q, the server received traceparent %q: the line must carry that header's trace id", id, gotTraceparent)
	}
	if _, ok := line["traceparent"]; ok {
		t.Errorf("the line has a traceparent key: a proxy rewrites the header's span id at every hop, so the whole header finds no row; only trace_id is printed")
	}
	if local := whereStr(t, line, "local"); local != gotPeer {
		t.Errorf("local = %q, the server saw the request come from %q", local, gotPeer)
	}
	if remote := whereStr(t, line, "remote"); remote != addr {
		t.Errorf("remote = %q, want the address the connection reached, %q", remote, addr)
	}
	if dial := whereStr(t, line, "dial"); dial != addr {
		t.Errorf("dial = %q, want the address that was dialled, %q", dial, addr)
	}
}

// TestFailLineTraceIDIsPerProbe: two failed probes, two keys. A constant would join
// every failure to every row.
func TestFailLineTraceIDIsPerProbe(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = refusedAddr(t)
	failOut := &syncBuffer{}
	p, _ := newTestProber(t, cfg, failOut)
	tgt := findTarget(t, p, tierLiveness)

	p.probe(context.Background(), tgt)
	p.probe(context.Background(), tgt)

	lines := failLines(t, failOut.String())
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	a, b := whereStr(t, lines[0], "trace_id"), whereStr(t, lines[1], "trace_id")
	if a == b || !traceIDRe.MatchString(a) || !traceIDRe.MatchString(b) {
		t.Errorf("trace ids %q and %q, want two different well-formed ones", a, b)
	}
}

// refusedAddr is an address nothing listens on.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestFailLineNoConnection: a refused connect never had a connection, so remote and
// local are empty, and dial is all there is: the address the probe tried.
func TestFailLineNoConnection(t *testing.T) {
	addr := refusedAddr(t)
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = addr
	failOut := &syncBuffer{}
	p, _ := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	line := singleFailLine(t, failOut.String())
	if line["phase"] != phaseConnect {
		t.Fatalf("phase = %v, want %s: %v", line["phase"], phaseConnect, line)
	}
	if dial := whereStr(t, line, "dial"); dial != addr {
		t.Errorf("dial = %q, want %q", dial, addr)
	}
	if remote, local := whereStr(t, line, "remote"), whereStr(t, line, "local"); remote != "" || local != "" {
		t.Errorf("remote/local = %q/%q, want both empty: the connect was refused", remote, local)
	}
	if id := whereStr(t, line, "trace_id"); !traceIDRe.MatchString(id) {
		t.Errorf("trace_id = %q, want the trace id of the header the probe would have sent", id)
	}
}

// TestFailLineSilentPeer: connected, request written, no answer (the shape of a
// mesh_dns timeout). The line has both ends of the connection.
func TestFailLineSilentPeer(t *testing.T) {
	addr := silentListener(t)
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = addr
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, _ := newTestProber(t, cfg, failOut)

	p.probe(context.Background(), findTarget(t, p, tierLiveness))

	line := singleFailLine(t, failOut.String())
	if line["phase"] != phaseFirstByte {
		t.Fatalf("phase = %v, want %s: %v", line["phase"], phaseFirstByte, line)
	}
	if remote := whereStr(t, line, "remote"); remote != addr {
		t.Errorf("remote = %q, want %q", remote, addr)
	}
	host, port, err := net.SplitHostPort(whereStr(t, line, "local"))
	if err != nil || host != "127.0.0.1" || port == "0" || net.JoinHostPort(host, port) == addr {
		t.Errorf("local = %q (%v), want this end of the connection: 127.0.0.1 and its own port", line["local"], err)
	}
}

// TestFailLineReusedConnection: a pooled connection is not dialled again, so there is
// no dial address, and remote still says where the request went.
func TestFailLineReusedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
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
	addr := ln.Addr().String()
	cfg := DefaultConfig()
	cfg.Timeout = phaseTestTimeout
	cfg.Egress = addr
	cfg.LivenessPath = "/"
	failOut := &syncBuffer{}
	p, _ := newTestProber(t, cfg, failOut)
	tgt := findTarget(t, p, tierLiveness)

	p.probe(context.Background(), tgt) // succeeds, pools the connection
	p.probe(context.Background(), tgt) // reuses it; the server never answers

	line := singleFailLine(t, failOut.String())
	if line["reused"] != true {
		t.Fatalf("reused = %v, want true: %v", line["reused"], line)
	}
	if dial := whereStr(t, line, "dial"); dial != "" {
		t.Errorf("dial = %q, want empty: a reused connection is not dialled", dial)
	}
	if remote := whereStr(t, line, "remote"); remote != addr {
		t.Errorf("remote = %q, want %q", remote, addr)
	}
}

// TestTraceID: the trace id of a traceparent, and "" for anything that is not one (the
// fixed fallback of notSampledTraceparent included, which is one).
func TestTraceID(t *testing.T) {
	for tp, want := range map[string]string{
		"00-9fa32b3befe77ea2253e8831d3472fa8-3d6debbac343aef3-00": "9fa32b3befe77ea2253e8831d3472fa8",
		"00-0000000000000000000000000000ace0-00000000000000a1-00": "0000000000000000000000000000ace0",
		"": "",
		"00-9fa32b3befe77ea2253e8831d3472fa8-3d6debbac343aef3":    "",
		"00_9fa32b3befe77ea2253e8831d3472fa8-3d6debbac343aef3-00": "",
		"00-9fa32b3befe77ea2253e8831d3472fa8_3d6debbac343aef3-00": "",
		"00-9fa32b3befe77ea2253e8831d3472fa8-3d6debbac343aef3_00": "",
	} {
		if got := traceID(tp); got != want {
			t.Errorf("traceID(%q) = %q, want %q", tp, got, want)
		}
	}
}

// TestFailLineNeverSent: a saturated probe was never sent. The four keys are there, and
// empty: there is no row to find.
func TestFailLineNeverSent(t *testing.T) {
	failOut := &syncBuffer{}
	p, _ := newTestProber(t, DefaultConfig(), failOut)

	p.record(findTarget(t, p, tierLiveness), resultSaturated, 0, errSaturated, noPhase)

	line := singleFailLine(t, failOut.String())
	for _, k := range []string{"dial", "remote", "local", "trace_id"} {
		if v := whereStr(t, line, k); v != "" {
			t.Errorf("%s = %q for a probe that was never sent, want empty", k, v)
		}
	}
}
