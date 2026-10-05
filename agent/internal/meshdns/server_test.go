package meshdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookup(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:18054", "", slog.New(slog.DiscardHandler))
	// 020 Part 1: records keyed by "<ns>/<svc>".
	s.SetRecords(map[string]string{"team-a/svc-1": "10.111.0.5", "default/echo": "10.111.0.6"})

	// <svc>.<ns>.<meshDomain> -> its record; case-insensitive; trailing dot tolerated.
	assertHit(t, s, "svc-1.team-a.aether.internal.", "10.111.0.5")
	assertHit(t, s, "SVC-1.TEAM-A.AETHER.INTERNAL", "10.111.0.5")
	assertHit(t, s, "echo.default.aether.internal.", "10.111.0.6")

	// Mesh names that miss the record table are authoritative (NOT forwarded);
	// lookup returns "" but the name still parses as a mesh name.
	assertMeshMiss(t, s, "unknown.team-a.aether.internal.")
	assertMeshMiss(t, s, "svc-1.aether-test.aether.internal.", "wrong namespace")

	// The resolver owns the WHOLE zone: any name under .meshDomain is a mesh name
	// (answered authoritatively, never forwarded) even when malformed.
	assert.True(t, s.isMeshName("svc-1.aether.internal."), "single label under the zone is still ours")
	assert.True(t, s.isMeshName("a.b.c.aether.internal."), "three labels under the zone are still ours")

	// Not under the mesh domain -> forwarded (isMeshName false). The bare apex has no
	// leading label and is not matched (harmless: nobody resolves it as a service).
	assert.False(t, s.isMeshName("svc-1.aether-test.svc.cluster.local."), "cluster.local is not a mesh name")
	assert.False(t, s.isMeshName("aether.internal."), "the bare mesh domain apex")
	assert.False(t, s.isMeshName("google.com."), "external name")
}

func assertHit(t *testing.T, s *Server, qname, want string) {
	t.Helper()
	assert.True(t, s.isMeshName(qname), "%s should be a mesh name", qname)
	ip, ready := s.lookup(qname)
	assert.Equal(t, want, ip)
	assert.True(t, ready, "records populated -> ready")
}

func assertMeshMiss(t *testing.T, s *Server, qname string, msg ...string) {
	t.Helper()
	assert.True(t, s.isMeshName(qname), "%s should be a mesh name", qname)
	ip, _ := s.lookup(qname)
	assert.Empty(t, ip, msg)
}

// TestWarmStartLoadsSnapshot: a NewServer with an existing snapshot file warm-starts
// its record table (and flips ready) before any SetRecords/reconcile.
func TestWarmStartLoadsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	data, err := json.Marshal(map[string]string{"team-a/svc-1": "10.111.0.5"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))

	ip, ready := s.lookup("svc-1.team-a.aether.internal.")
	assert.Equal(t, "10.111.0.5", ip, "warm-started from snapshot")
	assert.True(t, ready, "a non-empty warm load flips ready")
}

// TestWarmStartMissingSnapshotIsCold: no snapshot file -> cold (not ready), empty records.
func TestWarmStartMissingSnapshotIsCold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))

	ip, ready := s.lookup("svc-1.team-a.aether.internal.")
	assert.Empty(t, ip)
	assert.False(t, ready, "no snapshot -> cold")
}

// TestSetRecordsPersistsSnapshot: SetRecords writes the table to disk so a subsequent
// process can warm-start from it.
func TestSetRecordsPersistsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	s.SetRecords(map[string]string{"default/echo": "10.111.0.6"})

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got Snapshot
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, map[string]string{"default/echo": "10.111.0.6"}, got.Records)
	assert.Equal(t, uint64(1), got.Generation)
	assert.Positive(t, got.WrittenAt)

	// A second server warm-starts from what the first persisted.
	s2 := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	ip, ready := s2.lookup("echo.default.aether.internal.")
	assert.Equal(t, "10.111.0.6", ip)
	assert.True(t, ready)
}

// TestSnapshotRoundTrip: WriteSnapshot then ReadSnapshot returns the same table via
// the versioned envelope (records + generation + a fresh writtenAt), and a missing
// file surfaces os.ErrNotExist so callers can treat it as cold.
func TestSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "records.json") // dir does not exist yet
	want := map[string]string{"team-a/svc-1": "10.111.0.5", "default/echo": "10.111.0.6"}
	before := time.Now().Unix()
	require.NoError(t, WriteSnapshot(path, want, 7), "WriteSnapshot creates the parent dir")

	got, err := ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, want, got.Records)
	assert.Equal(t, uint64(7), got.Generation, "the caller's generation round-trips")
	assert.GreaterOrEqual(t, got.WrittenAt, before, "writtenAt is stamped at write time")
	assert.LessOrEqual(t, got.WrittenAt, time.Now().Unix())

	// The on-disk form really is the envelope, not a bare map.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.Contains(t, envelope, "records")
	assert.Contains(t, envelope, "writtenAt")
	assert.Contains(t, envelope, "generation")

	_, err = ReadSnapshot(filepath.Join(t.TempDir(), "missing.json"))
	assert.ErrorIs(t, err, os.ErrNotExist, "a missing snapshot is distinguishable as not-exist")
}

// TestReadSnapshotLegacyBareMap: a snapshot written by a pre-#586 agent is a BARE
// record map with no envelope. ReadSnapshot must still accept it (an in-place upgrade
// must never start the resolver cold) and back-fill writtenAt from the file mtime, so
// the freshness gauge degrades gracefully instead of reading zero.
func TestReadSnapshotLegacyBareMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	want := map[string]string{"team-a/svc-1": "10.111.0.5", "default/echo": "10.111.0.6"}
	data, err := json.Marshal(want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	mtime := time.Now().Add(-90 * time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, mtime, mtime))

	got, err := ReadSnapshot(path)
	require.NoError(t, err, "the legacy bare-map form is still readable")
	assert.Equal(t, want, got.Records)
	assert.Zero(t, got.Generation, "a legacy snapshot carries no generation")
	assert.Equal(t, mtime.Unix(), got.WrittenAt, "writtenAt falls back to the file mtime")
}

// TestReadSnapshotLegacyEnvelopeShapedKeys: a legacy bare map whose service keys
// happen to collide with envelope field names still decodes as the legacy form.
func TestReadSnapshotLegacyEnvelopeShapedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	want := map[string]string{"generation/svc": "10.111.0.5", "writtenAt/svc": "10.111.0.6"}
	data, err := json.Marshal(want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	got, err := ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, want, got.Records)
}

// TestReadSnapshotCorrupt: a file that exists but does not decode is reported as a
// parse error (distinct from a missing file and from an I/O failure), which is what
// the snapshot_reloads_total result label keys off.
func TestReadSnapshotCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	_, err := ReadSnapshot(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotParse)
	assert.NotErrorIs(t, err, os.ErrNotExist)
}

// TestReloadResultClassification: every ReadSnapshot outcome maps to the expected
// snapshot_reloads_total result label.
func TestReloadResultClassification(t *testing.T) {
	dir := t.TempDir()

	ok := filepath.Join(dir, "ok.json")
	require.NoError(t, WriteSnapshot(ok, map[string]string{"default/echo": "10.111.0.6"}, 1))
	_, err := ReadSnapshot(ok)
	assert.Equal(t, reloadSuccess, reloadResult(err))

	_, err = ReadSnapshot(filepath.Join(dir, "missing.json"))
	assert.Equal(t, reloadMissing, reloadResult(err))

	corrupt := filepath.Join(dir, "corrupt.json")
	require.NoError(t, os.WriteFile(corrupt, []byte("[1,2,3]"), 0o644))
	_, err = ReadSnapshot(corrupt)
	assert.Equal(t, reloadParseError, reloadResult(err))

	// A directory in place of the file: readable path, unreadable content -> I/O error.
	_, err = ReadSnapshot(dir)
	assert.Equal(t, reloadReadError, reloadResult(err))
}

// TestObservedState: the values the observable gauges export track the snapshot the
// resolver actually serves (records/writtenAt/generation/ready) plus the daemon's
// watcher and upstream configuration.
func TestObservedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, WriteSnapshot(path, map[string]string{"default/echo": "10.111.0.6"}, 3))

	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	st := s.observedState()
	assert.Equal(t, int64(1), st.records)
	assert.Equal(t, uint64(3), st.generation)
	assert.True(t, st.ready, "a non-empty warm load is ready")
	assert.False(t, st.watchActive, "the daemon has not started its watcher yet")
	assert.Zero(t, st.upstreams)
	assert.Positive(t, st.writtenAt, "writtenAt comes from the envelope")

	s.SetWatchActive(true)
	s.SetUpstreams([]string{"10.96.0.10", "10.96.0.11"})
	require.NoError(t, WriteSnapshot(path, map[string]string{
		"default/echo": "10.222.0.9", "team-a/svc-1": "10.222.0.10",
	}, 4))
	s.ReloadFromSnapshot()

	st = s.observedState()
	assert.Equal(t, int64(2), st.records)
	assert.Equal(t, uint64(4), st.generation, "the reloaded generation is exported")
	assert.True(t, st.watchActive)
	assert.Equal(t, int64(2), st.upstreams)

	s.SetWatchActive(false)
	assert.False(t, s.observedState().watchActive, "a dead watcher is visible")
}

// TestObservedStateEmptySnapshot: an EMPTY snapshot is the silent-outage case — the
// resolver is ready (so mesh misses are authoritative NXDOMAINs) with zero records.
// The records gauge must show that.
func TestObservedStateEmptySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, WriteSnapshot(path, map[string]string{}, 9))

	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	s.ReloadFromSnapshot()

	st := s.observedState()
	assert.Zero(t, st.records, "an empty table NXDOMAINs the whole mesh")
	assert.True(t, st.ready)
}

// TestReloadFromSnapshot: after a snapshot is rewritten on disk, ReloadFromSnapshot
// re-reads it and the resolver answers from the new table.
func TestReloadFromSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, WriteSnapshot(path, map[string]string{"default/echo": "10.111.0.6"}, 1))

	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	ip, _ := s.lookup("echo.default.aether.internal.")
	require.Equal(t, "10.111.0.6", ip, "warm-started from the initial snapshot")

	// The agent rewrites the snapshot with a new IP; the daemon reloads.
	require.NoError(t, WriteSnapshot(path, map[string]string{"default/echo": "10.222.0.9"}, 2))
	s.ReloadFromSnapshot()

	ip, ready := s.lookup("echo.default.aether.internal.")
	assert.Equal(t, "10.222.0.9", ip, "reloaded the rewritten record")
	assert.True(t, ready)
}

// TestReusePortCoBind: two Servers with WithReusePort co-bind the same host:port
// simultaneously (the surge-handoff guarantee) and both answer mesh queries. Without
// SO_REUSEPORT the second bind would fail with EADDRINUSE.
func TestReusePortCoBind(t *testing.T) {
	records := map[string]string{"default/echo": "10.111.0.6"}

	// Only the FIRST resolver's bind is retried on a fresh port (#1236). Once it holds
	// the address no other process can take it, so an EADDRINUSE from the co-binder is
	// the regression this test exists for, and it fails at once.
	addr := onFreshDNSAddr(t, func(addr string) error {
		return startReusePortServer(t, addr, records)
	})
	require.NoError(t, startReusePortServer(t, addr, records), // co-bind the SAME addr
		"the second resolver must co-bind the address the first one holds")

	// Both resolvers answer over their shared port (the kernel load-balances across
	// the two SO_REUSEPORT sockets; either answering proves both are live).
	assertResolves(t, addr, "echo.default.aether.internal.", "10.111.0.6")
	assertResolves(t, addr, "echo.default.aether.internal.", "10.111.0.6")
}

// TestOnFreshDNSAddrRetriesALostPort pins the #1236 retry on a REAL lost port: another
// socket takes the first attempt's port before the resolver binds it, which is what a
// process racing freeDNSPort does, and the helper must come back with the resolver
// bound on a different port rather than fail the test.
func TestOnFreshDNSAddrRetriesALostPort(t *testing.T) {
	var tried []string
	addr := onFreshDNSAddr(t, func(addr string) error {
		tried = append(tried, addr)
		if len(tried) == 1 {
			// No SO_REUSEPORT on this socket, so the resolver cannot join it: its bind
			// fails with EADDRINUSE exactly as on a port another process took.
			thief, err := net.ListenPacket("udp", addr)
			require.NoError(t, err)
			t.Cleanup(func() { _ = thief.Close() })
		}
		return startReusePortServer(t, addr, map[string]string{})
	})
	require.Len(t, tried, 2, "the lost port is retried exactly once")
	assert.NotEqual(t, tried[0], addr, "the retry runs on a fresh port")
	assert.Equal(t, tried[1], addr, "the address returned is the one the resolver bound")
}

// startReusePortServer starts a reuse-port Server bound to addr with the given records
// and registers cleanup to stop it. It waits for the server's OWN ready marker, written
// right after its bind, so a co-bound peer already answering on addr cannot satisfy the
// wait for it. A lost port (Start returned EADDRINUSE) comes back as the error, for
// onFreshDNSAddr to retry; anything else fails the test.
func startReusePortServer(t *testing.T, addr string, records map[string]string) error {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready")
	s := NewServerWithOptions("aether.internal", addr, "", slog.New(slog.DiscardHandler),
		WithReusePort(true), WithReadyMarker(marker))
	s.SetRecords(records)
	ctx, cancel := context.WithCancel(context.Background())
	errc := startServer(ctx, s)
	t.Cleanup(func() {
		cancel()
		select {
		case <-errc:
		case <-time.After(eventWait):
		}
	})
	return startFailure(t, waitStarted(errc, fileExists(marker)), "resolver did not bind %s", addr)
}

// TestReadyMarkerWrittenOnStartRemovedOnCancel: Start writes the pod-local ready
// marker once the listeners are bound, and removes it when the context is cancelled.
func TestReadyMarkerWrittenOnStartRemovedOnCancel(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sub", "mesh-dns.ready") // dir created by Start

	var (
		cancel context.CancelFunc
		errc   <-chan error
	)
	// Once the resolver answers over UDP its listeners are bound, so the marker
	// (written right after buildServers) must exist. A port lost before the bind is
	// retried on a fresh one (#1236).
	onFreshDNSAddr(t, func(addr string) error {
		s := NewServerWithOptions("aether.internal", addr, "", slog.New(slog.DiscardHandler),
			WithReusePort(true), WithReadyMarker(marker))
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		t.Cleanup(cancel)
		errc = startServer(ctx, s)
		return startFailure(t, waitStarted(errc, udpAnswers(addr)), "resolver did not bind %s", addr)
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, eventWait, 20*time.Millisecond, "ready marker written after bind")

	// Shutdown removes the marker so a terminating pod stops reporting ready.
	cancel()
	select {
	case <-errc:
	case <-time.After(eventWait):
		t.Fatal("Start did not return after cancel")
	}
	_, err := os.Stat(marker)
	assert.ErrorIs(t, err, os.ErrNotExist, "ready marker removed on shutdown")
}

// TestReadyMarkerDisabledWhenUnset: with no marker path, Start writes nothing (and
// does not fail). Guards the empty-path no-op branch.
func TestReadyMarkerDisabledWhenUnset(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	// Both helpers must be safe no-ops when readyMarker is empty.
	s.writeReadyMarker(context.Background())
	s.removeReadyMarker()
	assert.Empty(t, s.readyMarker)
}

// assertResolves sends a UDP A query to addr and asserts the answer IP.
func assertResolves(t *testing.T, addr, name, want string) {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(query(name, dns.TypeA), addr)
	require.NoError(t, err)
	require.Len(t, resp.Answer, 1)
	a, ok := resp.Answer[0].(*dns.A)
	require.True(t, ok)
	assert.Equal(t, want, a.A.String())
}

// freeUDPPort grabs an ephemeral UDP port and releases it so a reuse-port Server
// can bind it.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, pc.Close())
	return port
}

// eventWait bounds every wait in this package's tests on a discrete event: a resolver
// binding, a ready marker landing, a goroutine returning, an upstream seeing a query.
// Each wait returns the moment its event happens, so a healthy run pays nothing for
// the width. The bound has to be wide enough for a race-instrumented binary on a
// starved runner (#1177: the old 2 s and 10 s budgets both missed under parallel
// --config=race load). It also has to stay a HARD bound, so a wedged resolver still
// fails with the wait's own message, well inside the 60 s Bazel timeout of a "small"
// test.
const eventWait = 30 * time.Second

// startServer runs s.Start(ctx) in the background and returns a channel that carries
// its error. The channel is buffered and closed after the send, so a cleanup still
// returns at once when a wait (awaitBound, on an early exit) already drained it.
func startServer(ctx context.Context, s *Server) <-chan error {
	errc := make(chan error, 1)
	go func() {
		errc <- s.Start(ctx)
		close(errc)
	}()
	return errc
}

// awaitBound polls cond until it holds, bounded by eventWait. If the server's Start
// (errc) returns first, it fails AT ONCE with Start's error. A resolver that could not
// bind (EADDRINUSE on a port that was free a moment earlier) never gets there, and
// waiting out the whole budget would mislabel a bind failure as slowness (#1177).
func awaitBound(t *testing.T, errc <-chan error, cond func() bool, format string, args ...any) {
	t.Helper()
	if err := waitStarted(errc, cond); err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}

// errStartReturned marks a waitStarted failure where the server's Start returned before
// the awaited condition held. It wraps Start's own error, so errors.Is still sees the
// bind error (EADDRINUSE) underneath.
var errStartReturned = errors.New("the server's Start returned first")

// waitStarted is awaitBound without the t.Fatal, for callers that retry a lost port. It
// returns nil once cond holds, Start's error (wrapped in errStartReturned) if Start
// returns first, and a timeout error after eventWait.
func waitStarted(errc <-chan error, cond func() bool) error {
	deadline := time.Now().Add(eventWait)
	for {
		if cond() {
			return nil
		}
		select {
		case err := <-errc:
			return fmt.Errorf("%w: %w", errStartReturned, err)
		default:
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("not within %s", eventWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isLostPort reports whether err is the resolver's Start failing its bind because the
// port was taken: the one start failure a fresh port can cure.
func isLostPort(err error) bool {
	return errors.Is(err, errStartReturned) && errors.Is(err, syscall.EADDRINUSE)
}

// startFailure returns err when it is nil or a lost port (for onFreshDNSAddr to retry)
// and fails the test at once on anything else.
func startFailure(t *testing.T, err error, format string, args ...any) error {
	t.Helper()
	if err != nil && !isLostPort(err) {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
	return err
}

// bindAttempts bounds onFreshDNSAddr. A port is lost only when another process binds it
// in the instant between freeDNSPort's probe and the resolver's own bind, so losing
// this many in a row is not bad luck: it is a resolver that cannot bind at all.
const bindAttempts = 5

// onFreshDNSAddr starts the FIRST resolver on an address from freeDNSPort and returns
// that address. freeDNSPort has to release its probe sockets so the resolver can bind
// the port by address, as it does in production, and another process can take the port
// in that gap (#1236). When start reports exactly that (see isLostPort) the attempt is
// repeated on a fresh port, at most bindAttempts times. Any other error fails the test.
//
// start must fail only through the resolver's Start returning, so a failed attempt
// leaves nothing bound and nothing running. Only the first resolver on an address goes
// through here: once it holds the address no other process can take it, so a resolver
// co-binding it that gets EADDRINUSE is a real SO_REUSEPORT failure, never retried.
func onFreshDNSAddr(t *testing.T, start func(addr string) error) string {
	t.Helper()
	var err error
	for range bindAttempts {
		addr := fmt.Sprintf("127.0.0.1:%d", freeDNSPort(t))
		if err = start(addr); err == nil {
			return addr
		}
		if !isLostPort(err) {
			t.Fatalf("resolver on %s did not start: %v", addr, err)
		}
		t.Logf("lost %s before the resolver bound it: %v", addr, err)
	}
	t.Fatalf("the resolver lost its port %d times in a row: %v", bindAttempts, err)
	return ""
}

// udpAnswers reports whether a UDP query to addr gets an answer, i.e. a server is bound.
func udpAnswers(addr string) func() bool {
	c := &dns.Client{Net: "udp", Timeout: 200 * time.Millisecond}
	return func() bool {
		_, _, err := c.Exchange(query("echo.default.aether.internal.", dns.TypeA), addr)
		return err == nil
	}
}

// fileExists reports whether path exists, e.g. a resolver's ready marker.
func fileExists(path string) func() bool {
	return func() bool {
		_, err := os.Stat(path)
		return err == nil
	}
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

// serve drives the handler in-process over the production in-memory ResponseWriter
// (memResponseWriter, shared with the self-check watchdog) presenting a UDP client.
func serve(s *Server, r *dns.Msg) *dns.Msg {
	return serveFrom(s, r, nil)
}

// serveFrom drives the handler with a specific client address, so a test can present a
// TCP-originated query (the forward path branches on it).
func serveFrom(s *Server, r *dns.Msg, remote net.Addr) *dns.Msg {
	w := newMemResponseWriter(remote)
	s.ServeDNS(w, r)
	return w.Msg()
}

// TestServeMeshMissColdIsServfail: a mesh miss BEFORE records are ever populated is
// SERVFAIL (retryable), and it is never forwarded upstream.
func TestServeMeshMissColdIsServfail(t *testing.T) {
	// No upstreams configured: if the code ever forwarded a mesh name, forward()
	// would fall through to its own SERVFAIL path — but we assert authoritative,
	// which forward() never sets, so the two are distinguishable.
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	resp := serve(s, query("svc-1.team-a.aether.internal", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeServerFailure, resp.Rcode, "cold mesh miss -> SERVFAIL")
	assert.True(t, resp.Authoritative, "answered authoritatively, not forwarded")
	assert.Empty(t, resp.Answer)
}

// TestServeMeshMissReadyIsNXDomain: once records are populated, an unknown mesh name
// is answered NXDOMAIN authoritatively, never forwarded.
func TestServeMeshMissReadyIsNXDomain(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetRecords(map[string]string{"default/echo": "10.111.0.6"})
	resp := serve(s, query("nope.default.aether.internal", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeNameError, resp.Rcode, "ready mesh miss -> NXDOMAIN")
	assert.True(t, resp.Authoritative)
	assert.Empty(t, resp.Answer)
}

// TestServeMalformedUnderZoneIsNXDomain: a name under the mesh domain that is NOT a
// well-formed "<svc>.<ns>" (wrong label count, e.g. the flat "<svc>.<meshDomain>" or a
// three-label spelling) is answered authoritative NXDOMAIN and NEVER forwarded — even
// while cold, since a structurally invalid name can never become valid.
func TestServeMalformedUnderZoneIsNXDomain(t *testing.T) {
	// Cold server (never populated) + a black-hole upstream: if a malformed mesh name
	// were ever forwarded, forward() would answer NON-authoritative; we assert the
	// opposite, so forwarding is ruled out.
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetUpstreams([]string{"127.0.0.1:1"})

	for _, name := range []string{"svc-1.aether.internal", "a.b.c.aether.internal"} {
		resp := serve(s, query(name, dns.TypeA))
		require.NotNil(t, resp, name)
		assert.Equal(t, dns.RcodeNameError, resp.Rcode, "%s -> NXDOMAIN", name)
		assert.True(t, resp.Authoritative, "%s answered authoritatively, not forwarded", name)
		assert.Empty(t, resp.Answer, name)
	}
}

// TestServeMeshHitAnswersWithEDNS0: a mesh hit answers the A record and echoes the
// client's EDNS0 OPT.
func TestServeMeshHitAnswersWithEDNS0(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetRecords(map[string]string{"default/echo": "10.111.0.6"})

	r := query("echo.default.aether.internal", dns.TypeA)
	r.SetEdns0(4096, true)
	resp := serve(s, r)
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
	assert.True(t, resp.Authoritative)
	require.Len(t, resp.Answer, 1)
	a, ok := resp.Answer[0].(*dns.A)
	require.True(t, ok)
	assert.Equal(t, "10.111.0.6", a.A.String())
	assert.NotNil(t, resp.IsEdns0(), "client EDNS0 OPT echoed back")
}

// TestServeMeshHitNonAIsNODATA: a non-A query for a known mesh name is NODATA
// (NOERROR, empty answer, authoritative) so the name consistently EXISTS.
func TestServeMeshHitNonAIsNODATA(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetRecords(map[string]string{"default/echo": "10.111.0.6"})

	resp := serve(s, query("echo.default.aether.internal", dns.TypeAAAA))
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeSuccess, resp.Rcode, "NODATA, not NXDOMAIN")
	assert.True(t, resp.Authoritative)
	assert.Empty(t, resp.Answer)
}

// TestServeNonMeshForwards: a genuinely non-mesh name IS forwarded to the upstream
// resolver. With no reachable upstream, forward() answers SERVFAIL and, crucially,
// does NOT set Authoritative (the distinguishing marker from the mesh cold path).
func TestServeNonMeshForwards(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	// Point the upstream at a black hole so Exchange fails fast and forward() falls
	// through to its non-authoritative SERVFAIL.
	s.SetUpstreams([]string{"127.0.0.1:1"})

	resp := serve(s, query("google.com", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeServerFailure, resp.Rcode)
	assert.False(t, resp.Authoritative, "forwarded (upstream failed), not an authoritative mesh answer")
}

// TestQueryProto: the transport a query arrived on is read off the writer's remote
// address. Anything that is not a TCP address (including a nil one) is UDP.
func TestQueryProto(t *testing.T) {
	assert.Equal(t, protoUDP, queryProto(newMemResponseWriter(nil)))
	assert.Equal(t, protoUDP, queryProto(newMemResponseWriter(&net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234})))
	assert.Equal(t, protoTCP, queryProto(newMemResponseWriter(&net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234})))
	assert.Equal(t, protoUDP, queryProto(newMemResponseWriter(nilAddr{})), "an unknown addr type is treated as UDP")
}

// nilAddr is an addr of neither UDP nor TCP type.
type nilAddr struct{}

func (nilAddr) Network() string { return "unix" }
func (nilAddr) String() string  { return "@" }

// TestForwardTCPQueryUsesTCP: a query that arrived over TCP is forwarded over TCP. The
// CNI DNATs pod :53 for TCP too, and such a client has already decided it wants an
// answer that does not fit a datagram — relaying it over UDP is what could hand it a
// truncated reply we would then record as a clean "forwarded".
func TestForwardTCPQueryUsesTCP(t *testing.T) {
	seen := &protoRecorder{}
	addr := startUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		seen.add(upstreamProto(w))
		m := new(dns.Msg)
		m.SetReply(r)
		_ = w.WriteMsg(m)
	})

	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetUpstreams([]string{addr})

	resp := serveFrom(s, query("google.com", dns.TypeA), &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5555})
	require.NotNil(t, resp)
	assert.Equal(t, dns.RcodeSuccess, resp.Rcode)
	assert.Equal(t, []string{protoTCP}, seen.all(), "a TCP-originated query is forwarded over TCP")

	// A UDP-originated query still goes out over UDP (the cheap common path).
	resp = serve(s, query("google.com", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, []string{protoTCP, protoUDP}, seen.all())
}

// TestForwardTruncatedUDPReplyRetriesOverTCP: when the upstream truncates the UDP reply
// (TC=1) the resolver re-asks over TCP and returns the full answer, instead of passing a
// truncated reply back as a clean "forwarded" while the client loops.
func TestForwardTruncatedUDPReplyRetriesOverTCP(t *testing.T) {
	seen := &protoRecorder{}
	addr := startUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		proto := upstreamProto(w)
		seen.add(proto)
		m := new(dns.Msg)
		m.SetReply(r)
		if proto == protoUDP {
			// Too big for a datagram: answer TC=1 with nothing, as a real upstream does.
			m.Truncated = true
			_ = w.WriteMsg(m)
			return
		}
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.IPv4(93, 184, 216, 34),
		}}
		_ = w.WriteMsg(m)
	})

	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetUpstreams([]string{addr})

	resp := serve(s, query("big.example.com", dns.TypeA))
	require.NotNil(t, resp)
	assert.Equal(t, []string{protoUDP, protoTCP}, seen.all(), "UDP first, then the TCP retry")
	assert.False(t, resp.Truncated, "the full TCP answer fits the client's buffer")
	require.Len(t, resp.Answer, 1, "the client gets the answer, not an empty TC=1 reply")
	a, ok := resp.Answer[0].(*dns.A)
	require.True(t, ok)
	assert.Equal(t, "93.184.216.34", a.A.String())
}

// TestForwardTruncatedRetryFailureKeepsTruncatedReply: if the TCP retry itself fails,
// the client still gets the upstream's TC=1 reply — a signal it can act on (retry over
// TCP, which the CNI also DNATs here) — rather than a SERVFAIL.
func TestForwardTruncatedRetryFailureKeepsTruncatedReply(t *testing.T) {
	addr := startUpstreamUDPOnly(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Truncated = true
		_ = w.WriteMsg(m)
	})

	s := NewServer("aether.internal", "127.0.0.1:0", "", slog.New(slog.DiscardHandler))
	s.SetUpstreams([]string{addr})

	resp := serve(s, query("big.example.com", dns.TypeA))
	require.NotNil(t, resp)
	assert.True(t, resp.Truncated, "the truncated reply is passed through when TCP is unreachable")
	assert.Equal(t, dns.RcodeSuccess, resp.Rcode, "not a SERVFAIL")
}

// TestUDPSizeFor: the re-truncation ceiling is the client's advertised EDNS0 buffer,
// with RFC 1035's 512-byte floor when it asked without EDNS0 (or advertised less).
func TestUDPSizeFor(t *testing.T) {
	assert.Equal(t, dns.MinMsgSize, udpSizeFor(query("google.com", dns.TypeA)))

	big := query("google.com", dns.TypeA)
	big.SetEdns0(4096, false)
	assert.Equal(t, 4096, udpSizeFor(big))

	small := query("google.com", dns.TypeA)
	small.SetEdns0(200, false)
	assert.Equal(t, dns.MinMsgSize, udpSizeFor(small), "never below the 512-byte floor")
}

// protoRecorder collects, in order, the transports an upstream saw.
type protoRecorder struct {
	mu     sync.Mutex
	protos []string
}

func (p *protoRecorder) add(proto string) {
	p.mu.Lock()
	p.protos = append(p.protos, proto)
	p.mu.Unlock()
}

func (p *protoRecorder) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.protos...)
}

// upstreamProto reports which transport a query reached the TEST upstream over.
func upstreamProto(w dns.ResponseWriter) string {
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		return protoTCP
	}
	return protoUDP
}

// startUpstream runs a test resolver on ONE host:port over BOTH udp and tcp (what a
// real upstream looks like) and returns its address. The sockets are bound here, on
// a kernel-chosen port, and handed to the servers still open (#1199): probing for a
// free port, closing the probe and binding the port again later let another process
// take it in between.
func startUpstream(t *testing.T, h dns.HandlerFunc) string {
	t.Helper()
	pc, ln := listenDNSPair(t)
	serveUpstream(t, &dns.Server{PacketConn: pc, Handler: h})
	serveUpstream(t, &dns.Server{Listener: ln, Handler: h})
	return pc.LocalAddr().String()
}

// startUpstreamUDPOnly runs a test resolver that answers over udp only, so a TCP retry
// against it fails.
func startUpstreamUDPOnly(t *testing.T, h dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	serveUpstream(t, &dns.Server{PacketConn: pc, Handler: h})
	return pc.LocalAddr().String()
}

// listenDNSPair binds a UDP socket on a kernel-chosen loopback port and a TCP listener
// on the same port, and returns both still open. A port whose TCP side is taken is
// released and another one tried.
func listenDNSPair(t *testing.T) (net.PacketConn, net.Listener) {
	t.Helper()
	for range 20 {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		ln, err := net.Listen("tcp", pc.LocalAddr().String())
		if err == nil {
			return pc, ln
		}
		require.NoError(t, pc.Close())
	}
	t.Fatal("no port could be bound on both udp and tcp")
	return nil, nil
}

// serveUpstream starts a test upstream and waits for it to serve. A server handed an
// open socket (PacketConn or Listener) serves it; one with only Addr/Net binds it. If
// serving returns before the server starts -- a bind error, say -- it fails AT ONCE
// with that error, as awaitBound does for the resolver, instead of waiting out the
// whole eventWait and reporting a bind failure as slowness (#1199).
func serveUpstream(t *testing.T, srv *dns.Server) {
	t.Helper()
	where := srv.Addr + "/" + srv.Net
	switch {
	case srv.PacketConn != nil:
		where = srv.PacketConn.LocalAddr().String() + "/udp"
	case srv.Listener != nil:
		where = srv.Listener.Addr().String() + "/tcp"
	}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	errc := make(chan error, 1)
	go func() {
		if srv.PacketConn != nil || srv.Listener != nil {
			errc <- srv.ActivateAndServe()
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	t.Cleanup(func() { _ = srv.Shutdown() })
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("test upstream on %s did not start: %v", where, err)
	case <-time.After(eventWait):
		t.Fatalf("test upstream did not bind %s in time", where)
	}
}

// freeDNSPort finds a port free on BOTH udp and tcp: the forward path dials one
// host:port over either transport, so the test upstream must own both.
func freeDNSPort(t *testing.T) int {
	t.Helper()
	for range 20 {
		port := freeUDPPort(t)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		require.NoError(t, ln.Close())
		return port
	}
	t.Fatal("no port was free on both udp and tcp")
	return 0
}

// TestEnsureSnapshotDir: the agent pre-creates the snapshot's parent so the resolver
// daemon -- which mounts the volume READ-ONLY and cannot create it -- always has a
// directory to watch. Without this the daemon came up Ready but permanently
// record-less on a fresh cluster (#589).
func TestEnsureSnapshotDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh-dns", "records.json")
	require.NoDirExists(t, filepath.Dir(path))

	require.NoError(t, EnsureSnapshotDir(path))
	assert.DirExists(t, filepath.Dir(path), "parent dir created")

	// Idempotent: the agent calls it at startup AND on every write.
	assert.NoError(t, EnsureSnapshotDir(path))
}
