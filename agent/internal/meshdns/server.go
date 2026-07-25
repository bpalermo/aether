// Package meshdns is the node agent's in-process DNS resolver (Istio-style, proposal
// 018 mesh-global FQDN). Unlike Envoy's dns_filter — which broke c-ares resolvers
// (curl/Alpine), breaking even non-mesh resolution because it mishandled forwarded
// queries — this is a real DNS server (miekg/dns): it answers <svc>.<meshDomain> from
// the registry-fed records and forwards everything else to the upstream resolver
// (kube-dns), speaking the full protocol correctly.
//
// It listens on a single HOST-local address (the agent is host-network, HOST_IP:18054)
// — no setns, no per-pod sockets. The CNI DNATs each pod's outbound :53 straight to
// this resolver; conntrack rewrites the reply's source back to the pod's configured
// nameserver. No Envoy DNS layer, no privilege change.
package meshdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bpalermo/aether/common/file"
	"github.com/bpalermo/aether/common/serviceref"

	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

const answerTTL = 30

// snapshotFileMode is the permission for the persisted records snapshot.
const snapshotFileMode = 0o644

// snapshotDirMode is the permission for the snapshot's parent directory.
const snapshotDirMode = 0o755

// ErrSnapshotParse marks a snapshot that exists and was readable but could not be
// decoded, so a caller (and the reload metric) can tell a corrupt file apart from an
// I/O failure or a missing file (fs.ErrNotExist).
var ErrSnapshotParse = errors.New("parse mesh-DNS snapshot")

// Snapshot is the versioned on-disk mesh-DNS record envelope the agent writes and the
// standalone resolver daemon reads (issue #586). The envelope exists because mtime
// alone cannot prove freshness: the writer is event-driven, so the agent re-stamps
// WrittenAt on a periodic heartbeat even when Records are unchanged, and the daemon
// exports now-WrittenAt as aether.mesh_dns.snapshot_age_seconds. A wedged/crashed
// agent then shows up as a growing age instead of silently serving a frozen table.
//
// The legacy bare-map form (just Records, no envelope) is still accepted on read —
// see ReadSnapshot — so an in-place upgrade never starts cold.
type Snapshot struct {
	// WrittenAt is the writer's wall clock (unix seconds) at persist time. It is
	// re-stamped by the heartbeat even when Records do not change. Zero means
	// unknown (a legacy snapshot whose mtime could not be stat'd).
	WrittenAt int64 `json:"writtenAt"`
	// Generation is the writer's record-table version: it advances only when the
	// record CONTENT changes, so a heartbeat rewrite keeps it stable. It is
	// diagnostic only — the resolver always serves whatever Records it read.
	Generation uint64 `json:"generation"`
	// Records maps "<ns>/<svc>" to the mesh Service's A-record IP.
	Records map[string]string `json:"records"`
}

// Server answers mesh A records and forwards the rest. Safe for concurrent use, and a
// controller-runtime Runnable (Start serves until the context is cancelled).
type Server struct {
	meshDomain   string
	addr         string
	snapshotPath string
	readyMarker  string
	reusePort    bool
	log          *slog.Logger
	client       *dns.Client
	metrics      *metrics

	mu          sync.RWMutex
	records     map[string]string // "<ns>/<svc>" -> A-record IP
	ready       bool              // records have been populated at least once
	upstreams   []string
	writtenAt   int64  // Snapshot.WrittenAt of the table currently served (0 = unknown)
	generation  uint64 // Snapshot.Generation of the table currently served
	watchActive bool   // the fsnotify snapshot watcher is running
}

// Option configures a Server built via NewServerWithOptions.
type Option func(*Server)

// WithReusePort makes Start bind the UDP+TCP listeners with SO_REUSEPORT (and
// SO_REUSEADDR) so two Servers can co-bind the same host:port simultaneously.
// This is what makes the standalone mesh-DNS DaemonSet's surge (maxSurge:1)
// hitless: the successor pod binds :18054 while the predecessor still serves.
func WithReusePort(v bool) Option {
	return func(s *Server) { s.reusePort = v }
}

// WithReadyMarker makes Start write a pod-local ready-marker file at path once the
// UDP+TCP listeners are bound (and remove it on shutdown). The exec readiness probe
// (`agent mesh-dns --readiness-check`) stats this file, so the DaemonSet's
// maxSurge:1/maxUnavailable:0 rollout keeps the predecessor pod until the successor
// is TRULY bound — that overlap is what lets SO_REUSEPORT hand off with zero gap.
// The marker is intentionally pod-local (each container's own fs): a network probe
// to hostIP:18054 could be answered by the OTHER co-bound REUSEPORT socket and
// falsely mark this process ready before it has bound. An empty path disables it.
func WithReadyMarker(path string) Option {
	return func(s *Server) { s.readyMarker = path }
}

// NewServer builds the resolver for meshDomain, listening on addr (host:port).
// snapshotPath, when non-empty, is a host-persistent file the last-known record
// table is written to on every SetRecords and warm-loaded from at boot (Fix 1):
// a freshly-restarted agent answers mesh names from last-known ClusterIPs within
// ms of process start, before the informer cache has synced the first reconcile.
func NewServer(meshDomain, addr, snapshotPath string, log *slog.Logger) *Server {
	return NewServerWithOptions(meshDomain, addr, snapshotPath, log)
}

// NewServerWithOptions is NewServer plus functional options (e.g. WithReusePort).
func NewServerWithOptions(meshDomain, addr, snapshotPath string, log *slog.Logger, opts ...Option) *Server {
	s := &Server{
		meshDomain:   meshDomain,
		addr:         addr,
		snapshotPath: snapshotPath,
		log:          commonlog.Named(log, "mesh-dns"),
		client:       &dns.Client{Net: "udp"},
		records:      map[string]string{},
	}
	for _, o := range opts {
		o(s)
	}
	// Instruments (including the observable gauges reading s under its RWMutex) are
	// built before the warm load so the very first snapshot read is counted.
	s.metrics = newMetrics(s.observedState, s.log)
	s.loadSnapshot()
	return s
}

// observedState snapshots the resolver state the observable gauges export. It takes
// the Server's RWMutex only to copy a handful of scalars and releases it before
// returning, so the OTel collect callback never holds the lock across an export (and
// never nests it under a metrics lock).
func (s *Server) observedState() resolverState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolverState{
		records:     int64(len(s.records)),
		writtenAt:   s.writtenAt,
		generation:  s.generation,
		ready:       s.ready,
		watchActive: s.watchActive,
		upstreams:   int64(len(s.upstreams)),
	}
}

// SetWatchActive records whether the daemon's fsnotify snapshot watcher is running.
// It backs the aether.mesh_dns.watch_active gauge: 0 means the watcher never started
// or has died, i.e. the resolver will serve its current table until it restarts.
func (s *Server) SetWatchActive(active bool) {
	s.mu.Lock()
	s.watchActive = active
	s.mu.Unlock()
}

// loadSnapshot warm-starts the record table from the on-disk snapshot, if present.
// A missing/unreadable/corrupt snapshot is not fatal (the resolver simply starts
// cold and forwards nothing for mesh names until the first reconcile — see
// ServeDNS). A successful non-empty load flips ready true so mesh misses answer
// NXDOMAIN authoritatively from the very first query rather than SERVFAIL.
func (s *Server) loadSnapshot() {
	if s.snapshotPath == "" {
		return
	}
	snap, err := s.readSnapshotCounted()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("failed to read mesh-DNS snapshot; starting cold", "path", s.snapshotPath, "error", err)
		}
		return
	}
	s.mu.Lock()
	s.records = snap.Records
	s.writtenAt = snap.WrittenAt
	s.generation = snap.Generation
	if len(snap.Records) > 0 {
		s.ready = true
	}
	s.mu.Unlock()
	s.log.Info("warm-started mesh-DNS records from snapshot",
		"path", s.snapshotPath, "records", len(snap.Records), "generation", snap.Generation, "writtenAt", snap.WrittenAt)
}

// readSnapshotCounted reads the snapshot and increments
// aether.mesh_dns.snapshot_reloads_total with the outcome (success / missing /
// parse_error / read_error), so a wedged or corrupt snapshot is visible in metrics
// and not only in a log line.
func (s *Server) readSnapshotCounted() (*Snapshot, error) {
	snap, err := ReadSnapshot(s.snapshotPath)
	s.metrics.recordReload(reloadResult(err))
	return snap, err
}

// reloadResult classifies a ReadSnapshot error into the reload metric's result label.
func reloadResult(err error) string {
	switch {
	case err == nil:
		return reloadSuccess
	case errors.Is(err, fs.ErrNotExist):
		return reloadMissing
	case errors.Is(err, ErrSnapshotParse):
		return reloadParseError
	default:
		return reloadReadError
	}
}

// ReloadFromSnapshot re-reads the snapshot file and swaps in its record table. It
// is the daemon's fsnotify handler: the writing agent persists a new snapshot and
// the standalone resolver picks it up without an informer. A missing/corrupt file
// is a no-op (the resolver keeps its current table) — never fatal. Unlike
// SetRecords it does NOT re-persist (it read from disk); an empty table still
// flips ready so misses answer NXDOMAIN, matching SetRecords' semantics.
func (s *Server) ReloadFromSnapshot() {
	if s.snapshotPath == "" {
		return
	}
	snap, err := s.readSnapshotCounted()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("failed to reload mesh-DNS snapshot; keeping current records", "path", s.snapshotPath, "error", err)
		}
		return
	}
	s.mu.Lock()
	s.records = snap.Records
	s.writtenAt = snap.WrittenAt
	s.generation = snap.Generation
	s.ready = true
	s.mu.Unlock()
	s.log.Debug("reloaded mesh-DNS records from snapshot",
		"path", s.snapshotPath, "records", len(snap.Records), "generation", snap.Generation, "writtenAt", snap.WrittenAt)
}

// SetRecords replaces the service->IP answer table, flips the ready flag (so a
// subsequent mesh miss answers NXDOMAIN, not SERVFAIL), and persists the table to
// the host-persistent snapshot so a future agent restart warm-starts from it.
//
// This is the in-process serve path (the standalone daemon is fed from disk instead),
// so every call is a fresh projection: the generation advances unconditionally and
// writtenAt is stamped now.
func (s *Server) SetRecords(records map[string]string) {
	s.mu.Lock()
	s.generation++
	s.records = records
	s.writtenAt = time.Now().Unix()
	s.ready = true
	generation := s.generation
	s.mu.Unlock()
	if s.snapshotPath == "" {
		return
	}
	if err := WriteSnapshot(s.snapshotPath, records, generation); err != nil {
		s.log.Warn("failed to persist mesh-DNS snapshot", "path", s.snapshotPath, "error", err)
	}
}

// ReadSnapshot loads and parses the versioned snapshot envelope from path. A caller
// can distinguish a missing file via errors.Is(err, fs.ErrNotExist) and treat it as a
// cold start, and a corrupt one via errors.Is(err, ErrSnapshotParse).
//
// It also accepts the LEGACY bare-map form (`{"<ns>/<svc>":"<ip>"}`, everything
// written before issue #586) so an in-place upgrade — a new daemon reading the old
// agent's file, or a new agent's file read by an old daemon — never starts cold. A
// legacy snapshot carries no writtenAt, so the file's mtime is used instead: the
// freshness signal degrades to mtime exactly for the upgrade window and is exact
// again on the first envelope write.
func ReadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snap, legacy, err := parseSnapshot(data)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrSnapshotParse, path, err)
	}
	if legacy {
		if fi, statErr := os.Stat(path); statErr == nil {
			snap.WrittenAt = fi.ModTime().Unix()
		}
	}
	return snap, nil
}

// parseSnapshot decodes either the versioned envelope or the legacy bare record map,
// reporting which form it found. The envelope is tried first and only accepted when it
// actually carries a "records" object; anything else falls back to the bare map, so a
// legacy file whose service keys happen to collide with envelope field names still
// decodes.
func parseSnapshot(data []byte) (snap *Snapshot, legacy bool, err error) {
	var envelope Snapshot
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Records != nil {
		return &envelope, false, nil
	}
	var records map[string]string
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, false, err
	}
	return &Snapshot{Records: records}, true, nil
}

// WriteSnapshot atomically writes the record table to path as a versioned envelope
// (creating the parent dir), stamping writtenAt with the current wall clock and
// carrying the caller's generation. It is the shared persist path used by the agent's
// capture sink — both on a record change and on the periodic freshness heartbeat —
// and by Server.SetRecords, so the standalone resolver daemon warm-starts and reloads
// from exactly what the agent wrote.
func WriteSnapshot(path string, records map[string]string, generation uint64) error {
	if records == nil {
		// Never emit "records": null — the reader would see an envelope without a
		// records object and fall back to the legacy bare-map decode, which fails.
		records = map[string]string{}
	}
	data, err := json.Marshal(&Snapshot{
		WrittenAt:  time.Now().Unix(),
		Generation: generation,
		Records:    records,
	})
	if err != nil {
		return fmt.Errorf("marshal mesh-DNS snapshot: %w", err)
	}
	// The snapshot lives in a dedicated subdir under the host-persistent registry
	// volume; create it (host mount is DirectoryOrCreate, the subdir is ours).
	if err := os.MkdirAll(filepath.Dir(path), snapshotDirMode); err != nil {
		return fmt.Errorf("create mesh-DNS snapshot dir %s: %w", filepath.Dir(path), err)
	}
	if err := file.AtomicWrite(path, data, snapshotFileMode); err != nil {
		return fmt.Errorf("write mesh-DNS snapshot %s: %w", path, err)
	}
	return nil
}

// SetUpstreams sets the resolver(s) non-mesh queries are forwarded to (host[:port]).
func (s *Server) SetUpstreams(u []string) {
	s.mu.Lock()
	s.upstreams = u
	s.mu.Unlock()
}

// Start serves UDP + TCP on the host address until the context is cancelled. When
// reusePort is set the listeners are opened with SO_REUSEPORT (+SO_REUSEADDR) so a
// successor process can co-bind the same host:port during a surge rollout — the
// standalone daemon's hitless handoff. Otherwise it uses miekg/dns's own
// ListenAndServe (the in-agent single-binder path).
func (s *Server) Start(ctx context.Context) error {
	udp, tcp, err := s.buildServers(ctx)
	if err != nil {
		return err
	}
	// Both listeners are bound now (buildServers pre-binds the sockets on the
	// reusePort path and dns.Server binds them synchronously otherwise): report
	// ready so a surge successor is only marked Ready once it can actually serve.
	s.writeReadyMarker(ctx)
	defer s.removeReadyMarker()
	errc := make(chan error, 2)
	if s.reusePort {
		go func() { errc <- udp.ActivateAndServe() }()
		go func() { errc <- tcp.ActivateAndServe() }()
	} else {
		go func() { errc <- udp.ListenAndServe() }()
		go func() { errc <- tcp.ListenAndServe() }()
	}
	s.log.InfoContext(ctx, "mesh DNS resolver listening", "addr", s.addr, "reusePort", s.reusePort)
	select {
	case <-ctx.Done():
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return nil
	case err := <-errc:
		return err
	}
}

// writeReadyMarker creates (truncating) the pod-local ready-marker file. A
// write error is logged but never fatal — the resolver's job is serving DNS, and
// a missing marker only degrades the surge handoff (the probe stays not-ready),
// it never breaks resolution. A no-op when no marker path is configured.
func (s *Server) writeReadyMarker(ctx context.Context) {
	if s.readyMarker == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.readyMarker), snapshotDirMode); err != nil {
		s.log.WarnContext(ctx, "failed to create mesh-DNS ready-marker dir", "path", s.readyMarker, "error", err)
		return
	}
	f, err := os.OpenFile(s.readyMarker, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, snapshotFileMode)
	if err != nil {
		s.log.WarnContext(ctx, "failed to write mesh-DNS ready-marker", "path", s.readyMarker, "error", err)
		return
	}
	_ = f.Close()
	s.log.InfoContext(ctx, "mesh-DNS ready-marker written", "path", s.readyMarker)
}

// removeReadyMarker best-effort deletes the ready-marker on shutdown so a
// terminating pod stops reporting ready (its readiness probe then fails).
func (s *Server) removeReadyMarker() {
	if s.readyMarker == "" {
		return
	}
	if err := os.Remove(s.readyMarker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("failed to remove mesh-DNS ready-marker on shutdown", "path", s.readyMarker, "error", err)
	}
}

// buildServers constructs the UDP and TCP dns.Servers. With reusePort it
// pre-binds the sockets (SO_REUSEPORT+SO_REUSEADDR via net.ListenConfig) and
// hands them to dns.Server as PacketConn/Listener for ActivateAndServe;
// otherwise it returns Addr-configured servers for ListenAndServe.
func (s *Server) buildServers(ctx context.Context) (udp, tcp *dns.Server, err error) {
	if !s.reusePort {
		return &dns.Server{Addr: s.addr, Net: "udp", Handler: s},
			&dns.Server{Addr: s.addr, Net: "tcp", Handler: s}, nil
	}
	lc := net.ListenConfig{Control: reusePortControl}
	pc, err := lc.ListenPacket(ctx, "udp", s.addr)
	if err != nil {
		return nil, nil, fmt.Errorf("mesh-DNS udp listen %s: %w", s.addr, err)
	}
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("mesh-DNS tcp listen %s: %w", s.addr, err)
	}
	return &dns.Server{PacketConn: pc, Handler: s},
		&dns.Server{Listener: ln, Handler: s}, nil
}

// reusePortControl is the net.ListenConfig.Control hook that sets SO_REUSEPORT
// and SO_REUSEADDR on the raw socket before bind, letting two resolver processes
// co-bind the same HOST_IP:18054 during a surge rollout.
func reusePortControl(_, _ string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		if sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); sockErr != nil {
			return
		}
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

// ServeDNS answers a known mesh name authoritatively for EVERY query type — A
// returns the record, anything else (AAAA, etc.) returns NODATA (NOERROR, empty) so
// the name consistently EXISTS. Forwarding the AAAA would yield NXDOMAIN upstream, and
// c-ares (curl/Alpine) then concludes the whole name is gone.
//
// The resolver is AUTHORITATIVE for the ENTIRE mesh domain: ANY name under
// .meshDomain is answered locally and NEVER forwarded to kube-dns (which has no mesh
// zone and would answer SERVFAIL/NXDOMAIN or a slow upstream round-trip, surfacing as
// roll-correlated failures). A well-formed "<svc>.<ns>" miss is NXDOMAIN once records
// have ever been populated (a real miss) or SERVFAIL while still cold (never
// populated, so the name may simply not have been reconciled yet — retryable); a
// malformed spelling under the zone is always NXDOMAIN. Only genuinely non-mesh names
// (cluster.local, external) are forwarded upstream.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	start := time.Now()
	result := s.serve(w, r)
	s.metrics.observeQuery(result, time.Since(start))
}

// serve dispatches a query to the mesh or forward path and returns the metric result
// label, so ServeDNS can time the whole handling in one place.
func (s *Server) serve(w dns.ResponseWriter, r *dns.Msg) string {
	if len(r.Question) == 1 {
		q := r.Question[0]
		if s.isMeshName(q.Name) {
			return s.serveMesh(w, r, q)
		}
	}
	return s.forward(w, r)
}

// serveMesh answers a mesh-domain query authoritatively: a well-formed "<svc>.<ns>"
// hit returns the A record (NODATA for non-A) and a miss returns NXDOMAIN when ready
// or SERVFAIL when still cold; a malformed name under the zone (wrong label count)
// is always NXDOMAIN — it can never exist, so retrying can't help.
func (s *Server) serveMesh(w dns.ResponseWriter, r *dns.Msg, q dns.Question) string {
	if _, _, ok := s.parseMeshName(q.Name); !ok {
		// Under the mesh domain but not a well-formed "<svc>.<ns>" name. Structurally
		// invalid regardless of readiness: authoritative NXDOMAIN, never forwarded.
		return s.writeRcode(w, r, dns.RcodeNameError, resultNXDomain)
	}
	ip, ready := s.lookup(q.Name)
	if ip == "" {
		if !ready {
			// Never populated: the name may simply not be reconciled yet. SERVFAIL is
			// retryable and not negatively cached, unlike NXDOMAIN.
			return s.writeRcode(w, r, dns.RcodeServerFailure, resultCold)
		}
		// Authoritative miss: the mesh has records but not this name.
		return s.writeRcode(w, r, dns.RcodeNameError, resultNXDomain)
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	// Echo the client's EDNS0 OPT (UDP size + DO); c-ares rejects an answer
	// that omits the OPT it asked with (getaddrinfo/dig are lenient).
	if opt := r.IsEdns0(); opt != nil {
		m.SetEdns0(opt.UDPSize(), opt.Do())
	}
	if q.Qtype == dns.TypeA {
		if v4 := net.ParseIP(ip).To4(); v4 != nil {
			m.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: answerTTL},
				A:   v4,
			}}
		}
	}
	// Non-A (incl. AAAA): NODATA — empty answer, NOERROR, authoritative.
	_ = w.WriteMsg(m)
	return resultAnswered
}

// writeRcode replies with an authoritative bare rcode (no answer) and returns the
// caller's metric result label.
func (s *Server) writeRcode(w dns.ResponseWriter, r *dns.Msg, rcode int, result string) string {
	m := new(dns.Msg)
	m.SetRcode(r, rcode)
	m.Authoritative = true
	_ = w.WriteMsg(m)
	return result
}

// isMeshName reports whether qname is any name UNDER the mesh domain (has at least
// one label before ".<meshDomain>"). The resolver owns the whole zone, so every such
// name is answered authoritatively and NEVER forwarded — a well-formed "<svc>.<ns>"
// resolves (hit / NXDOMAIN / cold SERVFAIL in serveMesh), and a malformed spelling
// (wrong label count, e.g. the flat "<svc>.<meshDomain>") is an authoritative
// NXDOMAIN rather than a wasted, slow round-trip to kube-dns (which has no mesh zone).
// The bare apex "<meshDomain>" has no leading label and is not matched (it falls
// through to forwarding — harmless, nobody resolves it as a service).
func (s *Server) isMeshName(qname string) bool {
	name := strings.TrimSuffix(strings.ToLower(qname), ".")
	return strings.HasSuffix(name, "."+s.meshDomain)
}

// parseMeshName splits <svc>.<ns>.<meshDomain> into (ns, svc). ok is false when the
// name is not a well-formed two-label mesh name.
func (s *Server) parseMeshName(qname string) (ns, svc string, ok bool) {
	name := strings.TrimSuffix(strings.ToLower(qname), ".")
	suffix := "." + s.meshDomain
	if !strings.HasSuffix(name, suffix) {
		return "", "", false
	}
	// "<svc>.<ns>" — exactly two labels (service name, then namespace).
	svc, ns, found := strings.Cut(strings.TrimSuffix(name, suffix), ".")
	if !found || svc == "" || ns == "" || strings.Contains(ns, ".") {
		return "", "", false
	}
	return ns, svc, true
}

// lookup maps <svc>.<ns>.<meshDomain> -> its A-record IP (proposal 020 Part 1;
// records are keyed by the "<ns>/<svc>" key). ready reports whether the record
// table has ever been populated (warm-start snapshot or a reconcile), so the
// caller can pick SERVFAIL (cold) vs NXDOMAIN (real miss) for an empty result.
func (s *Server) lookup(qname string) (ip string, ready bool) {
	ns, svc, ok := s.parseMeshName(qname)
	if !ok {
		return "", false
	}
	s.mu.RLock()
	ip = s.records[serviceref.New(ns, svc).Key()]
	ready = s.ready
	s.mu.RUnlock()
	return ip, ready
}

func (s *Server) forward(w dns.ResponseWriter, r *dns.Msg) string {
	s.mu.RLock()
	ups := s.upstreams
	s.mu.RUnlock()
	for _, up := range ups {
		addr := up
		if !strings.Contains(addr, ":") {
			addr += ":53"
		}
		if resp, _, err := s.client.Exchange(r, addr); err == nil && resp != nil {
			_ = w.WriteMsg(resp)
			return resultForwarded
		}
	}
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
	return resultForwardError
}
