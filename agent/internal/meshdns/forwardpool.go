package meshdns

import (
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Forward-path UDP socket pool (issue #674).
//
// miekg/dns's Client.Exchange dials, exchanges and closes on EVERY call, so before this
// each forwarded query paid a full socket setup: a dialer allocation, address
// resolution, an fd + poller registration, connect(2), two epoll_ctl(2)s and a close(2).
// Continuous profiling put net.(*Dialer).DialContext at 20.97% of the mesh-DNS daemon's
// CPU — MORE than the exchange it exists to serve (18.47%) — on a process that sits on
// the critical path of every managed pod's DNS.
//
// The pool keeps K already-connected UDP sockets per upstream and hands one to a query
// that can take it without waiting. It is deliberately the smallest thing that removes
// the dial: no ID demultiplexing, no reader goroutines, no waiter map. See
// forwardPool.exchange for why it can only ever remove work, never add latency.

// DefaultForwardPoolSize is the shipped number of pooled sockets per upstream,
// overridable per-Server with WithForwardPoolSize (0 disables pooling entirely).
//
// In-flight forwarded queries ~= forwarded QPS x upstream RTT. kube-dns is node-local-ish
// at ~0.3-1ms, so even 2000 QPS holds ~2 exchanges in flight; 8 slots makes slot
// contention unobservable while costing 8 idle fds per upstream.
const DefaultForwardPoolSize = 8

var (
	// forwardConnMaxAge and forwardConnMaxQueries are the two INDEPENDENT rotation
	// budgets a pooled socket lives under — whichever expires first retires it — and
	// forwardConnAgeJitter spreads the age budget +/-20% per slot so K sockets never
	// rotate in lockstep.
	//
	// They exist for two unrelated reasons.
	//
	// Source-port randomisation. Dial-per-query gave every single query a fresh random
	// source port, i.e. the full ~2^32 (port x transaction ID) guessing space per
	// query. A socket reused for a 30s window collapses that to ~2^19 for the queries
	// in the window. The trade is acceptable HERE, and only here, because the socket is
	// CONNECTED: the kernel drops any datagram whose source is not the upstream's exact
	// (IP, port), so an off-path attacker cannot land a spoofed answer at all — they
	// must forge the kube-dns ClusterIP as a source address INTO the node's host
	// netns, past netfilter and rp_filter, at which point cache poisoning is not the
	// interesting attack available to them. The upstream is an in-cluster resolver, not
	// an internet-facing recursor. The 30s cap plus jitter bounds the window regardless.
	//
	// Stale conntrack. The upstream is a ClusterIP. Connecting a UDP socket to it
	// creates a conntrack entry that pins DNAT to ONE kube-dns backend pod for the life
	// of the socket. When that backend rolls the entry survives, still pointing at a
	// dead pod, and datagrams are black-holed with NO ICMP — the socket only ever shows
	// a read timeout. Retire-on-error (see connSlot.exchange) catches that on the first
	// failed query, but the age budget is what guarantees a bounded self-heal even for
	// a socket that is never used again, and what keeps long-lived sockets from pinning
	// this node's whole forward load onto a single kube-dns replica.
	//
	// They are vars rather than consts ONLY so tests can shrink them; production never
	// writes them.
	forwardConnMaxAge     = 30 * time.Second
	forwardConnAgeJitter  = 0.2
	forwardConnMaxQueries = 1000
)

// errPoolClosed retires a socket that finished dialling into a pool which was closed
// underneath it (an upstream dropped by SetUpstreams, or shutdown).
var errPoolClosed = errors.New("mesh-DNS forward pool closed")

// connSlot is ONE pooled connected UDP socket plus its rotation budget.
//
// mu is taken with TryLock ONLY (see forwardPool.exchange). A blocking Lock would put
// another query's forwardTimeout-bounded upstream wait directly into THIS query's
// latency, which is exactly the regression the pool exists to avoid; a busy slot is
// skipped instead, and an all-busy pool falls back to the old dial-per-query path.
//
// conn is an atomic pointer rather than a plain field so retireConn — and therefore
// forwardPool.close — can take the socket away WITHOUT the lock. net.Conn is safe for
// concurrent method calls, so closing a socket an exchange is mid-read on simply fails
// that read, and the exchange then finds the slot already empty. left and expires are
// touched only under mu.
type connSlot struct {
	mu      sync.Mutex
	conn    atomic.Pointer[dns.Conn]
	expires time.Time
	left    int
}

// forwardPool is K independent connected UDP sockets to ONE upstream address. There is
// no shared state between slots beyond the round-robin cursor, so K queries to the same
// upstream proceed fully in parallel, exactly as they did when each dialled its own.
type forwardPool struct {
	addr   string
	slots  []*connSlot
	next   atomic.Uint64
	closed atomic.Bool
}

// newForwardPool builds an EMPTY pool: sockets are dialled lazily on first use, so a
// resolver configured with an unreachable upstream still costs nothing until a query
// actually needs it.
func newForwardPool(addr string, size int) *forwardPool {
	p := &forwardPool{addr: addr, slots: make([]*connSlot, size)}
	for i := range p.slots {
		p.slots[i] = &connSlot{}
	}
	return p
}

// exchangeUDPPooled runs one UDP exchange over a pooled socket, falling back to a fresh
// dial when every slot is busy, when the slot's socket had to be retired, or when
// pooling is disabled.
//
// It NEVER blocks on a slot. The fallback is precisely the pre-#674 behaviour, so the
// pool can only ever remove work from a query's path and never add latency to it: the
// worst case for any individual query is the dial it would have paid anyway.
func (s *Server) exchangeUDPPooled(r *dns.Msg, addr string) *dns.Msg {
	p := s.poolFor(addr)
	if p == nil { // pooling disabled, or the upstream is not in the current set
		return s.exchangeWith(s.udpClient, r, addr)
	}
	start := p.next.Add(1)
	n := uint64(len(p.slots))
	for i := range p.slots {
		sl := p.slots[(start+uint64(i))%n]
		if !sl.mu.TryLock() {
			continue // busy: try the next socket rather than serialise behind it
		}
		resp, err := sl.exchange(p, s, r)
		sl.mu.Unlock()
		if err == nil {
			return resp
		}
		// The slot retired its socket. Take one cold dial rather than walk the rest of
		// the pool: whatever broke this socket (upstream gone, conntrack black-hole)
		// most likely broke its siblings too.
		break
	}
	s.metrics.recordForwardDial(dialReasonFallback)
	return s.exchangeWith(s.udpClient, r, addr)
}

// exchange runs ONE query on this slot's socket. The caller holds sl.mu, which is what
// makes one-query-at-a-time per socket true and therefore makes ExchangeWithConn — whose
// packet-conn path drains replies until the transaction ID matches — the right primitive
// with no ID demultiplexing of our own.
//
// ANY failure retires the socket; only a successful reply keeps it. An upstream that
// went away surfaces two ways on a CONNECTED UDP socket: as ECONNREFUSED, from the ICMP
// port-unreachable the kernel delivers to the socket when the port is closed, and as a
// plain read timeout with no ICMP at all when a stale conntrack entry for the kube-dns
// ClusterIP still DNATs at a dead pod. Neither socket may be reused, and telling them
// apart buys nothing — the cost of being wrong is one extra dial.
func (sl *connSlot) exchange(p *forwardPool, s *Server, r *dns.Msg) (*dns.Msg, error) {
	c, err := sl.ensureConn(p, s)
	if err != nil {
		return nil, err
	}
	sl.left--
	resp, _, err := s.udpClient.ExchangeWithConn(r, c)
	if err != nil {
		sl.retireConn(s, recycleError)
		return nil, err
	}
	return resp, nil
}

// ensureConn returns this slot's socket, retiring and re-dialling it when the slot is
// empty or its rotation budget is spent. The caller holds sl.mu.
//
// The dial deliberately re-resolves p.addr every time: --mesh-dns-upstream accepts
// host[:port], so a hostname upstream is resolved per DIAL rather than once at
// configuration time. Do not "optimise" that into a one-time resolve — it would pin the
// resolver to an address that can change under it.
func (sl *connSlot) ensureConn(p *forwardPool, s *Server) (*dns.Conn, error) {
	if c := sl.conn.Load(); c != nil {
		if sl.left > 0 && time.Now().Before(sl.expires) {
			return c, nil
		}
		sl.retireConn(s, recycleRotated)
	}
	c, err := s.udpClient.Dial(p.addr)
	if err != nil {
		return nil, err
	}
	sl.conn.Store(c)
	sl.left = forwardConnMaxQueries
	sl.expires = time.Now().Add(jitteredMaxAge())
	s.poolConns.Add(1)
	s.metrics.recordForwardDial(dialReasonPoolFill)
	if p.closed.Load() {
		// close set closed BEFORE emptying the slots and we store BEFORE reading it, so
		// one of the two always observes the other: the socket we just opened cannot be
		// orphaned in a pool nobody will ever close again.
		sl.retireConn(s, recycleRotated)
		return nil, errPoolClosed
	}
	return c, nil
}

// retireConn takes this slot's socket away, closes it and counts the recycle.
//
// It holds NO lock: the swap is atomic and net.Conn tolerates a concurrent Close, so
// forwardPool.close can retire a slot that an exchange is currently using. It is
// idempotent for the same reason — a swap that finds no socket does nothing — so the
// racing exchange's own retire cannot double-close the fd or double-count the gauge.
func (sl *connSlot) retireConn(s *Server, reason string) {
	c := sl.conn.Swap(nil)
	if c == nil {
		return
	}
	_ = c.Close()
	s.poolConns.Add(-1)
	s.metrics.recordForwardRecycle(reason)
}

// close permanently empties the pool. Slots that are mid-exchange have their socket
// closed underneath them, which fails that exchange and sends the query down the cold
// dial fallback — one slow query at reconfiguration or shutdown, never a leaked fd.
func (p *forwardPool) close(s *Server) {
	p.closed.Store(true)
	for _, sl := range p.slots {
		sl.retireConn(s, recycleRotated)
	}
}

// jitteredMaxAge is forwardConnMaxAge spread by +/-forwardConnAgeJitter, so the K
// sockets of a pool (dialled together on the first burst of traffic) do not all expire
// in the same instant and hand a synchronised dial storm to the upstream.
func jitteredMaxAge() time.Duration {
	if forwardConnAgeJitter <= 0 {
		return forwardConnMaxAge
	}
	span := float64(forwardConnMaxAge) * forwardConnAgeJitter
	return time.Duration(float64(forwardConnMaxAge) - span + rand.Float64()*2*span)
}

// poolFor returns the pool for an upstream address, or nil when pooling is disabled or
// the address is not in the current upstream set (in which case the caller dials).
func (s *Server) poolFor(addr string) *forwardPool {
	s.poolMu.RLock()
	p := s.pools[addr]
	s.poolMu.RUnlock()
	return p
}

// setPools re-points the registry at one pool per configured upstream, keeping the pools
// of addresses that survived (so a SetUpstreams that only appends does not throw away
// warm sockets) and closing the ones whose upstream went away.
func (s *Server) setPools(addrs []string) {
	if s.poolSize <= 0 {
		return
	}
	next := make(map[string]*forwardPool, len(addrs))
	s.poolMu.Lock()
	for _, addr := range addrs {
		if p, ok := s.pools[addr]; ok {
			next[addr] = p
			continue
		}
		next[addr] = newForwardPool(addr, s.poolSize)
	}
	var dropped []*forwardPool
	for addr, p := range s.pools {
		if _, ok := next[addr]; !ok {
			dropped = append(dropped, p)
		}
	}
	s.pools = next
	s.poolMu.Unlock()
	// Closing outside the registry lock: close touches only per-slot atomics, but a
	// pool operation must never be able to stall a concurrent poolFor on the hot path.
	for _, p := range dropped {
		p.close(s)
	}
}

// closeForwardPools closes every pooled socket. Start defers it, so a resolver that
// stops serving does not leave connected UDP sockets — and their conntrack entries —
// behind.
func (s *Server) closeForwardPools() {
	s.poolMu.Lock()
	pools := s.pools
	s.pools = nil
	s.poolMu.Unlock()
	for _, p := range pools {
		p.close(s)
	}
}
