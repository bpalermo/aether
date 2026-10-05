package prober

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"
)

// TestPhaseRecorderConcurrent fires the trace hooks from many goroutines while another
// snapshots, the way the transport's detached dial goroutine keeps firing them after
// Do has returned. Its value is under `--config=race`.
func TestPhaseRecorderConcurrent(t *testing.T) {
	r := newPhaseRecorder()
	tr := r.trace()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				tr.GetConn("x:1")
				tr.DNSStart(httptrace.DNSStartInfo{Host: "x"})
				tr.DNSDone(httptrace.DNSDoneInfo{})
				tr.ConnectStart("tcp", "x:1")
				tr.ConnectDone("tcp", "x:1", nil)
				tr.TLSHandshakeStart()
				tr.TLSHandshakeDone(tls.ConnectionState{}, nil)
				tr.GotConn(httptrace.GotConnInfo{Reused: true})
				tr.WroteRequest(httptrace.WroteRequestInfo{})
				tr.GotFirstResponseByte()
			}
		})
	}
	for range 200 {
		s := r.snapshot(time.Now())
		_ = s.timings()
	}
	wg.Wait()
	s := r.snapshot(time.Now())
	if got := s.phase(); got != phaseHeaders {
		t.Fatalf("phase after every hook = %q, want %q", got, phaseHeaders)
	}
	if s.connecting != 0 {
		t.Fatalf("connecting = %d after balanced Start/Done, want 0", s.connecting)
	}
}

// TestPhaseOrdering pins phase() on hand-built traces, including a failed dial (every
// step finished, no connection): the last step that ran is the one that failed.
func TestPhaseOrdering(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	for _, tc := range []struct {
		name string
		pt   phaseTimes
		want string
	}{
		{"nothing fired", phaseTimes{}, phaseNone},
		{"pool wait", phaseTimes{getConn: at(0)}, phaseConnWait},
		{"lookup in flight", phaseTimes{getConn: at(0), dnsStart: at(1)}, phaseDNS},
		{"lookup failed", phaseTimes{getConn: at(0), dnsStart: at(1), dnsDone: at(2)}, phaseDNS},
		{"connect in flight", phaseTimes{getConn: at(0), dnsStart: at(1), dnsDone: at(2), connectStart: at(3), connecting: 1}, phaseConnect},
		{"connect refused", phaseTimes{getConn: at(0), connectStart: at(3), connectDone: at(4)}, phaseConnect},
		{"handshake in flight", phaseTimes{getConn: at(0), connectStart: at(3), connectDone: at(4), tlsStart: at(5)}, phaseTLS},
		{"handshake failed", phaseTimes{getConn: at(0), connectStart: at(3), connectDone: at(4), tlsStart: at(5), tlsDone: at(6)}, phaseTLS},
		{"writing", phaseTimes{getConn: at(0), gotConn: at(7)}, phaseWrite},
		{"awaiting first byte", phaseTimes{getConn: at(0), gotConn: at(7), wroteRequest: at(8)}, phaseFirstByte},
		{"reading headers", phaseTimes{getConn: at(0), gotConn: at(7), wroteRequest: at(8), firstByte: at(9)}, phaseHeaders},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := phaseSnapshot{phaseTimes: tc.pt, end: at(100)}
			if got := s.phase(); got != tc.want {
				t.Fatalf("phase = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPhaseTimings pins the *_ms fields: finished phases are their own span, the phase
// that was running ends at the failure, and phases that never started are -1.
func TestPhaseTimings(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	at := func(ms float64) time.Time { return t0.Add(time.Duration(ms * float64(time.Millisecond))) }
	s := phaseSnapshot{
		phaseTimes: phaseTimes{
			getConn: at(0.1), dnsStart: at(0.2), dnsDone: at(12.25),
			connectStart: at(12.3), connectDone: at(13.3), gotConn: at(13.4), wroteRequest: at(13.5),
		},
		end: at(2000),
	}
	got := s.timings()
	want := phaseTimings{
		Phase: phaseFirstByte, ConnMS: 13.3, DNSMS: 12.1, ConnectMS: 1, TLSMS: -1,
		WriteMS: 0.1, TTFBMS: 1986.5,
	}
	if got != want {
		t.Fatalf("timings = %+v, want %+v", got, want)
	}
}

// TestClassifyFailure: the phase only ever turns a deadline in the dns phase into
// dns_timeout. Every other (error, phase) pair keeps classifyErr's answer, so the
// result label set is exactly what it was before #1252.
func TestClassifyFailure(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	refused := fmt.Errorf("dial tcp 10.0.0.1:18081: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	nx := fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "bogus.aether.internal", IsNotFound: true})
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		phase string
		want  string
	}{
		{"deadline in dns", expired, context.DeadlineExceeded, phaseDNS, resultDNSTimeout},
		{"deadline in connect", expired, context.DeadlineExceeded, phaseConnect, resultTimeout},
		{"deadline in tls", expired, context.DeadlineExceeded, phaseTLS, resultTimeout},
		{"deadline awaiting first byte", expired, context.DeadlineExceeded, phaseFirstByte, resultTimeout},
		{"deadline in pool wait", expired, context.DeadlineExceeded, phaseConnWait, resultTimeout},
		{"deadline with no trace", expired, context.DeadlineExceeded, phaseNone, resultTimeout},
		{"nxdomain stays nxdomain", context.Background(), nx, phaseDNS, resultDNSNXDomain},
		{"refusal in connect", context.Background(), refused, phaseConnect, resultConnectionError},
		// A non-deadline failure in the dns phase is not upgraded: only a deadline is.
		{"non-deadline in dns", context.Background(), refused, phaseDNS, resultConnectionError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.ctx, tc.err, tc.phase); got != tc.want {
				t.Fatalf("classifyFailure = %q, want %q", got, tc.want)
			}
		})
	}
}
