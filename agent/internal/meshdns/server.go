// Package meshdns is the node agent's in-process DNS resolver (Istio-style, proposal
// 018 mesh-global FQDN). Unlike Envoy's dns_filter — which proved incompatible with
// c-ares resolvers (curl/Alpine), breaking even non-mesh resolution — this is a real
// DNS server (miekg/dns): it answers <svc>.<meshDomain> from the registry-fed records
// and forwards everything else to the upstream resolver (kube-dns), speaking the full
// protocol correctly.
//
// The agent is host-network, so — like Envoy bound per-pod listeners via
// NetworkNamespaceFilepath — the server binds a UDP+TCP socket inside each pod's netns
// at the DNS capture port (the CNI redirects the pod's :53 there) via setns + raw
// syscalls (Go's net package can't create a socket in a foreign netns reliably).
package meshdns

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"

	commonconstants "github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

const answerTTL = 30

// Server answers mesh A records and forwards the rest. Safe for concurrent use.
type Server struct {
	meshDomain string
	port       int
	log        *slog.Logger
	client     *dns.Client

	mu        sync.RWMutex
	records   map[string]string // bare service -> A-record IP
	upstreams []string

	nsMu    sync.Mutex
	serving map[string][]*dns.Server // netns path -> its UDP+TCP servers
}

// NewServer builds the resolver for meshDomain.
func NewServer(meshDomain string, log *slog.Logger) *Server {
	return &Server{
		meshDomain: meshDomain,
		port:       commonconstants.ProxyDNSCapturePort,
		log:        commonlog.Named(log, "mesh-dns"),
		client:     &dns.Client{Net: "udp"},
		records:    map[string]string{},
		serving:    map[string][]*dns.Server{},
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

// AddNetns starts serving DNS (UDP+TCP) inside the pod netns at netnsPath. Idempotent.
func (s *Server) AddNetns(netnsPath string) error {
	s.nsMu.Lock()
	defer s.nsMu.Unlock()
	if _, ok := s.serving[netnsPath]; ok {
		return nil
	}
	pc, l, err := openInNetns(netnsPath, s.port)
	if err != nil {
		return fmt.Errorf("open mesh-DNS sockets in %s: %w", netnsPath, err)
	}
	udp := &dns.Server{PacketConn: pc, Handler: s}
	tcp := &dns.Server{Listener: l, Handler: s}
	go func() {
		if err := udp.ActivateAndServe(); err != nil {
			s.log.Debug("mesh-DNS udp server stopped", "netns", netnsPath, "error", err)
		}
	}()
	go func() {
		if err := tcp.ActivateAndServe(); err != nil {
			s.log.Debug("mesh-DNS tcp server stopped", "netns", netnsPath, "error", err)
		}
	}()
	s.serving[netnsPath] = []*dns.Server{udp, tcp}
	s.log.Info("serving mesh DNS in pod netns", "netns", netnsPath, "port", s.port)
	return nil
}

// RemoveNetns stops serving DNS in netnsPath.
func (s *Server) RemoveNetns(netnsPath string) {
	s.nsMu.Lock()
	defer s.nsMu.Unlock()
	for _, srv := range s.serving[netnsPath] {
		_ = srv.Shutdown()
	}
	delete(s.serving, netnsPath)
}

// ServeDNS answers an in-scope mesh A query from the table; everything else (other
// types, non-mesh names, cluster.local) is forwarded to the upstream resolver.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 1 && r.Question[0].Qtype == dns.TypeA {
		if ip := s.lookup(r.Question[0].Name); ip != "" {
			m := new(dns.Msg)
			m.SetReply(r)
			m.Authoritative = true
			m.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: answerTTL},
				A:   net.ParseIP(ip).To4(),
			}}
			_ = w.WriteMsg(m)
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
			return
		}
	}
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}

// openInNetns creates a UDP PacketConn and a TCP Listener bound to 0.0.0.0:port inside
// the network namespace at netnsPath, using raw syscalls on a setns'd locked thread.
func openInNetns(netnsPath string, port int) (net.PacketConn, net.Listener, error) {
	runtime.LockOSThread()
	origin, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread()
		return nil, nil, err
	}
	defer func() { _ = origin.Close() }()
	target, err := os.Open(netnsPath)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, nil, err
	}
	defer func() { _ = target.Close() }()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return nil, nil, fmt.Errorf("entering netns: %w", err)
	}

	pc, l, sockErr := bindSockets(port)

	if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
		// Poisoned thread: keep it locked so the runtime discards it.
		return nil, nil, fmt.Errorf("restoring host netns: %w", err)
	}
	runtime.UnlockOSThread()
	return pc, l, sockErr
}

// bindSockets (caller is setns'd into the target netns) creates the UDP+TCP sockets
// via raw syscalls and wraps them as net.Conn types (FilePacketConn/FileListener dup
// the fd and register it with the runtime poller, independent of the current netns).
func bindSockets(port int) (net.PacketConn, net.Listener, error) {
	sa := &unix.SockaddrInet4{Port: port}

	ufd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("udp socket: %w", err)
	}
	_ = unix.SetsockoptInt(ufd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	if err := unix.Bind(ufd, sa); err != nil {
		_ = unix.Close(ufd)
		return nil, nil, fmt.Errorf("udp bind: %w", err)
	}
	uf := os.NewFile(uintptr(ufd), "mesh-dns-udp")
	pc, err := net.FilePacketConn(uf)
	_ = uf.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("udp filepacketconn: %w", err)
	}

	tfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("tcp socket: %w", err)
	}
	_ = unix.SetsockoptInt(tfd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	if err := unix.Bind(tfd, sa); err != nil {
		_ = unix.Close(tfd)
		_ = pc.Close()
		return nil, nil, fmt.Errorf("tcp bind: %w", err)
	}
	if err := unix.Listen(tfd, 128); err != nil {
		_ = unix.Close(tfd)
		_ = pc.Close()
		return nil, nil, fmt.Errorf("tcp listen: %w", err)
	}
	tf := os.NewFile(uintptr(tfd), "mesh-dns-tcp")
	l, err := net.FileListener(tf)
	_ = tf.Close()
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("tcp filelistener: %w", err)
	}
	return pc, l, nil
}
