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

// Server answers mesh A records and forwards the rest. Safe for concurrent use, and a
// controller-runtime Runnable (Start serves until the context is cancelled).
type Server struct {
	meshDomain   string
	addr         string
	snapshotPath string
	reusePort    bool
	log          *slog.Logger
	client       *dns.Client
	metrics      *metrics

	mu        sync.RWMutex
	records   map[string]string // "<ns>/<svc>" -> A-record IP
	ready     bool              // records have been populated at least once
	upstreams []string
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
		metrics:      newMetrics(),
		records:      map[string]string{},
	}
	for _, o := range opts {
		o(s)
	}
	s.loadSnapshot()
	return s
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
	records, err := ReadSnapshot(s.snapshotPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("failed to read mesh-DNS snapshot; starting cold", "path", s.snapshotPath, "error", err)
		}
		return
	}
	s.mu.Lock()
	s.records = records
	if len(records) > 0 {
		s.ready = true
	}
	s.mu.Unlock()
	s.log.Info("warm-started mesh-DNS records from snapshot", "path", s.snapshotPath, "records", len(records))
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
	records, err := ReadSnapshot(s.snapshotPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("failed to reload mesh-DNS snapshot; keeping current records", "path", s.snapshotPath, "error", err)
		}
		return
	}
	s.mu.Lock()
	s.records = records
	s.ready = true
	s.mu.Unlock()
	s.log.Info("reloaded mesh-DNS records from snapshot", "path", s.snapshotPath, "records", len(records))
}

// SetRecords replaces the service->IP answer table, flips the ready flag (so a
// subsequent mesh miss answers NXDOMAIN, not SERVFAIL), and persists the table to
// the host-persistent snapshot so a future agent restart warm-starts from it.
func (s *Server) SetRecords(records map[string]string) {
	s.mu.Lock()
	s.records = records
	s.ready = true
	s.mu.Unlock()
	if s.snapshotPath == "" {
		return
	}
	if err := WriteSnapshot(s.snapshotPath, records); err != nil {
		s.log.Warn("failed to persist mesh-DNS snapshot", "path", s.snapshotPath, "error", err)
	}
}

// ReadSnapshot loads and parses the JSON record table (\"<ns>/<svc>\" -> A-record
// IP) from path. A caller can distinguish a missing file via errors.Is(err,
// fs.ErrNotExist) and treat it as a cold start.
func ReadSnapshot(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var records map[string]string
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse mesh-DNS snapshot %s: %w", path, err)
	}
	return records, nil
}

// WriteSnapshot atomically writes the record table to path (creating the parent
// dir). It is the shared persist path used by the agent's capture sink and by
// Server.SetRecords, so the standalone resolver daemon warm-starts and reloads
// from exactly what the agent wrote.
func WriteSnapshot(path string, records map[string]string) error {
	data, err := json.Marshal(records)
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
	if len(r.Question) == 1 {
		q := r.Question[0]
		if s.isMeshName(q.Name) {
			s.serveMesh(w, r, q)
			return
		}
	}
	s.forward(w, r)
}

// serveMesh answers a mesh-domain query authoritatively: a well-formed "<svc>.<ns>"
// hit returns the A record (NODATA for non-A) and a miss returns NXDOMAIN when ready
// or SERVFAIL when still cold; a malformed name under the zone (wrong label count)
// is always NXDOMAIN — it can never exist, so retrying can't help.
func (s *Server) serveMesh(w dns.ResponseWriter, r *dns.Msg, q dns.Question) {
	if _, _, ok := s.parseMeshName(q.Name); !ok {
		// Under the mesh domain but not a well-formed "<svc>.<ns>" name. Structurally
		// invalid regardless of readiness: authoritative NXDOMAIN, never forwarded.
		s.writeRcode(w, r, dns.RcodeNameError, resultNXDomain)
		return
	}
	ip, ready := s.lookup(q.Name)
	if ip == "" {
		if !ready {
			// Never populated: the name may simply not be reconciled yet. SERVFAIL is
			// retryable and not negatively cached, unlike NXDOMAIN.
			s.writeRcode(w, r, dns.RcodeServerFailure, resultCold)
			return
		}
		// Authoritative miss: the mesh has records but not this name.
		s.writeRcode(w, r, dns.RcodeNameError, resultNXDomain)
		return
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
	s.metrics.record(resultAnswered)
}

// writeRcode replies with an authoritative bare rcode (no answer) and records the metric.
func (s *Server) writeRcode(w dns.ResponseWriter, r *dns.Msg, rcode int, result string) {
	m := new(dns.Msg)
	m.SetRcode(r, rcode)
	m.Authoritative = true
	_ = w.WriteMsg(m)
	s.metrics.record(result)
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

func (s *Server) forward(w dns.ResponseWriter, r *dns.Msg) {
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
			s.metrics.record(resultForwarded)
			return
		}
	}
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
	s.metrics.record(resultForwardError)
}
