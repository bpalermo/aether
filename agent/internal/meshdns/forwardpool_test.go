package meshdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// portRecorder collects the SOURCE ports the test upstream saw queries arrive from.
// That is the whole observable of the socket pool: one source port per pooled socket,
// a new one for every dial.
type portRecorder struct {
	mu    sync.Mutex
	ports []int
}

func (p *portRecorder) add(w dns.ResponseWriter) {
	udp, ok := w.RemoteAddr().(*net.UDPAddr)
	if !ok {
		return
	}
	p.mu.Lock()
	p.ports = append(p.ports, udp.Port)
	p.mu.Unlock()
}

// distinct is the number of different source ports seen — i.e. the number of distinct
// sockets the resolver used.
func (p *portRecorder) distinct() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[int]struct{}{}
	for _, port := range p.ports {
		seen[port] = struct{}{}
	}
	return len(seen)
}

func (p *portRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ports)
}

// at returns the i-th recorded source port (negative i counts back from the end).
func (p *portRecorder) at(i int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 {
		i += len(p.ports)
	}
	return p.ports[i]
}

// echoUpstream answers every query with an empty NOERROR reply and records the source
// port it arrived from.
func echoUpstream(seen *portRecorder) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		seen.add(w)
		m := new(dns.Msg)
		m.SetReply(r)
		_ = w.WriteMsg(m)
	}
}

// forwardCounts collects a mesh-DNS counter's data points as "reason" -> value.
func forwardCounts(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s should be an int64 sum", name)
			for _, dp := range sum.DataPoints {
				v, has := dp.Attributes.Value("reason")
				require.True(t, has, "%s data point is missing the reason attribute", name)
				counts[v.Emit()] += dp.Value
			}
		}
	}
	return counts
}

// gaugeValue reads a single-data-point int64 observable gauge.
func gaugeValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			require.True(t, ok, "%s should be an int64 gauge", name)
			require.Len(t, g.DataPoints, 1)
			return g.DataPoints[0].Value
		}
	}
	t.Fatalf("gauge %s was never exported", name)
	return 0
}

const (
	dialsMetric    = "aether.mesh_dns.forward_conn_dials_total"
	recyclesMetric = "aether.mesh_dns.forward_conn_recycles_total"
	poolOpenMetric = "aether.mesh_dns.forward_conn_pool_open"
)

// TestForwardReusesPooledConn is the whole point of #674: a stream of forwarded queries
// no longer opens a socket each. Before the pool this saw 50 distinct source ports —
// 50 dials, each a dialer allocation, an fd, a connect and two epoll registrations —
// which profiling measured at 20.97% of the daemon's CPU, MORE than the exchange itself.
func TestForwardReusesPooledConn(t *testing.T) {
	seen := &portRecorder{}
	addr := startUpstream(t, echoUpstream(seen))

	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)

	for range 50 {
		resp := serve(s, query("google.com", dns.TypeA))
		require.NotNil(t, resp)
		require.Equal(t, dns.RcodeSuccess, resp.Rcode)
	}

	assert.Equal(t, 50, seen.count(), "every query reached the upstream")
	assert.LessOrEqual(t, seen.distinct(), DefaultForwardPoolSize,
		"50 sequential queries must not open more sockets than the pool holds")
}

// TestForwardRotatesSourcePorts: a pooled socket is NOT kept forever. Reuse trades away
// per-query source-port randomisation, so each slot carries two independent budgets —
// a query count and a jittered age — and retires the socket when either expires.
func TestForwardRotatesSourcePorts(t *testing.T) {
	t.Run("query budget", func(t *testing.T) {
		withMaxQueries(t, 2)
		seen := &portRecorder{}
		s := poolServer(t, startUpstream(t, echoUpstream(seen)))

		for range 20 {
			require.NotNil(t, serve(s, query("google.com", dns.TypeA)))
		}
		assert.GreaterOrEqual(t, seen.distinct(), 5,
			"a 2-query budget must rotate the source port repeatedly")
	})

	t.Run("age budget", func(t *testing.T) {
		withMaxAge(t, 10*time.Millisecond)
		seen := &portRecorder{}
		s := poolServer(t, startUpstream(t, echoUpstream(seen)))

		for range 20 {
			require.NotNil(t, serve(s, query("google.com", dns.TypeA)))
			time.Sleep(2 * time.Millisecond)
		}
		assert.GreaterOrEqual(t, seen.distinct(), 2,
			"an expired age budget must rotate the source port")
	})
}

// TestConcurrentForwardsDoNotSerialise pins the property the whole design rests on: the
// K slots are INDEPENDENT sockets taken with TryLock, so concurrent queries proceed in
// parallel exactly as they did when each dialled its own. If a slot were taken with a
// blocking Lock, four simultaneous queries would serialise behind one socket, the
// upstream would never see four at once, and the barrier below would never open.
func TestConcurrentForwardsDoNotSerialise(t *testing.T) {
	const concurrency = 4

	var (
		mu       sync.Mutex
		inFlight int
	)
	release := make(chan struct{})
	allArrived := make(chan struct{})
	seen := &portRecorder{}

	addr := startUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		seen.add(w)
		mu.Lock()
		inFlight++
		if inFlight == concurrency {
			close(allArrived)
		}
		mu.Unlock()
		<-release // hold every query open until all of them have arrived
		m := new(dns.Msg)
		m.SetReply(r)
		_ = w.WriteMsg(m)
	})

	s := poolServer(t, addr)

	done := make(chan *dns.Msg, concurrency)
	for range concurrency {
		go func() { done <- serve(s, query("google.com", dns.TypeA)) }()
	}

	select {
	case <-allArrived:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the upstream never saw 4 simultaneous queries: the forward path serialised")
	}
	close(release)

	for range concurrency {
		select {
		case resp := <-done:
			require.NotNil(t, resp)
			require.Equal(t, dns.RcodeSuccess, resp.Rcode)
		case <-time.After(2 * time.Second):
			t.Fatal("a concurrent forward never completed")
		}
	}
	assert.GreaterOrEqual(t, seen.distinct(), concurrency,
		"each concurrent query used its own socket")
}

// TestPoolFallbackWhenAllSlotsBusy: with a single slot, a second concurrent query must
// NOT wait for it. It dials its own socket — precisely the pre-#674 behaviour — which
// is what makes the pool incapable of adding tail latency by construction.
func TestPoolFallbackWhenAllSlotsBusy(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 2)
	seen := &portRecorder{}

	addr := startUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		seen.add(w)
		arrived <- struct{}{}
		<-release
		m := new(dns.Msg)
		m.SetReply(r)
		_ = w.WriteMsg(m)
	})

	s, reader := meteredServer(t, nil)
	// One slot, so the second concurrent query can only be served by the fallback.
	WithForwardPoolSize(1)(s)
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)

	done := make(chan *dns.Msg, 2)
	for range 2 {
		go func() { done <- serve(s, query("google.com", dns.TypeA)) }()
	}

	for range 2 {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("the second query blocked on the single pooled slot instead of dialling its own")
		}
	}
	close(release)

	for range 2 {
		select {
		case resp := <-done:
			require.NotNil(t, resp)
			assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
		case <-time.After(2 * time.Second):
			t.Fatal("a forward never completed")
		}
	}

	assert.Positive(t, forwardCounts(t, reader, dialsMetric)[dialReasonFallback],
		"the all-slots-busy query is counted as a fallback dial")
}

// TestPooledConnRecycledAfterUpstreamRestart: a pooled socket survives across queries,
// so it must self-heal when the upstream goes away underneath it. On a CONNECTED UDP
// socket a dead upstream surfaces as ECONNREFUSED (the kernel delivers the ICMP
// port-unreachable to the socket) — the failing exchange retires the socket, and the
// next query dials a fresh one rather than reusing a dead conn forever.
func TestPooledConnRecycledAfterUpstreamRestart(t *testing.T) {
	port := freeDNSPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	seen := &portRecorder{}

	first := serveUpstreamOn(t, addr, echoUpstream(seen))

	s, reader := meteredServer(t, nil)
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)

	require.NotNil(t, serve(s, query("google.com", dns.TypeA)))
	require.Equal(t, 1, seen.distinct(), "one pooled socket so far")
	before := seen.at(0)

	require.NoError(t, first.Shutdown())

	// The dead upstream fails fast (ICMP port-unreachable on a connected socket), well
	// inside the 2s forward timeout, and the reply is a non-authoritative SERVFAIL.
	start := time.Now()
	resp := serve(s, query("google.com", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeServerFailure, resp.Rcode)
	assert.Less(t, time.Since(start), forwardTimeout,
		"a refused upstream must not burn the full forward timeout")

	// Same address comes back: the resolver must recover on the very next query.
	serveUpstreamOn(t, addr, echoUpstream(seen))
	resp = serve(s, query("google.com", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeSuccess, resp.Rcode, "the resolver recovers without a restart")

	assert.NotEqual(t, before, seen.at(-1),
		"the dead socket was retired, not reused")
	assert.Positive(t, forwardCounts(t, reader, recyclesMetric)[recycleError],
		"the failed exchange is counted as an error recycle")
}

// TestTCPForwardStillDialsPerQuery: the TCP leg is deliberately NOT pooled. Its volume
// is negligible, and on a stream conn miekg/dns's ExchangeWithConn returns ErrId instead
// of draining a mismatched transaction ID — a late reply left on a pooled TCP socket
// would poison the next query on it.
func TestTCPForwardStillDialsPerQuery(t *testing.T) {
	t.Run("tcp-originated query", func(t *testing.T) {
		s, reader := meteredServer(t, nil)
		addr := startUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			_ = w.WriteMsg(m)
		})
		s.SetUpstreams([]string{addr})
		t.Cleanup(s.closeForwardPools)

		resp := serveFrom(s, query("google.com", dns.TypeA), &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5555})
		require.NotNil(t, resp)
		require.Equal(t, dns.RcodeSuccess, resp.Rcode)

		assert.Empty(t, forwardCounts(t, reader, dialsMetric),
			"the TCP path never touches the UDP socket pool")
	})

	t.Run("truncation retry", func(t *testing.T) {
		s, reader := meteredServer(t, nil)
		addr := startUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			if upstreamProto(w) == protoUDP {
				m.Truncated = true
			}
			_ = w.WriteMsg(m)
		})
		s.SetUpstreams([]string{addr})
		t.Cleanup(s.closeForwardPools)

		resp := serve(s, query("big.example.com", dns.TypeA))
		require.NotNil(t, resp)
		assert.False(t, resp.Truncated, "the TCP retry returned the full answer")

		// Exactly one dial: the UDP leg filling its pooled slot. The TCP retry adds none.
		assert.Equal(t, map[string]int64{dialReasonPoolFill: 1},
			forwardCounts(t, reader, dialsMetric))
	})
}

// TestPoolPrunedOnUpstreamChange: SetUpstreams is authoritative. Pools for upstreams
// that went away are closed, so a reconfiguration cannot leak connected sockets (and the
// conntrack entries pinning them to a kube-dns backend) for the life of the process.
func TestPoolPrunedOnUpstreamChange(t *testing.T) {
	s, reader := meteredServer(t, nil)
	first := startUpstream(t, echoUpstream(&portRecorder{}))
	second := startUpstream(t, echoUpstream(&portRecorder{}))
	t.Cleanup(s.closeForwardPools)

	s.SetUpstreams([]string{first})
	require.NotNil(t, serve(s, query("google.com", dns.TypeA)))
	require.Equal(t, int64(1), gaugeValue(t, reader, poolOpenMetric), "one warm socket")

	s.SetUpstreams([]string{second})
	assert.Nil(t, s.poolFor(first), "the dropped upstream's pool is gone")
	assert.NotNil(t, s.poolFor(second))
	assert.Equal(t, int64(0), gaugeValue(t, reader, poolOpenMetric),
		"the dropped upstream's socket was closed, not leaked")

	require.NotNil(t, serve(s, query("google.com", dns.TypeA)))
	assert.Equal(t, int64(1), gaugeValue(t, reader, poolOpenMetric), "the new upstream warms its own")
}

// TestForwardPoolDisabled: --forward-pool-size=0 restores the pre-#674 dial-per-query
// behaviour exactly — a fresh source port every time, and no pool instrument moves.
func TestForwardPoolDisabled(t *testing.T) {
	seen := &portRecorder{}
	addr := startUpstream(t, echoUpstream(seen))

	s, reader := meteredServer(t, nil)
	WithForwardPoolSize(0)(s)
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)

	for range 5 {
		require.NotNil(t, serve(s, query("google.com", dns.TypeA)))
	}

	assert.Equal(t, 5, seen.distinct(), "every query dialled its own socket")
	assert.Empty(t, forwardCounts(t, reader, dialsMetric), "a disabled pool never fills or falls back")
	assert.Equal(t, int64(0), gaugeValue(t, reader, poolOpenMetric))
}

// TestJitteredMaxAgeStaysInBand: the age budget is spread per slot so K sockets dialled
// together do not all expire in the same instant and hand the upstream a dial storm.
func TestJitteredMaxAgeStaysInBand(t *testing.T) {
	low := time.Duration(float64(forwardConnMaxAge) * (1 - forwardConnAgeJitter))
	high := time.Duration(float64(forwardConnMaxAge) * (1 + forwardConnAgeJitter))

	seen := map[time.Duration]struct{}{}
	for range 100 {
		d := jitteredMaxAge()
		require.GreaterOrEqual(t, d, low)
		require.LessOrEqual(t, d, high)
		seen[d] = struct{}{}
	}
	assert.Greater(t, len(seen), 1, "the age budget is actually jittered, not constant")
}

// poolServer builds a pooled resolver pointed at addr, with the pools closed on cleanup.
func poolServer(t *testing.T, addr string) *Server {
	t.Helper()
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)
	return s
}

// serveUpstreamOn binds a UDP test upstream on a FIXED address and returns the server so
// the test can shut it down mid-run (the upstream-restart case). Unlike startUpstream it
// does not pick the port, because the restart has to land on the same one.
func serveUpstreamOn(t *testing.T, addr string, h dns.HandlerFunc) *dns.Server {
	t.Helper()
	started := make(chan struct{})
	srv := &dns.Server{Addr: addr, Net: "udp", Handler: h}
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("test upstream did not bind %s in time", addr)
	}
	return srv
}

// withMaxQueries shrinks the per-socket query budget for one test and restores it.
func withMaxQueries(t *testing.T, n int) {
	t.Helper()
	prev := forwardConnMaxQueries
	forwardConnMaxQueries = n
	t.Cleanup(func() { forwardConnMaxQueries = prev })
}

// withMaxAge shrinks the per-socket age budget for one test and restores it.
func withMaxAge(t *testing.T, d time.Duration) {
	t.Helper()
	prevAge, prevJitter := forwardConnMaxAge, forwardConnAgeJitter
	forwardConnMaxAge, forwardConnAgeJitter = d, 0
	t.Cleanup(func() { forwardConnMaxAge, forwardConnAgeJitter = prevAge, prevJitter })
}
