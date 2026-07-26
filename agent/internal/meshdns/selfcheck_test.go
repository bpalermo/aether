package meshdns

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSelfCheckQNameIsUnderTheMeshZone: the probe name is a mesh name (so it takes the
// authoritative path and is never forwarded) whose labels can never be a real
// Kubernetes namespace/Service, so no record can ever satisfy it.
func TestSelfCheckQNameIsUnderTheMeshZone(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))

	name := s.selfCheckQName()
	assert.Equal(t, "_selfcheck._aether.aether.internal.", name)
	assert.True(t, s.isMeshName(name), "the probe is under the mesh zone, so it is never forwarded")

	ns, svc, ok := s.parseMeshName(name)
	require.True(t, ok, "the probe is a well-formed <svc>.<ns> name, so it exercises the record-table lookup")
	assert.Equal(t, "_aether", ns)
	assert.Equal(t, "_selfcheck", svc)
	// Underscore labels are not legal DNS-1123 names, so this key cannot be projected.
	assert.NotContains(t, s.records, "_aether/_selfcheck")
}

// TestSelfCheckPassesOnHealthyServer: a healthy resolver passes, both once records are
// populated (NXDOMAIN for the reserved name) and while still cold (SERVFAIL) — a fresh
// node whose agent has not written a snapshot yet must not be called a wedge.
func TestSelfCheckPassesOnHealthyServer(t *testing.T) {
	cold := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	assert.NoError(t, cold.selfCheck(time.Second), "a cold resolver is healthy, just not populated")

	ready := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	ready.SetRecords(map[string]string{"default/echo": "10.111.0.6"})
	assert.NoError(t, ready.selfCheck(time.Second))

	// A pass stamps last-answered, so a quiet node never looks blind.
	newSelfChecker(ready).pass(context.Background())
	assert.Positive(t, ready.observedState().lastAnswered)
}

// TestSelfCheckDetectsWedgedHandler: a handler that cannot make progress (here: the
// record-table lock is held, exactly what a stuck reconcile or forward would do) fails
// the check instead of hanging the watchdog with it.
func TestSelfCheckDetectsWedgedHandler(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))

	s.mu.Lock() // wedge: every lookup now blocks
	err := s.selfCheck(50 * time.Millisecond)
	s.mu.Unlock()

	require.Error(t, err)
	assert.ErrorIs(t, err, errSelfCheck)
	assert.Contains(t, err.Error(), "did not reply")
}

// TestValidateSelfCheckReply: only an authoritative NXDOMAIN/SERVFAIL for the reserved
// name counts as healthy — a missing reply, a mismatched id, or a non-authoritative
// answer (i.e. the probe escaped to the forward path) is a failure.
func TestValidateSelfCheckReply(t *testing.T) {
	req := query("_selfcheck._aether.aether.internal", dns.TypeA)

	authoritative := func(rcode int) *dns.Msg {
		m := new(dns.Msg)
		m.SetRcode(req, rcode)
		m.Authoritative = true
		return m
	}

	assert.NoError(t, validateSelfCheckReply(req, authoritative(dns.RcodeNameError)))
	assert.NoError(t, validateSelfCheckReply(req, authoritative(dns.RcodeServerFailure)))

	assert.ErrorIs(t, validateSelfCheckReply(req, nil), errSelfCheck)

	forwarded := authoritative(dns.RcodeNameError)
	forwarded.Authoritative = false
	assert.ErrorIs(t, validateSelfCheckReply(req, forwarded), errSelfCheck)

	// NOERROR means something answered the reserved name, which cannot happen.
	assert.ErrorIs(t, validateSelfCheckReply(req, authoritative(dns.RcodeSuccess)), errSelfCheck)

	wrongID := authoritative(dns.RcodeNameError)
	wrongID.Id = req.Id + 1
	assert.ErrorIs(t, validateSelfCheckReply(req, wrongID), errSelfCheck)

	notAResponse := authoritative(dns.RcodeNameError)
	notAResponse.Response = false
	assert.ErrorIs(t, validateSelfCheckReply(req, notAResponse), errSelfCheck)
}

// TestWatchdogRemovesReadyMarkerAfterConsecutiveFailures: the ready marker survives
// isolated failures and is removed only once the consecutive-failure threshold is
// reached (so a single slow check cannot flap the pod), then restored on recovery.
func TestWatchdogRemovesReadyMarkerAfterConsecutiveFailures(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "mesh-dns.ready")
	s := NewServerWithOptions("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler),
		WithReadyMarker(marker))
	s.writeReadyMarker(context.Background()) // Start writes it once the listeners bind
	require.FileExists(t, marker)

	c := newSelfChecker(s)
	c.timeout = 50 * time.Millisecond
	require.Equal(t, 3, c.threshold)

	ctx := context.Background()
	s.mu.Lock() // wedge the handler for the whole failure run
	for i := 1; i < c.threshold; i++ {
		c.tick(ctx)
		assert.FileExists(t, marker, "still Ready after %d of %d failures", i, c.threshold)
	}
	c.tick(ctx)
	s.mu.Unlock()

	assert.NoFileExists(t, marker, "the marker is removed once the threshold is reached")
	assert.True(t, c.demoted)

	// A single healthy check restores it: the readiness probe flips the pod back Ready.
	c.tick(ctx)
	assert.FileExists(t, marker, "the marker is restored on recovery")
	assert.False(t, c.demoted)
	assert.Zero(t, c.fails)
}

// TestWatchdogRunStopsWithContext: the watchdog goroutine Start launches exits when
// Start's context is cancelled.
func TestWatchdogRunStopsWithContext(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	c := newSelfChecker(s)
	c.interval = 10 * time.Millisecond
	c.timeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()

	// Let at least one tick land, then stop.
	require.Eventually(t, func() bool { return s.observedState().lastAnswered > 0 },
		2*time.Second, 10*time.Millisecond, "the watchdog stamps last-answered on a quiet resolver")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog did not stop with its context")
	}
}

// TestWatchdogWithoutMarkerIsHarmless: the in-agent resolver runs with no ready marker
// (the DaemonSet is the only user), so a failing check must be a safe no-op there.
func TestWatchdogWithoutMarkerIsHarmless(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	c := newSelfChecker(s)
	c.timeout = 20 * time.Millisecond

	ctx := context.Background()
	s.mu.Lock()
	for range c.threshold {
		c.tick(ctx)
	}
	s.mu.Unlock()
	assert.True(t, c.demoted, "the verdict is still reached and logged")

	c.tick(ctx)
	assert.False(t, c.demoted)
}

// TestMemResponseWriterWrite: the shared in-memory writer also accepts a pre-packed
// reply, so it satisfies dns.ResponseWriter faithfully rather than approximately.
func TestMemResponseWriterWrite(t *testing.T) {
	m := new(dns.Msg)
	m.SetRcode(query("echo.default.aether.internal", dns.TypeA), dns.RcodeNameError)
	packed, err := m.Pack()
	require.NoError(t, err)

	w := newMemResponseWriter(nil)
	n, err := w.Write(packed)
	require.NoError(t, err)
	assert.Equal(t, len(packed), n)
	require.NotNil(t, w.Msg())
	assert.Equal(t, dns.RcodeNameError, w.Msg().Rcode)

	_, err = w.Write([]byte{0x01})
	assert.Error(t, err, "a malformed buffer is rejected, not silently captured")

	assert.NoError(t, w.Close())
	assert.NoError(t, w.TsigStatus())
	w.TsigTimersOnly(true)
	w.Hijack()
	assert.NotNil(t, w.LocalAddr())
	assert.NotNil(t, w.RemoteAddr())
}
