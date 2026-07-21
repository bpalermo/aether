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
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bpalermo/aether/common/file"
	"github.com/bpalermo/aether/common/serviceref"

	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/miekg/dns"
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
	log          *slog.Logger
	client       *dns.Client
	metrics      *metrics

	mu        sync.RWMutex
	records   map[string]string // "<ns>/<svc>" -> A-record IP
	ready     bool              // records have been populated at least once
	upstreams []string
}

// NewServer builds the resolver for meshDomain, listening on addr (host:port).
// snapshotPath, when non-empty, is a host-persistent file the last-known record
// table is written to on every SetRecords and warm-loaded from at boot (Fix 1):
// a freshly-restarted agent answers mesh names from last-known ClusterIPs within
// ms of process start, before the informer cache has synced the first reconcile.
func NewServer(meshDomain, addr, snapshotPath string, log *slog.Logger) *Server {
	s := &Server{
		meshDomain:   meshDomain,
		addr:         addr,
		snapshotPath: snapshotPath,
		log:          commonlog.Named(log, "mesh-dns"),
		client:       &dns.Client{Net: "udp"},
		metrics:      newMetrics(),
		records:      map[string]string{},
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
	data, err := os.ReadFile(s.snapshotPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("failed to read mesh-DNS snapshot; starting cold", "path", s.snapshotPath, "error", err)
		}
		return
	}
	var records map[string]string
	if err := json.Unmarshal(data, &records); err != nil {
		s.log.Warn("failed to parse mesh-DNS snapshot; starting cold", "path", s.snapshotPath, "error", err)
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

// SetRecords replaces the service->IP answer table, flips the ready flag (so a
// subsequent mesh miss answers NXDOMAIN, not SERVFAIL), and persists the table to
// the host-persistent snapshot so a future agent restart warm-starts from it.
func (s *Server) SetRecords(records map[string]string) {
	s.mu.Lock()
	s.records = records
	s.ready = true
	s.mu.Unlock()
	s.persistSnapshot(records)
}

// persistSnapshot atomically writes the record table to the snapshot file. Errors
// are logged, never fatal: a failed persist only means the NEXT restart starts
// cold, and serving continues normally.
func (s *Server) persistSnapshot(records map[string]string) {
	if s.snapshotPath == "" {
		return
	}
	data, err := json.Marshal(records)
	if err != nil {
		s.log.Warn("failed to marshal mesh-DNS snapshot", "error", err)
		return
	}
	// The snapshot lives in a dedicated subdir under the host-persistent registry
	// volume; create it (host mount is DirectoryOrCreate, the subdir is ours).
	if err := os.MkdirAll(filepath.Dir(s.snapshotPath), snapshotDirMode); err != nil {
		s.log.Warn("failed to create mesh-DNS snapshot dir", "path", s.snapshotPath, "error", err)
		return
	}
	if err := file.AtomicWrite(s.snapshotPath, data, snapshotFileMode); err != nil {
		s.log.Warn("failed to persist mesh-DNS snapshot", "path", s.snapshotPath, "error", err)
	}
}

// SetUpstreams sets the resolver(s) non-mesh queries are forwarded to (host[:port]).
func (s *Server) SetUpstreams(u []string) {
	s.mu.Lock()
	s.upstreams = u
	s.mu.Unlock()
}

// Start serves UDP + TCP on the host address until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	udp := &dns.Server{Addr: s.addr, Net: "udp", Handler: s}
	tcp := &dns.Server{Addr: s.addr, Net: "tcp", Handler: s}
	errc := make(chan error, 2)
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	s.log.InfoContext(ctx, "mesh DNS resolver listening", "addr", s.addr)
	select {
	case <-ctx.Done():
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return nil
	case err := <-errc:
		return err
	}
}

// ServeDNS answers a known mesh name authoritatively for EVERY query type — A
// returns the record, anything else (AAAA, etc.) returns NODATA (NOERROR, empty) so
// the name consistently EXISTS. Forwarding the AAAA would yield NXDOMAIN upstream, and
// c-ares (curl/Alpine) then concludes the whole name is gone.
//
// The resolver is AUTHORITATIVE for the mesh domain: a name under .meshDomain that
// is not in the record table is NEVER forwarded to kube-dns (which has no mesh
// zone and would answer SERVFAIL/NXDOMAIN, surfacing as roll-correlated failures).
// Instead the miss is answered locally — NXDOMAIN once records have ever been
// populated (a real miss), or SERVFAIL while still cold (never populated, so the
// name may simply not have been reconciled yet — retryable). Only genuinely
// non-mesh names (cluster.local, external) are forwarded upstream.
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

// serveMesh answers a mesh-domain query authoritatively: a hit returns the A record
// (NODATA for non-A), a miss returns NXDOMAIN when ready or SERVFAIL when still cold.
func (s *Server) serveMesh(w dns.ResponseWriter, r *dns.Msg, q dns.Question) {
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

// isMeshName reports whether qname is a well-formed name under the mesh domain
// ("<svc>.<ns>.<meshDomain>", exactly two labels before the suffix). Names under
// the mesh domain that are answered authoritatively (hit, NXDOMAIN, or cold
// SERVFAIL) — never forwarded. A malformed name under the mesh domain (wrong label
// count) is NOT a mesh name and falls through to forwarding, matching the previous
// lookup()=="" behaviour for those spellings.
func (s *Server) isMeshName(qname string) bool {
	_, _, ok := s.parseMeshName(qname)
	return ok
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
