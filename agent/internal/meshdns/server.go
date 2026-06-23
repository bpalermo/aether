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
	"log/slog"
	"net"
	"strings"
	"sync"

	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/miekg/dns"
)

const answerTTL = 30

// Server answers mesh A records and forwards the rest. Safe for concurrent use, and a
// controller-runtime Runnable (Start serves until the context is cancelled).
type Server struct {
	meshDomain string
	addr       string
	log        *slog.Logger
	client     *dns.Client
	metrics    *metrics

	mu        sync.RWMutex
	records   map[string]string // bare service -> A-record IP
	upstreams []string
}

// NewServer builds the resolver for meshDomain, listening on addr (host:port).
func NewServer(meshDomain, addr string, log *slog.Logger) *Server {
	return &Server{
		meshDomain: meshDomain,
		addr:       addr,
		log:        commonlog.Named(log, "mesh-dns"),
		client:     &dns.Client{Net: "udp"},
		metrics:    newMetrics(),
		records:    map[string]string{},
	}
}

// SetRecords replaces the service->IP answer table.
func (s *Server) SetRecords(records map[string]string) {
	s.mu.Lock()
	s.records = records
	s.mu.Unlock()
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
// c-ares (curl/Alpine) then concludes the whole name is gone. Unknown names (incl.
// cluster.local and external) are forwarded to the upstream resolver.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 1 {
		q := r.Question[0]
		if ip := s.lookup(q.Name); ip != "" {
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
			return
		}
	}
	s.forward(w, r)
}

// lookup maps <svc>.<meshDomain> -> its A-record IP, or "" if not a known mesh name.
func (s *Server) lookup(qname string) string {
	name := strings.TrimSuffix(strings.ToLower(qname), ".")
	suffix := "." + s.meshDomain
	if !strings.HasSuffix(name, suffix) {
		return ""
	}
	svc := strings.TrimSuffix(name, suffix)
	if svc == "" || strings.Contains(svc, ".") {
		return ""
	}
	s.mu.RLock()
	ip := s.records[svc]
	s.mu.RUnlock()
	return ip
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
