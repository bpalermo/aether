package meshdns

import (
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

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
