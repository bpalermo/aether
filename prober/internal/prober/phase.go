package prober

import (
	"crypto/tls"
	"math"
	"net/http/httptrace"
	"sync"
	"time"
)

// Per-probe phase tracing (#1252).
//
// A probe that ran out of budget used to say only `timeout`. That cannot tell a stalled
// name lookup from a blackholed SYN from an upstream that took the request and never
// answered, and the first of those could never be named at all: net/http's
// Transport.getConn dials on context.WithoutCancel(req.Context()) and, when the request
// context ends first, returns context.Cause of the REQUEST context. A lookup still in
// flight at the deadline therefore surfaces as a bare `context deadline exceeded`, never
// a *net.DNSError (go1.27.1 src/net/http/transport.go, getConn;
// TestTransportHidesResolutionStall pins it). Every probe now carries an
// httptrace.ClientTrace, and a deadline is classified by the phase it interrupted.
//
// The phases, in pipeline order, are a small closed set. They appear in the
// AETHER_PROBE_FAIL line only, never as a metric label.
const (
	// phaseNone: no trace event fired; the request never reached the transport.
	phaseNone = ""
	// phaseConnWait: waiting for a connection with no resolution, connect or TLS
	// handshake in flight (a pool or dial-slot wait).
	phaseConnWait = "conn_wait"
	phaseDNS      = "dns"
	phaseConnect  = "connect"
	phaseTLS      = "tls"
	// phaseWrite: connection in hand, request not yet fully written.
	phaseWrite = "write"
	// phaseFirstByte: request written, no response byte yet.
	phaseFirstByte = "first_byte"
	// phaseHeaders: first response byte received, headers not complete.
	phaseHeaders = "headers"
	// phaseResponse: the response headers arrived; the failure is the status
	// (http_error), not a stall.
	phaseResponse = "response"
)

// phaseRecorder collects one probe's trace timestamps. The hooks fire on the
// transport's goroutines: the DNS and connect hooks on the dial goroutine, which
// outlives the request when the deadline ends it. Every field is therefore under mu,
// and the probe reads them only through snapshot.
type phaseRecorder struct {
	mu sync.Mutex
	t  phaseTimes
}

// phaseTimes is the recorder's data: when each hook first fired (zero = never).
type phaseTimes struct {
	getConn      time.Time
	dnsStart     time.Time
	dnsDone      time.Time
	connectStart time.Time // first ConnectStart (Happy Eyeballs may start several)
	connectDone  time.Time // last ConnectDone
	connecting   int       // ConnectStart minus ConnectDone
	tlsStart     time.Time
	tlsDone      time.Time
	gotConn      time.Time
	reused       bool
	wroteRequest time.Time
	firstByte    time.Time
}

func newPhaseRecorder() *phaseRecorder {
	return &phaseRecorder{}
}

// mark records the current time into the field sel picks, keeping the first value.
func (r *phaseRecorder) mark(sel func(*phaseTimes) *time.Time) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := sel(&r.t); f.IsZero() {
		*f = now
	}
}

// trace returns the ClientTrace that feeds r.
func (r *phaseRecorder) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:  func(string) { r.mark(func(t *phaseTimes) *time.Time { return &t.getConn }) },
		DNSStart: func(httptrace.DNSStartInfo) { r.mark(func(t *phaseTimes) *time.Time { return &t.dnsStart }) },
		DNSDone:  func(httptrace.DNSDoneInfo) { r.mark(func(t *phaseTimes) *time.Time { return &t.dnsDone }) },
		ConnectStart: func(string, string) {
			now := time.Now()
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.t.connectStart.IsZero() {
				r.t.connectStart = now
			}
			r.t.connecting++
		},
		ConnectDone: func(string, string, error) {
			now := time.Now()
			r.mu.Lock()
			defer r.mu.Unlock()
			r.t.connectDone = now
			r.t.connecting--
		},
		TLSHandshakeStart: func() { r.mark(func(t *phaseTimes) *time.Time { return &t.tlsStart }) },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			r.mark(func(t *phaseTimes) *time.Time { return &t.tlsDone })
		},
		GotConn: func(info httptrace.GotConnInfo) {
			now := time.Now()
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.t.gotConn.IsZero() {
				r.t.gotConn = now
				r.t.reused = info.Reused
			}
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			r.mark(func(t *phaseTimes) *time.Time { return &t.wroteRequest })
		},
		GotFirstResponseByte: func() { r.mark(func(t *phaseTimes) *time.Time { return &t.firstByte }) },
	}
}

// phaseSnapshot is a probe's trace as it stood when the probe ended. It is a copy: the
// dial goroutine may keep firing hooks into the recorder afterwards.
type phaseSnapshot struct {
	phaseTimes
	end time.Time
}

// snapshot copies the recorder's state as of end.
func (r *phaseRecorder) snapshot(end time.Time) phaseSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return phaseSnapshot{phaseTimes: r.t, end: end}
}

// phase names the phase the probe was in when it ended: the latest one that started
// and had not finished.
func (s *phaseSnapshot) phase() string {
	switch {
	case !s.firstByte.IsZero():
		return phaseHeaders
	case !s.wroteRequest.IsZero():
		return phaseFirstByte
	case !s.gotConn.IsZero():
		return phaseWrite
	case !s.tlsStart.IsZero() && s.tlsDone.IsZero():
		return phaseTLS
	case s.connecting > 0:
		return phaseConnect
	case !s.dnsStart.IsZero() && s.dnsDone.IsZero():
		return phaseDNS
	case !s.tlsStart.IsZero():
		// Every dial step has finished, so the dial failed: the last step that ran is
		// the one that failed (a handshake error, a refused connect, NXDOMAIN).
		return phaseTLS
	case !s.connectStart.IsZero():
		return phaseConnect
	case !s.dnsStart.IsZero():
		return phaseDNS
	case !s.getConn.IsZero():
		return phaseConnWait
	default:
		return phaseNone
	}
}

// span returns the milliseconds from from to to, at 0.1 ms resolution. A phase still
// running (to zero, or after the end) runs to s.end; one that never started is -1.
func (s *phaseSnapshot) span(from, to time.Time) float64 {
	if from.IsZero() {
		return -1
	}
	if to.IsZero() || to.After(s.end) {
		to = s.end
	}
	return math.Round(float64(to.Sub(from).Microseconds())/100) / 10
}

// phaseTimings are the per-phase fields of the AETHER_PROBE_FAIL line. Every field is
// always present: -1 means the phase never started, and the phase the probe ended in
// is measured up to the failure.
type phaseTimings struct {
	Phase     string  `json:"phase"`
	Reused    bool    `json:"reused"`
	ConnMS    float64 `json:"conn_ms"`    // GetConn to GotConn: all it took to get a connection
	DNSMS     float64 `json:"dns_ms"`     // DNSStart to DNSDone
	ConnectMS float64 `json:"connect_ms"` // first ConnectStart to last ConnectDone
	TLSMS     float64 `json:"tls_ms"`     // TLSHandshakeStart to TLSHandshakeDone
	WriteMS   float64 `json:"write_ms"`   // GotConn to WroteRequest
	TTFBMS    float64 `json:"ttfb_ms"`    // WroteRequest to GotFirstResponseByte
}

// timings renders s for the failure line.
func (s *phaseSnapshot) timings() phaseTimings {
	connectEnd := s.connectDone
	if s.connecting > 0 {
		connectEnd = time.Time{} // still connecting: runs to the end
	}
	return phaseTimings{
		Phase:     s.phase(),
		Reused:    s.reused,
		ConnMS:    s.span(s.getConn, s.gotConn),
		DNSMS:     s.span(s.dnsStart, s.dnsDone),
		ConnectMS: s.span(s.connectStart, connectEnd),
		TLSMS:     s.span(s.tlsStart, s.tlsDone),
		WriteMS:   s.span(s.gotConn, s.wroteRequest),
		TTFBMS:    s.span(s.wroteRequest, s.firstByte),
	}
}

// noPhase is the timings of a failure that never reached the transport: a saturated
// probe, or a request that could not be built.
var noPhase = phaseTimings{Phase: phaseNone, ConnMS: -1, DNSMS: -1, ConnectMS: -1, TLSMS: -1, WriteMS: -1, TTFBMS: -1}
