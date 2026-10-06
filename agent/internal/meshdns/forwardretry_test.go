package meshdns

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const retriesMetric = "aether.mesh_dns.forward_retries_total"

// clientBudget is how long a pod's resolver waits for a datagram before it sends it
// again (resolv.conf `timeout:1`). A re-send only helps the client if it is answered
// inside this window.
const clientBudget = time.Second

// numberedUpstream is a test upstream that numbers the datagrams it receives (1, 2, ...)
// and lets the test decide, per datagram, whether and when to answer. An answer is an A
// record 10.0.0.<n>, so the test can tell WHICH try's reply was delivered.
type numberedUpstream struct {
	seen *portRecorder
	n    atomic.Int32
	// act is called with the datagram's number before it is answered. Returning false
	// swallows the datagram (no reply). It may block to delay the reply.
	act func(n int32) bool
}

func (u *numberedUpstream) handler() dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		u.seen.add(w)
		n := u.n.Add(1)
		if !u.act(n) {
			return
		}
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.IPv4(10, 0, 0, byte(n)),
		})
		_ = w.WriteMsg(m)
	}
}

// answerIP is the A record in a reply, or "" when it has none.
func answerIP(resp *dns.Msg) string {
	if resp == nil || len(resp.Answer) != 1 {
		return ""
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		return ""
	}
	return a.A.String()
}

// counterBy collects a counter's data points as <attribute key value> -> value.
func counterBy(t *testing.T, reader *sdkmetric.ManualReader, name, key string) map[string]int64 {
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
				v, has := dp.Attributes.Value(attribute.Key(key))
				require.True(t, has, "%s data point is missing the %s attribute", name, key)
				counts[v.Emit()] += dp.Value
			}
		}
	}
	return counts
}

// newGate returns a channel that holds an upstream handler back, and an idempotent
// release for it. The caller registers release with t.Cleanup AFTER starting the
// upstream: cleanups run last-in first-out, so the handler is released before the
// upstream server's Shutdown waits for it, even when the test fails early.
func newGate() (ch chan struct{}, release func()) {
	ch = make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

// slotsIdle reports whether no pooled slot of addr's pool is held by a try.
func slotsIdle(s *Server, addr string) func() bool {
	return func() bool {
		p := s.poolFor(addr)
		if p == nil {
			return true
		}
		for _, sl := range p.slots {
			if !sl.mu.TryLock() {
				return false
			}
			sl.mu.Unlock()
		}
		return true
	}
}

// TestForwardResendsALostDatagram is issue #1254. The upstream swallows the first
// datagram and answers the second. Before, the forward waited the whole 2 s
// forwardTimeout on the first socket, past the client's 1 s budget. Now the first try
// gives up after forwardTryTimeout and ONE re-send on a fresh socket is answered, well
// inside the client's budget.
func TestForwardResendsALostDatagram(t *testing.T) {
	up := &numberedUpstream{seen: &portRecorder{}, act: func(n int32) bool { return n != 1 }}
	addr := startUpstream(t, up.handler())

	s, reader := meteredServer(t, nil)
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)

	start := time.Now()
	resp := serve(s, query("google.com", dns.TypeA))
	took := time.Since(start)

	require.NotNil(t, resp)
	require.Equal(t, dns.RcodeSuccess, resp.Rcode)
	assert.Equal(t, "10.0.0.2", answerIP(resp), "the re-send's reply is the one delivered")
	assert.GreaterOrEqual(t, took, forwardTryTimeout, "the re-send waits out the per-try timeout")
	assert.Less(t, took, clientBudget,
		"a lost datagram must be recovered inside the client's 1 s budget (took %v)", took)

	assert.Equal(t, 2, up.seen.count(), "exactly one re-send")
	assert.Equal(t, 2, up.seen.distinct(), "the re-send goes out on a fresh socket (new source port)")
	assert.Equal(t, map[string]int64{retryResend: 1}, counterBy(t, reader, retriesMetric, "result"))
	assert.Equal(t, map[string]int64{dialReasonPoolFill: 1, dialReasonRetry: 1},
		forwardCounts(t, reader, dialsMetric))

	// The first (pooled) socket never got its reply, so when the budget runs out it is
	// retired, as any failed pooled exchange is: a lost datagram and a black-holed
	// conntrack entry look the same.
	require.Eventually(t, func() bool {
		return forwardCounts(t, reader, recyclesMetric)[recycleError] == 1
	}, eventWait, 10*time.Millisecond, "the silent pooled socket is retired at the budget")
	assert.Equal(t, int64(0), gaugeValue(t, reader, poolOpenMetric))
}

// TestForwardDeadUpstreamFailsWithinBudget: an upstream that never answers (and sends no
// ICMP, like a stale conntrack entry) fails the forward at the per-upstream budget, with
// no more than ONE re-send. Before #1254 a pooled socket waited forwardTimeout and the
// cold-dial fallback waited forwardTimeout again: 4 s per upstream.
func TestForwardDeadUpstreamFailsWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		poolSize  int
		wantDials map[string]int64
	}{
		{"pooled", DefaultForwardPoolSize, map[string]int64{dialReasonPoolFill: 1, dialReasonRetry: 1}},
		{"pool disabled", 0, map[string]int64{dialReasonRetry: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &numberedUpstream{seen: &portRecorder{}, act: func(int32) bool { return false }}
			addr := startUpstream(t, up.handler())

			s, reader := meteredServer(t, nil)
			WithForwardPoolSize(tc.poolSize)(s)
			s.SetUpstreams([]string{addr})
			t.Cleanup(s.closeForwardPools)

			start := time.Now()
			resp := serve(s, query("google.com", dns.TypeA))
			took := time.Since(start)

			require.NotNil(t, resp)
			assert.Equal(t, dns.RcodeServerFailure, resp.Rcode)
			assert.GreaterOrEqual(t, took, forwardTimeout, "both tries get the whole budget")
			assert.Less(t, took, forwardTimeout+clientBudget,
				"a dead upstream must fail at the budget, not after a second full timeout (took %v)", took)

			assert.Equal(t, 2, up.seen.count(), "no more than one re-send")
			assert.Equal(t, map[string]int64{retryFailed: 1}, counterBy(t, reader, retriesMetric, "result"))
			assert.Equal(t, tc.wantDials, forwardCounts(t, reader, dialsMetric))
			if tc.poolSize > 0 {
				assert.Equal(t, int64(1), forwardCounts(t, reader, recyclesMetric)[recycleError],
					"the silent pooled socket is retired")
				assert.Equal(t, int64(0), gaugeValue(t, reader, poolOpenMetric))
			}
		})
	}
}

// TestForwardResendRaceDeliversOneReply covers both orders of the race between the
// first try's reply and the re-send's. Exactly one reply is delivered, the first to
// arrive. The pooled socket that got its reply late is HEALTHY: it read that reply to
// the end and stays in the pool, and the next query on it gets its own answer, not the
// stale one.
func TestForwardResendRaceDeliversOneReply(t *testing.T) {
	t.Run("slow first try, re-send wins", func(t *testing.T) {
		gate, release := newGate()
		up := &numberedUpstream{seen: &portRecorder{}}
		up.act = func(n int32) bool {
			if n == 1 {
				<-gate // the first try's reply comes only after the re-send was delivered
			}
			return true
		}
		addr := startUpstream(t, up.handler())
		t.Cleanup(release)

		s, reader := meteredServer(t, nil)
		WithForwardPoolSize(1)(s) // one slot: the next query must take the same socket
		s.SetUpstreams([]string{addr})
		t.Cleanup(s.closeForwardPools)

		start := time.Now()
		resp := serve(s, query("google.com", dns.TypeA))
		took := time.Since(start)
		require.NotNil(t, resp)
		assert.Equal(t, "10.0.0.2", answerIP(resp), "the re-send answered first and is delivered")
		assert.Less(t, took, clientBudget)
		assert.Equal(t, map[string]int64{retryResend: 1}, counterBy(t, reader, retriesMetric, "result"))
		pooledPort := up.seen.at(0)

		// Now the first try's late reply arrives on the pooled socket, which is still
		// listening. It must be read and dropped there.
		release()
		require.Eventually(t, slotsIdle(s, addr), eventWait, 10*time.Millisecond,
			"the first try finishes once its late reply arrives")
		assert.Empty(t, forwardCounts(t, reader, recyclesMetric),
			"a socket whose reply was only late is healthy and is not recycled")
		assert.Equal(t, int64(1), gaugeValue(t, reader, poolOpenMetric))

		// The next query takes that same pooled socket and gets ITS answer: the late
		// reply was consumed, not left in the buffer for this query to read.
		resp = serve(s, query("example.com", dns.TypeA))
		require.NotNil(t, resp)
		assert.Equal(t, "10.0.0.3", answerIP(resp))
		require.Len(t, resp.Question, 1)
		assert.Equal(t, "example.com.", resp.Question[0].Name)
		assert.Equal(t, pooledPort, up.seen.at(-1), "the kept pooled socket was reused")
		assert.Equal(t, map[string]int64{retryResend: 1}, counterBy(t, reader, retriesMetric, "result"),
			"the healthy socket answered in time: no new re-send")
	})

	t.Run("re-send goes out, first try's late reply wins", func(t *testing.T) {
		resent, markResent := newGate()
		up := &numberedUpstream{seen: &portRecorder{}}
		up.act = func(n int32) bool {
			switch n {
			case 1:
				<-resent // the first reply arrives only once the re-send is out
				return true
			case 2:
				markResent()
				return false // the re-send itself is never answered
			default:
				return true
			}
		}
		addr := startUpstream(t, up.handler())
		t.Cleanup(markResent)

		s, reader := meteredServer(t, nil)
		WithForwardPoolSize(1)(s) // one slot: the next query must take the same socket
		s.SetUpstreams([]string{addr})
		t.Cleanup(s.closeForwardPools)

		start := time.Now()
		resp := serve(s, query("google.com", dns.TypeA))
		took := time.Since(start)
		require.NotNil(t, resp)
		assert.Equal(t, "10.0.0.1", answerIP(resp), "the first try's late reply is delivered")
		assert.Less(t, took, clientBudget)
		assert.Equal(t, map[string]int64{retryLate: 1}, counterBy(t, reader, retriesMetric, "result"))

		// The pooled socket is finished (and kept) BEFORE its reply is handed over, so
		// this is deterministic without waiting.
		assert.True(t, slotsIdle(s, addr)())
		assert.Empty(t, forwardCounts(t, reader, recyclesMetric))
		assert.Equal(t, int64(1), gaugeValue(t, reader, poolOpenMetric))

		resp = serve(s, query("example.com", dns.TypeA))
		require.NotNil(t, resp)
		assert.Equal(t, "10.0.0.3", answerIP(resp))
		assert.Equal(t, up.seen.at(0), up.seen.at(-1), "the kept pooled socket was reused")
	})
}

// TestForwardRefusedUpstreamDoesNotResend: only a TIMEOUT re-sends. An upstream whose
// port is closed answers with an ICMP port-unreachable, which a connected UDP socket
// reports as ECONNREFUSED, and that fails fast exactly as before #1254: the pooled
// socket is retired and the query gets one cold fallback dial, with no re-send.
//
// The host can drop the ICMP under load (#1237). A query whose ICMP was dropped then
// times out and legitimately re-sends. So the test runs up to refusedAttempts queries
// and checks the two properties that hold either way: every FAST failure recorded no
// re-send (the re-send count equals the number of slow attempts), and at least one
// attempt failed fast.
func TestForwardRefusedUpstreamDoesNotResend(t *testing.T) {
	// A closed port nobody else can take: a UDP socket on a kernel-chosen port, CONNECTED
	// to the discard port. The kernel delivers only its peer's datagrams to it, so the
	// resolver's query finds no socket and gets the ICMP port-unreachable. It does NOT
	// set SO_REUSEPORT, so no other socket (in particular another test process's
	// reuse-port resolver, which would answer SERVFAIL itself) can share the port.
	placeholder, err := net.Dial("udp", "127.0.0.1:9")
	require.NoError(t, err)
	t.Cleanup(func() { _ = placeholder.Close() })
	addr := placeholder.LocalAddr().String()

	s, reader := meteredServer(t, nil)
	s.SetUpstreams([]string{addr})
	t.Cleanup(s.closeForwardPools)

	const refusedAttempts = 3
	var slow int64
	fast := false
	for range refusedAttempts {
		start := time.Now()
		resp := serve(s, query("google.com", dns.TypeA))
		took := time.Since(start)
		require.NotNil(t, resp)
		assert.Equal(t, dns.RcodeServerFailure, resp.Rcode)
		if took >= forwardTryTimeout {
			slow++
			continue
		}
		fast = true
		break
	}
	require.True(t, fast, "a refused upstream must fail fast at least once in %d attempts", refusedAttempts)

	var resends int64
	for _, v := range counterBy(t, reader, retriesMetric, "result") {
		resends += v
	}
	assert.Equal(t, slow, resends, "a fast (refused) failure must not re-send")
	assert.Positive(t, forwardCounts(t, reader, dialsMetric)[dialReasonFallback],
		"a refused pooled socket still gets its one cold fallback dial, as before")
}

// TestForwardTimeoutBudgetOrdering pins the relation the design rests on: the per-try
// timeout leaves the re-send time to be answered inside the client's 1 s budget, and
// both tries fit inside the per-upstream budget that bounded a forward before #1254.
func TestForwardTimeoutBudgetOrdering(t *testing.T) {
	assert.Less(t, forwardTryTimeout, clientBudget, "the re-send must go out inside the client's budget")
	assert.GreaterOrEqual(t, clientBudget-forwardTryTimeout, 300*time.Millisecond,
		"the re-send needs room for a round trip before the client gives up")
	assert.Less(t, forwardTryTimeout, forwardTimeout, "the budget must leave room for the re-send")
	assert.LessOrEqual(t, forwardTimeout, 2*time.Second,
		"the per-upstream budget must not exceed the pre-#1254 per-stage timeout")
}
