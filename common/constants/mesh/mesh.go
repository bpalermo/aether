// Package mesh defines the cross-tree mesh data-plane ports, paths, domain, and
// netfilter marks shared by the agent, CNI plugin, and registrar.
package mesh

const (
	// ProxyOutboundPort is the port the per-pod outbound HTTP capture listener
	// binds inside each pod's network namespace. The CNI plugin probes this
	// address (from within the netns) to confirm the data plane is serving.
	ProxyOutboundPort = 18081
	// ProxyL4OutboundPort is the mesh's well-known L4 spelling, for BOTH
	// transports: a client dials <svc>.<ns>.<mesh-domain>:18082 to reach the
	// service's default raw-TCP port (proposal 037) or, since proposal 038 D1,
	// its plaintext UDP port. It is the L4 counterpart of ProxyOutboundPort.
	//
	// Why a second port rather than resolving the bare name by the service's
	// protocol: HTTP demuxes on the authority header, which carries its own port
	// as a string, so `http://<svc>/` is unambiguous without one. Raw TCP and UDP
	// have no authority — their only demux key is the 5-tuple — so they need a
	// port, and without a well-known one the meaning of the bare name would
	// depend on whichever protocol the service's PRIMARY port happened to be.
	//
	// ONE NUMBER, BOTH TRANSPORTS. TCP and UDP are independent socket families,
	// so a TCP listener and a UDP listener each bind 18082 on their own socket,
	// the way HTTP/3 runs TCP+QUIC on :443 and the way the inbound (18008) and
	// east-west gateway (18009) ports are shared. A second number for UDP would
	// mean a second CNI rule, a second Service port and a second capture path.
	// Pinned by TestL4OutboundPortIsSharedAcrossTransports: a distinct
	// UDP/QUIC outbound port constant in this package fails the build.
	//
	// (Renamed from ProxyL4OutboundPort when UDP joined it; the value is
	// unchanged.) 18082 was free tree-wide when chosen; in-use 18xxx are 18001,
	// 18008, 18009, 18021, 18054, 18080 (e2e fixtures) and 18081.
	ProxyL4OutboundPort = 18082
	// ProxyCapturePort is the port the per-pod TCP capture listener binds inside
	// the pod netns (proposal 018, Phase 3a). Under TPROXY capture (proposal 038)
	// the CNI diverts captured TCP to it with `tproxy to :18001` while leaving
	// the IP header intact, so ONE listener here serves every captured port —
	// 18081, 18082 and, under redirect-all, any port — and the accepted socket's
	// local endpoint is still the original destination. In aether's 18xxx range
	// (with ProxyOutboundPort) to avoid colliding with Istio's 15001
	// outbound-capture port if both meshes share a node.
	//
	// UDP does NOT bind this port. A datagram listener's reply source port is
	// always its bound port, so the UDP capture listener must bind the port the
	// client dialed — ProxyL4OutboundPort, 18082 — and no port rewrite is
	// possible for it. (Before 038 both transports bound 18001 via REDIRECT;
	// that is the design #916 retired.)
	//
	// SECURITY NOTE: UDP datagrams routed via the UDP capture listener are NOT
	// protected by mesh mTLS. mTLS is a TCP/TLS construct; DTLS is not
	// implemented in this mesh. UDP traffic is forwarded in plaintext to the
	// backend pods. This is a known limitation of the UDP floor — see proposal
	// 018 Phase 3b.
	ProxyCapturePort = 18001
	// CapturePassthroughFwMark is the netfilter fwmark Envoy stamps (via SO_MARK on
	// the passthrough_original_dst cluster's upstream sockets) on connections it
	// forwards out of the redirect-all capture (proposal 022, M2-default). The CNI
	// accepts this mark ahead of the capture rules so the proxy's OWN forwarded
	// egress is never re-captured into a loop. SO_MARK (not a UID match) is used
	// because the proxy runs as root — a UID rule would wrongly exempt any
	// root-running app pod. Distinct from Istio's 1337 so the two meshes' marks
	// don't collide if they share a node. Requires CAP_NET_ADMIN.
	//
	// In practice this never matches: the proxy is hostNetwork, so its upstream
	// sockets live in the HOST netns while the capture rules live in each POD
	// netns. It is kept as a defensive first rule (proposal 022: "harmless if the
	// passthrough egresses proxy-side").
	CapturePassthroughFwMark = 0xae7e
	// CaptureDivertFwMark is the fwmark the CNI's OUTPUT rule sets on a captured
	// packet under TPROXY capture (proposal 038). A policy-routing rule
	// (fwmark → CaptureDivertRouteTable, whose only route is `local default dev
	// lo`) then delivers the packet locally with its IP header INTACT, and a
	// prerouting `tproxy` rule hands it to the transparent capture socket. This
	// replaces the nat REDIRECT, which rewrote the destination and so lost it.
	//
	// Same 0xae7x family as the passthrough mark so the two read as related in
	// `nft list ruleset`; a DISTINCT value, and neither is a bitmask superset of
	// the other, so the passthrough accept can never match a diverted packet and
	// the divert rule can never re-mark a passthrough. Disjoint from kube-proxy's
	// masked 0x4000/0x8000 marks. A marked packet is always routed to lo and
	// never leaves the pod netns (crossing a veth scrubs skb->mark regardless).
	// Pinned by TestCaptureMarksAreDisjoint.
	CaptureDivertFwMark = 0xae71
	// CaptureDivertRouteTable is the policy-routing table the divert rule looks
	// up. Any fixed id above the main table works; 100 matches the kernel TPROXY
	// documentation's own example and the Phase 0/0b spikes.
	CaptureDivertRouteTable = 100
	// ProxyDNSResolverPort is the host port the node agent's in-process mesh-DNS
	// resolver listens on (UDP+TCP) at HOST_IP (proposal 018, mesh-global FQDN). The
	// CNI DNATs each pod's outbound :53 straight to HOST_IP:ProxyDNSResolverPort. In
	// aether's 18xxx range to avoid colliding with Istio's 15053 DNS port.
	ProxyDNSResolverPort = 18054
	// ProxyReadinessPath is the path matched by the non-pass-through
	// health_check filter on every outbound listener. A 200 proves the listener
	// is active on worker threads in that netns; a 503 means the answering
	// Envoy epoch is draining (hot restart) and the probe should retry.
	ProxyReadinessPath = "/aether/readyz"

	// DefaultMeshDomain is the DNS-style domain mesh authorities live under.
	// Clients address services as <service>.<mesh-domain> (the Host header on
	// the outbound listener); it is also the data-plane cluster name, the
	// service vhost domain, and the on-demand (ODCDS) catch-all suffix
	// (*.<mesh-domain>). Authorities outside the domain 404 deterministically
	// at the route table. Configurable via the agent's --mesh-domain flag.
	DefaultMeshDomain = "aether.internal"
)
