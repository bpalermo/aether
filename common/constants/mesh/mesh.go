// Package mesh defines the cross-tree mesh data-plane ports, paths, domain, and
// netfilter marks shared by the agent, CNI plugin, and registrar.
package mesh

const (
	// ProxyOutboundPort is the port the per-pod outbound HTTP capture listener
	// binds inside each pod's network namespace. The CNI plugin probes this
	// address (from within the netns) to confirm the data plane is serving.
	ProxyOutboundPort = 18081
	// ProxyTCPOutboundPort is the mesh's well-known TCP spelling: a client dials
	// <svc>.<ns>.<mesh-domain>:18082 to reach the service's default TCP port
	// (proposal 037). It is the L4 counterpart of ProxyOutboundPort.
	//
	// Why a second port rather than resolving the bare name by the service's
	// protocol: HTTP demuxes on the authority header, which carries its own port
	// as a string, so `http://<svc>/` is unambiguous without one. Raw TCP has no
	// authority — its only demux key is the 5-tuple — so it needs a port, and
	// without a well-known one the meaning of the bare name would depend on
	// whichever protocol the service's PRIMARY port happened to be. That made a
	// client's spelling change meaning when a service was re-annotated.
	//
	// The capture listener matches it with `destination_port 18082` on top of the
	// ClusterIP /32, which Envoy evaluates ahead of prefix_ranges and which
	// use_original_dst already recovers. Unlike a per-app-port TCP dial, this one
	// does NOT require redirect-all: the scoped CNI rule redirects it alongside
	// ProxyOutboundPort, and the generated mesh Service exposes it, so it also
	// resolves through kube-proxy rather than hanging.
	//
	// 18082 was free tree-wide when chosen; in-use 18xxx are 18001, 18008, 18009,
	// 18021, 18054, 18080 (e2e fixtures) and 18081.
	ProxyTCPOutboundPort = 18082
	// ProxyCapturePort is the port both the per-pod TCP and UDP capture listeners
	// bind inside the pod netns (proposal 018, Phase 3a/3b). TCP and UDP are
	// independent at the socket layer — a UDP socket and a TCP socket can both
	// bind the same port number — so a single port serves both protocols (like
	// HTTP/3 running TCP+QUIC on :443). The same one-number-both-transports rule
	// binds the east-west tunnel port (proxy.DefaultEastWestTunnelPort, 18009):
	// east-west QUIC shares it rather than taking a new one. The CNI redirects outbound TCP to a mesh
	// ClusterIP:ProxyOutboundPort to this port (Phase 3a, TCP listener); it also
	// redirects outbound UDP to this same port (Phase 3b, UDP listener). Default
	// off. In aether's 18xxx range (with ProxyOutboundPort) to avoid colliding
	// with Istio's 15001 outbound-capture port if both meshes share a node.
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
	// matches this mark with a `meta mark → RETURN` rule ahead of the redirect, so
	// the proxy's OWN forwarded egress is never re-captured into a loop. SO_MARK
	// (not a UID match) is used because the proxy runs as root — a UID rule would
	// wrongly exempt any root-running app pod. Distinct from Istio's 1337 so the two
	// meshes' marks don't collide if they share a node. Requires CAP_NET_ADMIN
	// (the agent/proxy container has it).
	CapturePassthroughFwMark = 0xae7e
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
