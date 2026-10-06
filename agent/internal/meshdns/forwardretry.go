package meshdns

import (
	"errors"
	"net"
	"time"

	"github.com/miekg/dns"
)

// One re-send inside the client's budget (issue #1254).
//
// A pod's resolver gives up on a datagram after one second and sends it again
// (resolv.conf `timeout:1 attempts:3`). The forward path used to wait the whole
// forwardTimeout (2 s) on the first socket before trying anything else, and then made a
// cold dial with another 2 s wait. With the single shipped upstream (the kube-dns
// ClusterIP), one lost datagram therefore cost at least 2 s, which is two of the
// client's retry windows.
//
// Now a UDP forward to one upstream is at most two tries inside one forwardTimeout
// budget:
//
//   - The first try waits forwardTryTimeout (600 ms) for its reply.
//   - If that read TIMES OUT, the query is sent ONE more time on a freshly dialled
//     socket. A new socket has a new source port, so it gets its own conntrack entry,
//     which can land on a different kube-dns backend than a stale entry that is
//     black-holing the first socket. The re-send waits until the budget runs out.
//   - The first socket KEEPS LISTENING until the budget runs out. A reply that is only
//     slow, not lost, is still used. The two tries race and the first reply wins; the
//     other is read and dropped (see resend).
//
// Only a timeout re-sends. A first try that fails FAST (ECONNREFUSED from an ICMP
// port-unreachable, a write error, an unparseable reply) behaves as before: a pooled
// socket is retired and the query gets one cold dial, and a throwaway socket fails the
// upstream at once.
//
// The numbers:
//
//   - A lost datagram now costs forwardTryTimeout plus one RTT (about 0.6 s), inside the
//     client's 1 s window, instead of 2 s plus a dial.
//   - A dead or black-holed upstream fails at the budget: forwardTimeout (2 s) after the
//     forward started, with exactly one re-send. Before, a pooled socket waited 2 s and
//     then the cold-dial fallback waited 2 s more, so 4 s per upstream. The budget is per
//     upstream, so a query with N upstreams that are all dead still takes N x 2 s.
//   - Any reply that arrives within 2 s is still accepted, as before. So the change can
//     only make an answer arrive sooner. It never drops one that used to arrive.
//
// The client's own retry is unaffected. Each client re-send is a new query here, with
// its own budget, and a client that sent again after 1 s can still take the original
// query's answer: it is the same transaction ID.
//
// forwardTryTimeout is a const for the same reason forwardTimeout is: it is a protocol
// bound tied to the client's resolv.conf timeout, not a per-deployment knob.
const forwardTryTimeout = 600 * time.Millisecond

// tryResult is what one try of a raced forward reports back to resend.
type tryResult struct {
	resp *dns.Msg
	err  error
	// winner is the forward_retries_total result to record if this reply is the one
	// delivered (retryResend or retryLate).
	winner string
}

// exchangeUDP relays r to ONE upstream over UDP, returning nil when that upstream failed
// within the budget (the caller then tries the next one). See the comment on
// forwardTryTimeout for the try/re-send scheme.
func (s *Server) exchangeUDP(r *dns.Msg, addr string) *dns.Msg {
	start := time.Now()
	budget := start.Add(forwardTimeout)

	first, err := s.firstTry(addr, budget)
	if err != nil {
		return nil
	}
	resp, err := roundTrip(first.conn, r, earliest(start.Add(forwardTryTimeout), budget))
	if err == nil {
		first.finish(s, nil)
		return resp
	}
	if isTimeout(err) {
		if time.Now().Before(budget) {
			return s.resend(r, addr, first, budget)
		}
		first.finish(s, err)
		return nil
	}

	// A fast failure (ECONNREFUSED, a write error, an unparseable reply): the same as
	// before #1254. A pooled socket is retired and the query gets ONE cold dial,
	// because a pooled socket can be the only broken one (a stale conntrack entry from
	// before an upstream restart). A throwaway socket was already that cold dial.
	first.finish(s, err)
	if first.slot == nil {
		return nil
	}
	s.metrics.recordForwardDial(dialReasonFallback)
	c, err := dialUDP(addr, budget)
	if err != nil {
		return nil
	}
	resp, err = roundTrip(c, r, budget)
	_ = c.Close()
	if err != nil {
		return nil
	}
	return resp
}

// firstTry returns the socket for a forward's first try: a pooled one when a slot can be
// taken without waiting, otherwise a fresh dial (counted as a fallback dial when a pool
// exists, and not counted at all when pooling is disabled, which is dial-per-query by
// definition).
func (s *Server) firstTry(addr string, budget time.Time) (udpTry, error) {
	p := s.poolFor(addr)
	if p != nil {
		if t, ok := p.take(s); ok {
			return t, nil
		}
		s.metrics.recordForwardDial(dialReasonFallback)
	}
	c, err := dialUDP(addr, budget)
	if err != nil {
		return udpTry{}, err
	}
	return udpTry{conn: c}, nil
}

// resend runs the race after a first try timed out: ONE re-send on a freshly dialled
// socket, while the first socket keeps listening for its own late reply. Both run until
// budget. The first reply wins and is returned; resend does not wait for the other try.
//
// The losing try finishes on its own goroutine, never later than budget:
//
//   - A late reply on the first socket is read and dropped by that socket's read loop,
//     so it can never be left in the socket's buffer for the next query that takes the
//     pooled slot. The socket answered, so it is healthy and goes back into the pool
//     instead of being recycled.
//   - A first socket that gets nothing by budget is retired, as any failed pooled
//     exchange is (a black-holed conntrack entry looks exactly like this).
//   - The re-send's socket is a throwaway and is always closed.
//
// So exactly one reply is ever delivered to the client.
func (s *Server) resend(r *dns.Msg, addr string, first udpTry, budget time.Time) *dns.Msg {
	// The re-send packs its OWN copy. Packing a message that carries an EDNS0 OPT RR
	// writes the extended-rcode bits into it (miekg/dns PackBuffer), and once resend
	// returns, r is the caller's again: a truncated reply re-packs r for the TCP
	// re-fetch. The first try's listener needs only the transaction ID.
	again := r.Copy()
	id := r.Id
	results := make(chan tryResult, 2) // buffered: the loser never blocks on a gone reader

	go func() {
		resp, err := awaitReply(first.conn, id, budget)
		first.finish(s, err)
		results <- tryResult{resp: resp, err: err, winner: retryLate}
	}()
	go func() {
		s.metrics.recordForwardDial(dialReasonRetry)
		c, err := dialUDP(addr, budget)
		if err != nil {
			results <- tryResult{err: err}
			return
		}
		resp, err := roundTrip(c, again, budget)
		_ = c.Close()
		results <- tryResult{resp: resp, err: err, winner: retryResend}
	}()

	for range 2 {
		res := <-results
		if res.err == nil {
			s.metrics.recordForwardRetry(res.winner)
			return res.resp
		}
	}
	s.metrics.recordForwardRetry(retryFailed)
	return nil
}

// roundTrip writes r on a connected UDP socket and waits until deadline for the reply
// with r's transaction ID. It is miekg/dns's ExchangeWithConn for a packet conn, with an
// absolute deadline instead of the client's per-stage timeouts. That lets the two tries
// of one forward share a single budget without a context (and its timer) on every
// forwarded query.
func roundTrip(co *dns.Conn, r *dns.Msg, deadline time.Time) (*dns.Msg, error) {
	// Read a reply as large as the client said it can take, as ExchangeWithConn does.
	if opt := r.IsEdns0(); opt != nil && opt.UDPSize() >= dns.MinMsgSize {
		co.UDPSize = opt.UDPSize()
	}
	if err := co.SetWriteDeadline(deadline); err != nil {
		return nil, err
	}
	if err := co.WriteMsg(r); err != nil {
		return nil, err
	}
	return awaitReply(co, r.Id, deadline)
}

// awaitReply reads from co until the reply carrying transaction ID id arrives, or until
// deadline. A reply with any other ID is dropped: it answers an earlier query on this
// socket that already gave up.
func awaitReply(co *dns.Conn, id uint16, deadline time.Time) (*dns.Msg, error) {
	if err := co.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	for {
		m, err := co.ReadMsg()
		if err != nil {
			return nil, err
		}
		if m.Id == id {
			return m, nil
		}
	}
}

// dialUDP opens a throwaway connected UDP socket to addr. deadline bounds the dial,
// which only matters for a hostname upstream (--mesh-dns-upstream accepts host[:port]
// and is resolved per dial; see ensureConn): connecting a UDP socket sends nothing.
func dialUDP(addr string, deadline time.Time) (*dns.Conn, error) {
	d := net.Dialer{Deadline: deadline}
	c, err := d.Dial(protoUDP, addr)
	if err != nil {
		return nil, err
	}
	return &dns.Conn{Conn: c}, nil
}

// isTimeout reports whether err is a read or write deadline expiring. It is the ONLY
// failure that triggers a re-send: a timeout is what a lost datagram looks like.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// earliest returns the earlier of two instants.
func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
