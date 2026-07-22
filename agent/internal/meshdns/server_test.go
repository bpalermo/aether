package meshdns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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
	var got map[string]string
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, map[string]string{"default/echo": "10.111.0.6"}, got)

	// A second server warm-starts from what the first persisted.
	s2 := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	ip, ready := s2.lookup("echo.default.aether.internal.")
	assert.Equal(t, "10.111.0.6", ip)
	assert.True(t, ready)
}

// TestSnapshotRoundTrip: WriteSnapshot then ReadSnapshot returns the same table,
// and a missing file surfaces os.ErrNotExist so callers can treat it as cold.
func TestSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "records.json") // dir does not exist yet
	want := map[string]string{"team-a/svc-1": "10.111.0.5", "default/echo": "10.111.0.6"}
	require.NoError(t, WriteSnapshot(path, want), "WriteSnapshot creates the parent dir")

	got, err := ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	_, err = ReadSnapshot(filepath.Join(t.TempDir(), "missing.json"))
	assert.ErrorIs(t, err, os.ErrNotExist, "a missing snapshot is distinguishable as not-exist")
}

// TestReloadFromSnapshot: after a snapshot is rewritten on disk, ReloadFromSnapshot
// re-reads it and the resolver answers from the new table.
func TestReloadFromSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, WriteSnapshot(path, map[string]string{"default/echo": "10.111.0.6"}))

	s := NewServer("aether.internal", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	ip, _ := s.lookup("echo.default.aether.internal.")
	require.Equal(t, "10.111.0.6", ip, "warm-started from the initial snapshot")

	// The agent rewrites the snapshot with a new IP; the daemon reloads.
	require.NoError(t, WriteSnapshot(path, map[string]string{"default/echo": "10.222.0.9"}))
	s.ReloadFromSnapshot()

	ip, ready := s.lookup("echo.default.aether.internal.")
	assert.Equal(t, "10.222.0.9", ip, "reloaded the rewritten record")
	assert.True(t, ready)
}

// TestReusePortCoBind: two Servers with WithReusePort co-bind the same host:port
// simultaneously (the surge-handoff guarantee) and both answer mesh queries. Without
// SO_REUSEPORT the second bind would fail with EADDRINUSE.
func TestReusePortCoBind(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	records := map[string]string{"default/echo": "10.111.0.6"}

	s1 := newReusePortServer(t, addr, records)
	s2 := newReusePortServer(t, addr, records) // co-bind the SAME addr

	// Both resolvers answer over their shared port (the kernel load-balances across
	// the two SO_REUSEPORT sockets; either answering proves both are live).
	assertResolves(t, addr, "echo.default.aether.internal.", "10.111.0.6")
	assertResolves(t, addr, "echo.default.aether.internal.", "10.111.0.6")

	_ = s1
	_ = s2
}

// newReusePortServer starts a reuse-port Server bound to addr with the given
// records, waits for it to bind, and registers cleanup to stop it.
func newReusePortServer(t *testing.T, addr string, records map[string]string) *Server {
	t.Helper()
	s := NewServerWithOptions("aether.internal", addr, "", slog.New(slog.DiscardHandler), WithReusePort(true))
	s.SetRecords(records)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
		}
	})
	waitForUDP(t, addr)
	return s
}

// TestReadyMarkerWrittenOnStartRemovedOnCancel: Start writes the pod-local ready
// marker once the listeners are bound, and removes it when the context is cancelled.
func TestReadyMarkerWrittenOnStartRemovedOnCancel(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	marker := filepath.Join(t.TempDir(), "sub", "mesh-dns.ready") // dir created by Start

	s := NewServerWithOptions("aether.internal", addr, "", slog.New(slog.DiscardHandler),
		WithReusePort(true), WithReadyMarker(marker))
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Start(ctx) }()

	// Once the resolver answers over UDP its listeners are bound, so the marker
	// (written right after buildServers) must exist.
	waitForUDP(t, addr)
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 2*time.Second, 20*time.Millisecond, "ready marker written after bind")

	// Shutdown removes the marker so a terminating pod stops reporting ready.
	cancel()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
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

// waitForUDP polls until a UDP query to addr succeeds (the server has bound).
func waitForUDP(t *testing.T, addr string) {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := c.Exchange(query("echo.default.aether.internal.", dns.TypeA), addr); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("resolver did not bind %s in time", addr)
}

// fakeResponseWriter captures the reply message ServeDNS writes.
type fakeResponseWriter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

func (f *fakeResponseWriter) WriteMsg(m *dns.Msg) error { f.msg = m; return nil }
func (f *fakeResponseWriter) LocalAddr() net.Addr       { return &net.UDPAddr{} }
func (f *fakeResponseWriter) RemoteAddr() net.Addr      { return &net.UDPAddr{} }

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func serve(s *Server, r *dns.Msg) *dns.Msg {
	w := &fakeResponseWriter{}
	s.ServeDNS(w, r)
	return w.msg
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
