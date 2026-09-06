package meshdns

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeClock drives the lame-duck state machine without sleeping. Every channel is
// buffered and PRE-FILLED by the test, so which select arm fires is decided by the test
// and not by a race: an arm the test does not want to take is simply left empty.
type fakeClock struct {
	// times is handed out by successive Now calls (the last one repeats), so a test
	// pins the reported window duration exactly.
	times  []time.Time
	i      int
	afterC chan time.Time
	tickC  chan time.Time
	// afterD / tickD record the durations the state machine asked for, and stops
	// counts the ticker teardowns, so the test can assert the machine is not leaking
	// a ticker per window.
	afterD time.Duration
	tickD  time.Duration
	stops  int
}

func newFakeClock(times ...time.Time) *fakeClock {
	return &fakeClock{
		times:  times,
		afterC: make(chan time.Time, 1),
		tickC:  make(chan time.Time, 4),
	}
}

func (c *fakeClock) Now() time.Time {
	t := c.times[min(c.i, len(c.times)-1)]
	c.i++
	return t
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.afterD = d
	return c.afterC
}

func (c *fakeClock) Tick(d time.Duration) (<-chan time.Time, func()) {
	c.tickD = d
	return c.tickC, func() { c.stops++ }
}

// tick queues n probe opportunities.
func (c *fakeClock) tick(n int) {
	for range n {
		c.tickC <- time.Time{}
	}
}

// fire queues the deadline.
func (c *fakeClock) fire() { c.afterC <- time.Time{} }

// newTestLameDuck wires a state machine over a fake clock and a scripted detector.
// detect returns the nth element of results on the nth probe (the last one repeats).
func newTestLameDuck(clock *fakeClock, abort <-chan struct{}, results ...string) (*lameDuck, *int) {
	probes := 0
	d := &lameDuck{
		max:      10 * time.Second,
		interval: 200 * time.Millisecond,
		selfID:   "self",
		abort:    abort,
		clock:    clock,
		log:      slog.New(slog.DiscardHandler),
	}
	d.detect = func(context.Context) (string, bool) {
		peer := ""
		if len(results) > 0 {
			peer = results[min(probes, len(results)-1)]
		}
		probes++
		return peer, peer != ""
	}
	return d, &probes
}

// TestLameDuckExitsOnSuccessor is the reason the feature exists: the window ends the
// moment a DIFFERENT instance is seen answering, not when a timer says so.
func TestLameDuckExitsOnSuccessor(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := newFakeClock(t0, t0.Add(420*time.Millisecond))
	clock.tick(1)
	d, probes := newTestLameDuck(clock, nil, "peer-b")

	out := d.run(context.Background())

	assert.Equal(t, lameDuckSuccessor, out.reason)
	assert.Equal(t, "peer-b", out.peer, "the successor's stamp must be reported, for the log line and a post-mortem")
	assert.Equal(t, 420*time.Millisecond, out.duration)
	assert.Equal(t, 1, *probes)
	assert.Equal(t, 1, clock.stops, "the probe ticker must be stopped on every exit path")
}

// TestLameDuckKeepsProbingUntilSuccessorAnswers: a probe that loses the SO_REUSEPORT
// coin flip lands back on ourselves (or on nothing at all). That is "not yet", never
// "no successor" — closing on the first unhelpful probe would reintroduce the drop.
func TestLameDuckKeepsProbingUntilSuccessorAnswers(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := newFakeClock(t0, t0.Add(600*time.Millisecond))
	clock.tick(3)
	d, probes := newTestLameDuck(clock, nil, "", "", "peer-b")

	out := d.run(context.Background())

	assert.Equal(t, lameDuckSuccessor, out.reason)
	assert.Equal(t, 3, *probes, "the two inconclusive probes must not end the window")
}

// TestLameDuckExitsOnDeadline: with no successor (a scale-down, or a surge that never
// came up) the window is bounded, so the pod can never outlive its grace period and be
// SIGKILLed mid-drain.
func TestLameDuckExitsOnDeadline(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := newFakeClock(t0, t0.Add(10*time.Second))
	clock.fire()
	d, probes := newTestLameDuck(clock, nil)
	d.max = 10 * time.Second

	out := d.run(context.Background())

	assert.Equal(t, lameDuckDeadline, out.reason)
	assert.Empty(t, out.peer)
	assert.Equal(t, 10*time.Second, out.duration)
	assert.Zero(t, *probes)
	assert.Equal(t, 10*time.Second, clock.afterD, "the deadline must be armed at --lame-duck-max")
	assert.Equal(t, 200*time.Millisecond, clock.tickD)
	assert.Equal(t, 1, clock.stops)
}

// TestLameDuckAbortsOnSecondSignal: a second SIGTERM means "stop waiting". It must win
// even when a successor probe would have succeeded, so the deadline is not the only way
// out of a window an operator wants ended.
func TestLameDuckAbortsOnSecondSignal(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := newFakeClock(t0, t0.Add(50*time.Millisecond))
	abort := make(chan struct{})
	close(abort)
	d, _ := newTestLameDuck(clock, abort, "peer-b")

	out := d.run(context.Background())

	assert.Equal(t, lameDuckSignal, out.reason)
	assert.Equal(t, 50*time.Millisecond, out.duration)
	assert.Equal(t, 1, clock.stops)
}

// TestInstanceIDIsUniquePerProcess pins the property the whole detection rests on. It
// cannot be derived from the pid: predecessor and successor each run as pid 1 in their
// own container, so a pid stamp would make the successor indistinguishable from us and
// every window would run to its deadline.
func TestInstanceIDIsUniquePerProcess(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newInstanceID()
		assert.Len(t, id, 16, "64 random bits, hex")
		assert.False(t, seen[id], "instance ids must not repeat")
		seen[id] = true
	}
}

// TestServeInstanceAnswersTheStamp: the identity name is answered authoritatively (so
// it is never forwarded to kube-dns, which has no mesh zone), TXT carries the stamp,
// and every other type is NODATA so the name consistently exists.
func TestServeInstanceAnswersTheStamp(t *testing.T) {
	s := NewServerWithOptions("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler),
		WithInstanceID("stamp-a"))

	txt := serve(s, query("_instance._aether.aether.internal", dns.TypeTXT))
	require.NotNil(t, txt)
	assert.True(t, txt.Authoritative)
	assert.Equal(t, dns.RcodeSuccess, txt.Rcode)
	assert.Equal(t, "stamp-a", instanceIDFrom(txt))
	require.Len(t, txt.Answer, 1)
	assert.Zero(t, txt.Answer[0].Header().Ttl, "an identity must never be cached")

	a := serve(s, query("_instance._aether.aether.internal", dns.TypeA))
	require.NotNil(t, a)
	assert.True(t, a.Authoritative)
	assert.Equal(t, dns.RcodeSuccess, a.Rcode)
	assert.Empty(t, a.Answer)
}

// TestInstanceNameIsNotAMeshRecord: the identity name sits under the mesh domain, so it
// must be matched BEFORE the record path — otherwise it is just a permanently missing
// service and the probe never sees a stamp. A record table that somehow held the same
// key must not shadow it either.
func TestInstanceNameIsNotAMeshRecord(t *testing.T) {
	s := NewServerWithOptions("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler),
		WithInstanceID("stamp-a"))
	s.SetRecords(map[string]string{"_aether/_instance": "10.0.0.1"})

	resp := serve(s, query("_instance._aether.aether.internal", dns.TypeTXT))
	require.NotNil(t, resp)
	assert.Equal(t, "stamp-a", instanceIDFrom(resp))

	// Case-insensitivity: DNS names are, and a client may 0x20-randomise.
	upper := serve(s, query("_INSTANCE._Aether.Aether.Internal", dns.TypeTXT))
	require.NotNil(t, upper)
	assert.Equal(t, "stamp-a", instanceIDFrom(upper))

	// A neighbouring reserved name is NOT the identity name.
	other := serve(s, query("_selfcheck._aether.aether.internal", dns.TypeTXT))
	require.NotNil(t, other)
	assert.Equal(t, dns.RcodeNameError, other.Rcode)
}

// TestProbeDialAddr: a wildcard bind is not dialable, so it is probed through loopback
// — which still lands in the same reuseport group, because the group is keyed by port.
func TestProbeDialAddr(t *testing.T) {
	tests := []struct {
		name, listen, want string
	}{
		{name: "concrete host is dialled as-is", listen: "192.168.0.61:18054", want: "192.168.0.61:18054"},
		{name: "ipv4 wildcard goes to loopback", listen: "0.0.0.0:18054", want: "127.0.0.1:18054"},
		{name: "ipv6 wildcard goes to ipv6 loopback", listen: "[::]:18054", want: "[::1]:18054"},
		{name: "bare port goes to loopback", listen: ":18054", want: "127.0.0.1:18054"},
		{name: "unparseable disables probing", listen: "not-an-address", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probeDialAddr(tc.listen))
		})
	}
}

// TestInstanceIDFromIgnoresNonTXT guards the parser against a reply that is not ours.
func TestInstanceIDFromIgnoresNonTXT(t *testing.T) {
	empty := new(dns.Msg)
	assert.Empty(t, instanceIDFrom(empty))

	withA := new(dns.Msg)
	withA.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: "x.", Rrtype: dns.TypeA, Class: dns.ClassINET},
		A:   net.IPv4(10, 0, 0, 1),
	}}
	assert.Empty(t, instanceIDFrom(withA))
}

// TestLameDuckDisabledClosesImmediately: --lame-duck-max=0 is the documented kill switch
// and must restore the pre-#729 behaviour exactly — close on SIGTERM, no window.
func TestLameDuckDisabledClosesImmediately(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ready")
	s := NewServerWithOptions("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler),
		WithLameDuck(0), WithReadyMarker(marker))

	start := time.Now()
	s.runLameDuck(context.Background())

	assert.Less(t, time.Since(start), time.Second, "a disabled window must not wait")
}

// --- integration-style: two co-bound resolvers, one real handoff ----------------

// TestLameDuckHandsOffToACoBoundSuccessor is the end-to-end proof, hermetically: two
// real resolvers co-bind ONE SO_REUSEPORT UDP/TCP port on loopback, a client queries
// continuously throughout, and the predecessor is terminated mid-stream.
//
// The assertions are the two halves of #729: the predecessor exits for reason
// `successor` (it PROVED the peer was answering before closing, rather than trusting a
// readiness probe it cannot see), and not one query in the stream went unanswered while
// it did so.
func TestLameDuckHandsOffToACoBoundSuccessor(t *testing.T) {
	if testing.Short() {
		// It binds a real port and drives real datagrams; no container, but not free.
		t.Skip("skipping the co-bound handoff test in short mode")
	}
	reader := installManualReader(t)
	addr := reusablePort(t)
	domain := "aether.internal"
	records := map[string]string{"default/echo": "10.111.0.6"}

	predecessor, stopPredecessor, predDone := startCoBound(t, domain, addr, "pred", records, 5*time.Second)
	// The successor gets NO window of its own: it is not the subject here, and a
	// window would make the test's own teardown wait out its deadline.
	successor, _, _ := startCoBound(t, domain, addr, "succ", records, 0)

	// Both must be genuinely serving before the handoff is meaningful. The successor's
	// ready marker proves it BOUND; a probe that comes back with its stamp proves it
	// ANSWERS — which is exactly what the lame duck is about to look for.
	requireMarker(t, successor)
	requireStampSeen(t, predecessor, "succ")

	// A steady query stream over the shared port. Whichever socket the kernel picks,
	// the answer must be identical, so any failure here is a dropped datagram.
	stream := newQueryStream(t, addr, "echo.default."+domain)

	stopPredecessor() // the SIGTERM equivalent: it is what cancels Start's context
	select {
	case err := <-predDone:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the predecessor never returned from its lame-duck window")
	}

	failures, total := stream.stop()
	assert.Positive(t, total, "the query stream must actually have run")
	// At most the documented RESIDUAL: close() on a reuseport socket can still discard
	// a datagram the kernel queued between the last recvfrom and the close, which no
	// userspace window can prevent (eBPF sk_reuseport steering would). Anything beyond
	// that single instant is the pre-#729 behaviour coming back — before the window,
	// EVERY in-flight datagram for the whole handoff was lost, not one.
	assert.LessOrEqual(t, failures, int64(1),
		"queries must keep being answered throughout the handoff (%d sent, %d unanswered)", total, failures)

	assert.Equal(t, map[string]int64{lameDuckSuccessor: 1}, lameDuckExitCounts(t, reader),
		"the predecessor must close because it OBSERVED the successor, not because a timer expired")
	assert.NoFileExists(t, predecessor.readyMarker,
		"the lame duck must stop reporting ready the moment it starts")
}

// TestLameDuckExitsCarryThePerGenerationInstance is issue #736: the exit is a
// once-per-process event, so with only {node,reason} every generation increments a
// fresh process-local counter to 1 and Prometheus sees the same series go 1 -> 1 -> 1 —
// which is not a reset, so increase() over a multi-roll window reports ~0. The instance
// stamp makes each generation its own series, and each one a genuine 0 -> 1 rise.
//
// Both instruments are asserted: the histogram's _count is a counter too, with exactly
// the same defect when it is not per-generation.
func TestLameDuckExitsCarryThePerGenerationInstance(t *testing.T) {
	reader := installManualReader(t)

	// Two generations of the same resolver on one node: a predecessor and the successor
	// that later exits for the SAME reason. The abort channel is pre-closed so each
	// window ends at once through the real runLameDuck path — what is under test is the
	// wiring from the server's stamp onto the instruments, not the state machine.
	for _, id := range []string{"gen-a", "gen-b"} {
		abort := make(chan struct{})
		close(abort)
		s := NewServerWithOptions("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler),
			WithInstanceID(id), WithLameDuck(5*time.Second), WithLameDuckAbort(abort))
		s.runLameDuck(context.Background())
	}

	want := map[lameDuckSeries]int64{
		{reason: lameDuckSignal, instance: "gen-a"}: 1,
		{reason: lameDuckSignal, instance: "gen-b"}: 1,
	}
	assert.Equal(t, want, lameDuckExitSeries(t, reader, "aether.mesh_dns.lame_duck.exits"),
		"two generations exiting for the same reason must be two SERIES, not one series stuck at 1")
	assert.Equal(t, want, lameDuckExitSeries(t, reader, "aether.mesh_dns.lame_duck.duration"),
		"the duration histogram's _count resets per generation too, so it carries the same stamp")
}

// TestLameDuckExitInstanceIsTheServersStamp: the attribute must be THIS process's
// identity stamp — the same 16 hex characters the `lame duck started` / `successor
// observed` log lines carry as instance=, which is what makes the metric joinable to
// the logs of the generation that emitted it.
func TestLameDuckExitInstanceIsTheServersStamp(t *testing.T) {
	reader := installManualReader(t)
	abort := make(chan struct{})
	close(abort)
	// No WithInstanceID: the production path, a randomly minted stamp.
	s := NewServerWithOptions("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler),
		WithLameDuck(5*time.Second), WithLameDuckAbort(abort))
	require.Len(t, s.instanceID, 16)

	s.runLameDuck(context.Background())

	assert.Equal(t, map[lameDuckSeries]int64{
		{reason: lameDuckSignal, instance: s.instanceID}: 1,
	}, lameDuckExitSeries(t, reader, "aether.mesh_dns.lame_duck.exits"))
}

// installManualReader points the global MeterProvider at a manual reader for the test.
func installManualReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

// lameDuckSeries identifies one exported time series of the lame-duck instruments: the
// closed reason plus the per-generation instance stamp (#736).
type lameDuckSeries struct {
	reason   string
	instance string
}

// lameDuckExitCounts collects aether.mesh_dns.lame_duck.exits as reason -> count,
// summed across generations.
func lameDuckExitCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	counts := map[string]int64{}
	for series, v := range lameDuckExitSeries(t, reader, "aether.mesh_dns.lame_duck.exits") {
		counts[series.reason] += v
	}
	return counts
}

// lameDuckExitSeries collects one lame-duck instrument as {reason,instance} -> count.
// For the counter that is the summed value; for the duration histogram it is the bucket
// count — the number that carries the same per-generation reset problem.
func lameDuckExitSeries(t *testing.T, reader *sdkmetric.ManualReader, name string) map[lameDuckSeries]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[lameDuckSeries]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			collectLameDuckSeries(t, name, m.Data, out)
		}
	}
	return out
}

// collectLameDuckSeries folds one instrument's data points into out.
func collectLameDuckSeries(t *testing.T, name string, data metricdata.Aggregation, out map[lameDuckSeries]int64) {
	t.Helper()
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out[lameDuckSeriesOf(t, dp.Attributes)] += dp.Value
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			out[lameDuckSeriesOf(t, dp.Attributes)] += int64(dp.Count)
		}
	default:
		t.Fatalf("%s has unexpected aggregation %T", name, data)
	}
}

// lameDuckSeriesOf reads the identifying attributes off one exported data point. It
// also pins the reason set CLOSED: anything outside successor|deadline|signal is a new
// unbounded dimension, and it fails here rather than in Prometheus.
func lameDuckSeriesOf(t *testing.T, attrs attribute.Set) lameDuckSeries {
	t.Helper()
	reason, ok := attrs.Value("reason")
	require.True(t, ok, "every exit must carry a reason")
	require.Contains(t, []string{lameDuckSuccessor, lameDuckDeadline, lameDuckSignal}, reason.Emit(),
		"the reason attribute set must stay closed")
	instance, ok := attrs.Value("instance")
	require.True(t, ok, "every exit must carry its per-generation instance stamp (#736)")
	require.NotEmpty(t, instance.Emit())
	return lameDuckSeries{reason: reason.Emit(), instance: instance.Emit()}
}

// reusablePort picks a loopback port both servers can co-bind. It is chosen by binding
// and releasing an ephemeral one — the classic small race, and acceptable here because
// the alternative (a hard-coded port) collides with whatever else runs on the machine.
func reusablePort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := pc.LocalAddr().String()
	require.NoError(t, pc.Close())
	return addr
}

// startCoBound launches one resolver on the shared SO_REUSEPORT address and returns it
// with its cancel func and a channel carrying Start's error.
func startCoBound(t *testing.T, domain, addr, id string, records map[string]string, lameDuck time.Duration) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready")
	s := NewServerWithOptions(
		domain, addr, "", slog.New(slog.DiscardHandler),
		WithReusePort(true),
		WithReadyMarker(marker),
		WithInstanceID(id),
		WithLameDuck(lameDuck),
		WithForwardPoolSize(0),
	)
	s.SetRecords(records)

	ctx, cancel := context.WithCancel(context.Background())
	// Buffered AND closed after the send, so the cleanup below can wait for the exit
	// even when the test body has already consumed the error.
	done := make(chan error, 1)
	go func() {
		done <- s.Start(ctx)
		close(done)
	}()
	// Cleanups run LIFO, so each server is stopped and REAPED before the one launched
	// before it — no test may leave a socket in the shared reuseport group.
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s, cancel, done
}

// requireMarker waits until the server has written its pod-local ready marker, i.e. its
// listeners are bound.
func requireMarker(t *testing.T, s *Server) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := os.Stat(s.readyMarker)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the resolver never reported ready")
}

// requireStampSeen waits until a probe from s comes back carrying want's stamp, proving
// the kernel will steer to that peer AND that the peer answers.
func requireStampSeen(t *testing.T, s *Server, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		peer, ok := s.probeSuccessor(context.Background())
		return ok && peer == want
	}, 10*time.Second, 20*time.Millisecond, "never saw %q answer on the shared reuseport address", want)
}

// queryStream fires mesh A queries at the shared address until stopped, counting the
// ones that did not come back with the expected answer.
type queryStream struct {
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	failures atomic.Int64
	total    atomic.Int64
}

func newQueryStream(t *testing.T, addr, qname string) *queryStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	q := &queryStream{cancel: cancel}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		// A generous per-query timeout: the point is to catch a DROPPED datagram, not
		// to measure latency, and a loopback answer that takes 2s never happens.
		c := &dns.Client{Net: protoUDP, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
		req := new(dns.Msg)
		req.SetQuestion(dns.Fqdn(qname), dns.TypeA)
		for ctx.Err() == nil {
			resp, _, err := c.ExchangeContext(ctx, req.Copy(), addr)
			if ctx.Err() != nil {
				return
			}
			q.total.Add(1)
			if err != nil || resp == nil || len(resp.Answer) != 1 {
				q.failures.Add(1)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	t.Cleanup(cancel)
	return q
}

func (q *queryStream) stop() (failures, total int64) {
	q.cancel()
	q.wg.Wait()
	return q.failures.Load(), q.total.Load()
}
