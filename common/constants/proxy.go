package constants

const (
	// ProxyOutboundPort is the port the per-pod outbound HTTP capture listener
	// binds inside each pod's network namespace. The CNI plugin probes this
	// address (from within the netns) to confirm the data plane is serving.
	ProxyOutboundPort = 18081
	// ProxyCapturePort is the port the per-pod transparent-capture listener binds
	// inside the pod netns (proposal 018, Phase 3a). The CNI redirects outbound
	// TCP to a mesh ClusterIP:ProxyOutboundPort here; the listener recovers the
	// original ClusterIP and routes by cluster.local authority. Default off.
	// In aether's 18xxx range (with ProxyOutboundPort) to avoid colliding with
	// Istio's 15001 outbound-capture port if both meshes share a node.
	ProxyCapturePort = 18001
	// ProxyDNSCapturePort is the port the per-pod Envoy DNS listeners (UDP+TCP) bind
	// inside the pod netns (proposal 018, mesh-global FQDN). The CNI redirects the
	// pod's outbound DNS (:53) here; the listeners udp_proxy/tcp_proxy it to the
	// agent's in-process resolver (the mesh_dns cluster). In aether's 18xxx range to
	// avoid colliding with Istio's 15053 DNS port.
	ProxyDNSCapturePort = 18053
	// ProxyDNSResolverPort is the host port the node agent's in-process mesh-DNS
	// resolver listens on (UDP+TCP). The per-pod DNS listeners' mesh_dns cluster
	// targets HOST_IP:ProxyDNSResolverPort.
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
