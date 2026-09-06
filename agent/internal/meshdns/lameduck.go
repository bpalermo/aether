package meshdns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// DefaultLameDuckMax is the default ceiling on the post-SIGTERM lame-duck window
// (issue #729). It must stay comfortably below the DaemonSet's
// terminationGracePeriodSeconds — the chart derives that as this value + 5s — because
// the kubelet SIGKILLs at the grace deadline and a SIGKILL is exactly the abrupt
// socket close the lame duck exists to avoid.
const DefaultLameDuckMax = 10 * time.Second

// lameDuckProbeInterval is how often the lame duck asks the SO_REUSEPORT group who is
// answering. Each probe is a fresh 4-tuple, so with two sockets in the group the kernel
// steers roughly half of them at the successor: a handful of probes is enough, and at
// 200ms the whole detection costs a few datagrams to ourselves.
const lameDuckProbeInterval = 200 * time.Millisecond

// lameDuckProbeTimeout bounds ONE successor probe. It is deliberately shorter than the
// probe interval: the loopback exchange is a sub-millisecond in-process handler run, so
// anything slower is a lost datagram, and waiting on it would only delay the next roll
// of the reuseport dice.
const lameDuckProbeTimeout = 150 * time.Millisecond

// instanceLabels is the reserved two-label prefix of the instance-identity name; the
// mesh domain is appended to it. See instanceQName.
const instanceLabels = "_instance._aether"

// Lame-duck exit reasons — the `reason` attribute on aether.mesh_dns.lame_duck.exits.
const (
	// lameDuckSuccessor: a DIFFERENT instance answered on our own listen address, so
	// the successor is in the reuseport group and serving. This is the reason a
	// healthy surge rollout must show on every node.
	lameDuckSuccessor = "successor"
	// lameDuckDeadline: --lame-duck-max elapsed with no successor observed. Normal for
	// a genuine scale-down or node drain (nothing is replacing us); on a rollout it
	// means the successor never came up, and the exposure is the same drop this
	// feature exists to remove.
	lameDuckDeadline = "deadline"
	// lameDuckSignal: a SECOND termination signal arrived. An operator (or a kubelet
	// racing its own grace period) asked us to stop waiting, so we close at once.
	lameDuckSignal = "signal"
)

// WithLameDuck sets the ceiling on the post-SIGTERM lame-duck window (issue #729).
//
// On SIGTERM the resolver stops reporting ready but KEEPS SERVING until either a
// successor is observed answering on the same SO_REUSEPORT address or max elapses,
// and only then closes its sockets. Without it the predecessor's socket closes the
// instant the successor passes its first readiness probe, and every datagram the
// kernel had already queued to that socket is discarded — one lost query per roll,
// which #726 traced to a multi-second stall in every client that singleflights by
// name.
//
// Zero (or negative) disables the window and restores the pre-#729 close-on-SIGTERM
// behaviour.
func WithLameDuck(max time.Duration) Option {
	return func(s *Server) { s.lameDuckMax = max }
}

// WithLameDuckAbort wires a channel that cuts the lame-duck window short when it is
// closed (or receives). The standalone daemon closes it on a SECOND termination signal.
// A nil channel (the default) simply never fires.
func WithLameDuckAbort(ch <-chan struct{}) Option {
	return func(s *Server) { s.lameDuckAbort = ch }
}

// WithInstanceID overrides the random per-process identity stamp this resolver serves
// at instanceQName and compares successor probes against. Production never sets it —
// the point of the stamp is that no two processes can collude on it — but a test needs
// deterministic identities for the two co-bound servers it drives.
func WithInstanceID(id string) Option {
	return func(s *Server) { s.instanceID = id }
}

// newInstanceID mints this process's identity stamp: 64 random bits, hex.
//
// It CANNOT be derived from the pid. The predecessor and successor run in separate
// containers with their own PID namespaces, so both are pid 1 — a pid-based stamp would
// make the successor look like us and the lame duck would always run to its deadline.
func newInstanceID() string {
	var b [8]byte
	// crypto/rand.Read never returns an error (it panics if the system source is
	// unusable), so there is no degraded path to handle here.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// instanceQName is the name this resolver answers its identity stamp at —
// "_instance._aether.<meshDomain>." — as a TXT record with TTL 0.
//
// It is shaped exactly like the self-check probe name (see selfCheckQName) and for the
// same reasons: it sits under the mesh domain so it is answered AUTHORITATIVELY and
// never forwarded to kube-dns, and both labels begin with "_", which is not a legal
// DNS-1123 namespace or Service name, so the record table can never hold a colliding
// key. The difference is the direction: the self-check must never leave the process,
// while this one must — it is asked OVER the wire precisely so the kernel's reuseport
// hash decides which of the co-bound processes answers it.
//
// Any pod on the node can ask for it (the CNI DNATs :53 here) and learn 16 hex
// characters that mean nothing outside this handoff. That is the whole exposure.
func (s *Server) instanceQName() string {
	return instanceLabels + "." + dns.Fqdn(s.meshDomain)
}

// isInstanceName reports whether qname is the identity name. Checked BEFORE isMeshName
// in serve: the identity name is under the mesh domain and would otherwise be answered
// as an ordinary (always missing) mesh record.
func (s *Server) isInstanceName(qname string) bool {
	return strings.EqualFold(dns.Fqdn(qname), s.instanceQName())
}

// serveInstance answers the identity name authoritatively: TXT returns this process's
// stamp, every other type returns NODATA — the same "the name consistently EXISTS"
// contract serveMesh keeps, so a client that also asks A/AAAA is not told the zone is
// broken.
//
// TTL is 0: this is an identity, not a record, and a cached answer would let the
// predecessor's stamp outlive the predecessor.
func (s *Server) serveInstance(w dns.ResponseWriter, r *dns.Msg, q dns.Question) string {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if opt := r.IsEdns0(); opt != nil {
		m.SetEdns0(opt.UDPSize(), opt.Do())
	}
	if q.Qtype == dns.TypeTXT {
		m.Answer = []dns.RR{&dns.TXT{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
			Txt: []string{s.instanceID},
		}}
	}
	_ = w.WriteMsg(m)
	return resultInstance
}

// probeSuccessor asks the reuseport group for its identity stamp ONCE and reports the
// peer's stamp when a DIFFERENT process answered.
//
// This is the entire successor-detection mechanism, and it works because the query goes
// over the WIRE to our own listen address. Both processes hold a socket in the same
// SO_REUSEPORT group, so the kernel hashes the probe's 4-tuple to one of them; a fresh
// ephemeral source port per probe re-rolls that hash. Our own stamp coming back proves
// nothing (we already know we are alive) and is retried; the peer's stamp proves two
// things at once, and both are required before we may close: the successor is IN the
// group (so the kernel will steer to it) and it is ANSWERING (so what it is steered
// actually gets a reply).
//
// No reply at all is also "not yet": the probe itself can lose the reuseport coin flip
// against a socket that is bound but whose handler has not started, which is exactly the
// state we must not close into.
func (s *Server) probeSuccessor(ctx context.Context) (string, bool) {
	addr := probeDialAddr(s.addr)
	if addr == "" {
		return "", false
	}
	req := new(dns.Msg)
	req.SetQuestion(s.instanceQName(), dns.TypeTXT)
	c := &dns.Client{
		Net:          protoUDP,
		DialTimeout:  lameDuckProbeTimeout,
		ReadTimeout:  lameDuckProbeTimeout,
		WriteTimeout: lameDuckProbeTimeout,
	}
	resp, _, err := c.ExchangeContext(ctx, req, addr)
	if err != nil || resp == nil {
		return "", false
	}
	id := instanceIDFrom(resp)
	if id == "" || id == s.instanceID {
		return "", false
	}
	return id, true
}

// probeDialAddr turns the listen address into one the probe can dial. In production the
// resolver binds a concrete HOST_IP, which is dialable as-is; a wildcard bind
// (0.0.0.0 / :: / a bare ":port") is not, so it is dialled through loopback — which
// still lands in the same reuseport group, since the group is keyed by the bound port.
// An unparseable address disables probing (the lame duck then runs to its deadline).
func probeDialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if host == "" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return net.JoinHostPort("127.0.0.1", port)
		}
		return net.JoinHostPort("::1", port)
	}
	return net.JoinHostPort(host, port)
}

// instanceIDFrom extracts the stamp from an identity reply, or "" when the reply
// carries none (a NODATA, or an answer section from something that is not us).
func instanceIDFrom(m *dns.Msg) string {
	for _, rr := range m.Answer {
		if txt, ok := rr.(*dns.TXT); ok && len(txt.Txt) > 0 {
			return strings.Join(txt.Txt, "")
		}
	}
	return ""
}

// lameDuckClock is the lame duck's time source, injected so the state machine can be
// tested without sleeping. Tick returns the channel and its stop function rather than a
// *time.Ticker so a fake can hand back a plain channel.
type lameDuckClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	Tick(d time.Duration) (c <-chan time.Time, stop func())
}

// realClock is the production lameDuckClock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) Tick(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// lameDuckOutcome is why the window ended and how long it lasted.
type lameDuckOutcome struct {
	reason   string
	peer     string
	duration time.Duration
}

// lameDuck is the post-SIGTERM state machine: keep serving, watch for the successor,
// and give up at the deadline. It owns no sockets — closing them is the caller's job,
// and happens only once run returns.
type lameDuck struct {
	max      time.Duration
	interval time.Duration
	selfID   string
	// detect runs one successor probe, returning the peer's identity stamp when a
	// DIFFERENT process answered on our listen address.
	detect func(context.Context) (string, bool)
	abort  <-chan struct{}
	clock  lameDuckClock
	log    *slog.Logger
}

// run holds the listeners open until the successor is demonstrably serving, the
// deadline elapses, or a second signal cuts it short.
//
// ctx here is deliberately NOT the shutdown context (which is already cancelled by the
// time we are called) — it is a detached one, so the probes can still run.
func (d *lameDuck) run(ctx context.Context) lameDuckOutcome {
	start := d.clock.Now()
	d.log.InfoContext(ctx, "lame duck started",
		"instance", d.selfID, "max", d.max, "probeInterval", d.interval)
	deadline := d.clock.After(d.max)
	tick, stopTick := d.clock.Tick(d.interval)
	defer stopTick()
	for {
		select {
		case <-d.abort:
			d.log.InfoContext(ctx, "lame duck aborted by a second termination signal",
				"instance", d.selfID)
			return d.outcome(lameDuckSignal, "", start)
		case <-deadline:
			d.log.InfoContext(ctx, "lame duck deadline reached",
				"instance", d.selfID, "max", d.max)
			return d.outcome(lameDuckDeadline, "", start)
		case <-tick:
			peer, ok := d.detect(ctx)
			if !ok {
				continue
			}
			d.log.InfoContext(ctx, "successor observed",
				"instance", d.selfID, "successor", peer)
			return d.outcome(lameDuckSuccessor, peer, start)
		}
	}
}

// outcome stamps the elapsed window onto a reason.
func (d *lameDuck) outcome(reason, peer string, start time.Time) lameDuckOutcome {
	return lameDuckOutcome{reason: reason, peer: peer, duration: d.clock.Now().Sub(start)}
}

// runLameDuck is Start's post-SIGTERM step: stop reporting ready, keep serving until the
// successor is demonstrably answering (or the deadline), and record the outcome. The
// CALLER closes the listeners afterwards.
//
// Readiness goes first and it is honest, not cosmetic: from this moment on the pod is
// leaving, and the DaemonSet controller's view should say so. It does NOT steer
// datagrams — for a hostNetwork SO_REUSEPORT listener the kernel decides which co-bound
// socket receives each datagram, and it neither knows nor cares what the kubelet thinks
// of the pod. That is precisely why the socket has to stay open.
//
// RESIDUAL (kernel race, not closable from userspace): even after the successor is
// observed, the instant of close() can still discard a datagram the kernel queued to our
// socket between our last recvfrom and the close. Fully removing that needs eBPF
// sk_reuseport steering to drain the departing socket out of the group first. The window
// this narrows the exposure from is "every roll" to that sub-millisecond race.
func (s *Server) runLameDuck(ctx context.Context) {
	if s.lameDuckMax <= 0 {
		return
	}
	s.removeReadyMarker()
	d := &lameDuck{
		max:      s.lameDuckMax,
		interval: lameDuckProbeInterval,
		selfID:   s.instanceID,
		detect:   s.probeSuccessor,
		abort:    s.lameDuckAbort,
		clock:    realClock{},
		log:      s.log,
	}
	// Detached from the shutdown context: it is already cancelled, and every probe
	// issued under it would fail instantly.
	out := d.run(context.WithoutCancel(ctx))
	s.metrics.recordLameDuck(out.reason, out.duration)
}
