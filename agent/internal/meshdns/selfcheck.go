package meshdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// selfCheckInterval is how often the wedge watchdog exercises the handler. The pod-local
// readiness probe runs every 15s, so a 30s cadence reaches a NotReady verdict (3 failed
// checks) inside ~90s without adding measurable load to a node-critical daemon.
const selfCheckInterval = 30 * time.Second

// selfCheckTimeout bounds ONE self-check. A wedged handler never returns, so the check
// is run on its own goroutine and awaited: without a bound, the watchdog would wedge
// alongside the thing it is watching and never reach a verdict.
const selfCheckTimeout = 5 * time.Second

// selfCheckFailureThreshold is how many CONSECUTIVE failed checks remove the ready
// marker. More than one, so a single slow check (a scheduling stall on a saturated
// node) cannot flap the pod NotReady; small enough that a real wedge is demoted fast.
const selfCheckFailureThreshold = 3

// selfCheckLabels is the reserved two-label prefix of the watchdog's probe name; the
// mesh domain is appended to it. See selfCheckQName for why it looks like this.
const selfCheckLabels = "_selfcheck._aether"

// errSelfCheck marks a failed self-check so callers can match it with errors.Is.
var errSelfCheck = errors.New("mesh-DNS self-check")

// selfCheckQName builds the watchdog's probe name — "_selfcheck._aether.<meshDomain>."
//
// The name is chosen so the check proves THIS process answers, and nothing else:
//
//   - It is under the mesh domain, so it always takes the AUTHORITATIVE path and is
//     never forwarded. The check therefore cannot fail merely because kube-dns is down:
//     a wedge alarm that fires on an unrelated upstream outage is worse than none.
//   - It is never sent over the network. The daemon binds HOST_IP:18054 with
//     SO_REUSEPORT so a surge successor can co-bind, which means a query to that address
//     may be answered by the PEER pod — the exact false-ready trap #580 rejected a
//     tcpSocket probe over. The watchdog calls ServeDNS in-process instead, so only this
//     process can satisfy it.
//   - Both labels begin with "_", which is not a legal DNS-1123 Kubernetes namespace or
//     Service name. The record table therefore can NEVER hold this key, so the answer is
//     deterministic on a cluster with zero mesh services: NXDOMAIN once records are
//     populated, SERVFAIL while still cold. Requiring a real record to exist would have
//     made the check unusable exactly when the mesh is empty.
//
// Both accepted answers are authoritative and both prove the handler ran end to end —
// name parse, record-table lock, reply write — which is what a wedge takes out.
func (s *Server) selfCheckQName() string {
	return selfCheckLabels + "." + dns.Fqdn(s.meshDomain)
}

// selfChecker is the wedge watchdog: it periodically exercises the handler in-process
// and gates the pod-local ready marker on the verdict, so a resolver that is bound but
// blind stops reporting Ready and the DaemonSet's surge/rollout machinery can react.
// Its counters are owned by the single run goroutine and need no locking.
type selfChecker struct {
	s         *Server
	interval  time.Duration
	timeout   time.Duration
	threshold int

	fails   int
	demoted bool // this watchdog removed the ready marker
}

// newSelfChecker builds the watchdog with the production intervals.
func newSelfChecker(s *Server) *selfChecker {
	return &selfChecker{
		s:         s,
		interval:  selfCheckInterval,
		timeout:   selfCheckTimeout,
		threshold: selfCheckFailureThreshold,
	}
}

// run ticks the watchdog until the context is cancelled (Start returns).
func (c *selfChecker) run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// tick runs one self-check and applies the verdict to the ready marker.
func (c *selfChecker) tick(ctx context.Context) {
	if err := c.s.selfCheck(c.timeout); err != nil {
		c.fail(ctx, err)
		return
	}
	c.pass(ctx)
}

// fail counts a failed check and, once the consecutive-failure threshold is reached,
// removes the pod-local ready marker so the exec readiness probe flips the pod NotReady.
func (c *selfChecker) fail(ctx context.Context, err error) {
	c.s.metrics.recordSelfCheck(selfCheckFail)
	c.fails++
	if c.fails < c.threshold || c.demoted {
		c.s.log.ErrorContext(ctx, "mesh-DNS self-check failed",
			"error", err, "consecutiveFailures", c.fails, "threshold", c.threshold)
		return
	}
	c.s.log.ErrorContext(ctx, "mesh-DNS self-check failed repeatedly; removing the ready marker so this pod reports NotReady",
		"error", err, "consecutiveFailures", c.fails, "threshold", c.threshold)
	c.s.removeReadyMarker()
	c.demoted = true
}

// pass resets the failure run, stamps last-answered (so a quiet node never looks idle),
// and restores a marker this watchdog previously removed.
func (c *selfChecker) pass(ctx context.Context) {
	c.s.metrics.recordSelfCheck(selfCheckOK)
	c.s.markAnswered()
	c.fails = 0
	if !c.demoted {
		return
	}
	c.s.log.InfoContext(ctx, "mesh-DNS self-check recovered; restoring the ready marker")
	c.s.writeReadyMarker(ctx)
	c.demoted = false
}

// selfCheck exercises the DNS handler ONCE, in-process, and reports why it is unhealthy.
//
// The handler call runs on its own goroutine and is awaited with a timeout: a wedged
// handler (a stuck lock, a forward that never returns) never completes, and blocking the
// watchdog on it would defeat the entire point. On timeout that goroutine is abandoned —
// the process is wedged regardless and the pod is on its way to NotReady.
//
// The check goes through the real ServeDNS, so its queries also land in
// aether.mesh_dns.queries (two per minute per node, negligible): exercising the exact
// path real traffic takes is the point.
func (s *Server) selfCheck(timeout time.Duration) error {
	req := new(dns.Msg)
	req.SetQuestion(s.selfCheckQName(), dns.TypeA)
	w := newMemResponseWriter(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeDNS(w, req)
	}()

	select {
	case <-done:
		return validateSelfCheckReply(req, w.Msg())
	case <-time.After(timeout):
		return fmt.Errorf("%w: the handler did not reply within %s", errSelfCheck, timeout)
	}
}

// validateSelfCheckReply asserts the synthetic query got a sane authoritative reply.
// The reserved probe name can never be in the record table, so exactly two rcodes are
// healthy: NXDOMAIN (records populated) and SERVFAIL (still cold — a fresh node whose
// agent has not written a snapshot yet, which must NOT be called a wedge or the daemon
// would flap NotReady on every fresh cluster).
func validateSelfCheckReply(req, resp *dns.Msg) error {
	switch {
	case resp == nil:
		return fmt.Errorf("%w: the handler wrote no reply", errSelfCheck)
	case resp.Id != req.Id:
		return fmt.Errorf("%w: reply id %d does not match query id %d", errSelfCheck, resp.Id, req.Id)
	case !resp.Response:
		return fmt.Errorf("%w: reply is not flagged as a response", errSelfCheck)
	case !resp.Authoritative:
		return fmt.Errorf("%w: reply is not authoritative, so the probe left the mesh zone", errSelfCheck)
	case resp.Rcode != dns.RcodeNameError && resp.Rcode != dns.RcodeServerFailure:
		return fmt.Errorf("%w: unexpected rcode %s for the reserved probe name", errSelfCheck, dns.RcodeToString[resp.Rcode])
	}
	return nil
}

// memResponseWriter is an in-memory dns.ResponseWriter: it captures the reply the
// handler writes instead of putting it on a socket. The watchdog uses it to drive
// ServeDNS in-process (see selfCheckQName for why the check must never leave the
// process), and the tests use it to drive the handler directly — one implementation,
// not two.
type memResponseWriter struct {
	local  net.Addr
	remote net.Addr

	mu  sync.Mutex
	msg *dns.Msg
}

// selfCheckLocalAddr is the plausible server-side address the in-memory writer reports.
// Nothing is ever dialled through it; it exists because handlers are entitled to ask.
var selfCheckLocalAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}

// selfCheckRemoteAddr is the plausible client-side address. It is a *net.UDPAddr, so
// queryProto classifies the self-check as UDP — the transport that needs no special
// forward handling.
var selfCheckRemoteAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40404}

// newMemResponseWriter builds an in-memory writer presenting remote as the client. A nil
// remote defaults to a loopback UDP client.
func newMemResponseWriter(remote net.Addr) *memResponseWriter {
	if remote == nil {
		remote = selfCheckRemoteAddr
	}
	return &memResponseWriter{local: selfCheckLocalAddr, remote: remote}
}

// Msg returns the reply the handler wrote, or nil if it wrote none.
func (w *memResponseWriter) Msg() *dns.Msg {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.msg
}

func (w *memResponseWriter) LocalAddr() net.Addr  { return w.local }
func (w *memResponseWriter) RemoteAddr() net.Addr { return w.remote }

func (w *memResponseWriter) WriteMsg(m *dns.Msg) error {
	w.mu.Lock()
	w.msg = m
	w.mu.Unlock()
	return nil
}

// Write accepts a pre-packed reply. This resolver only ever calls WriteMsg, but the
// interface allows either, so the raw form is unpacked rather than dropped.
func (w *memResponseWriter) Write(b []byte) (int, error) {
	m := new(dns.Msg)
	if err := m.Unpack(b); err != nil {
		return 0, err
	}
	if err := w.WriteMsg(m); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *memResponseWriter) Close() error        { return nil }
func (w *memResponseWriter) TsigStatus() error   { return nil }
func (w *memResponseWriter) TsigTimersOnly(bool) {}
func (w *memResponseWriter) Hijack()             {}
