// Package cmd provides command-line interface configuration for the Aether agent.
package cmd

import (
	"time"

	"github.com/bpalermo/aether/agent/constants"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/manager"
)

// DefaultMeshConfigPath is where the chart mounts the MeshConfig ConfigMap.
const DefaultMeshConfigPath = "/etc/aether/mesh-config.yaml"

// AgentConfig holds configuration for the Aether agent.
//
// Aether system config (mesh domain, SPIRE on/off, and the embedded manager OTEL
// fields) is inherited from the aether umbrella chart's globals as flags. The
// proxy data-plane observability fields (access logs, tracing, stats-pod) are the
// only ones loaded from the MeshConfig ConfigMap at MeshConfigPath, defaulting to
// the system config when unset. See docs/proposals/015_mesh-config.md.
type AgentConfig struct {
	manager.Config

	// MeshConfigPath is the path to the mounted proxy MeshConfig YAML (ConfigMap).
	MeshConfigPath string

	// ProxyServiceNodeID is the xDS node ID for identifying the Envoy proxy instance
	ProxyServiceNodeID string

	// NodeName is the Kubernetes node name where the agent runs
	NodeName string
	// ClusterName is the Kubernetes cluster name
	ClusterName string

	// MountedLocalStorageDir is the directory where pod data is stored locally
	MountedLocalStorageDir string

	// RegistrarAddress is the gRPC address of the in-cluster Registrar service
	RegistrarAddress string

	// MeshDomain is the DNS-style domain mesh authorities live under
	// (<service>.<mesh-domain>); see constants.DefaultMeshDomain.
	MeshDomain string

	// EmitStatsPod enables per-pod labels (source_pod/destination_pod) on the
	// aether_stats request counter. Off by default to bound cardinality.
	EmitStatsPod bool

	// AccessLogsEnabled attaches the OTel access logger to every HCM (proposal
	// 014), pushing per-request OTLP logs to the collector. Off by default.
	AccessLogsEnabled bool
	// AccessLogSuccessSampleRate is the percent (0-100) of successful requests
	// logged; failures (any response flag / status >= 500) are always logged.
	AccessLogSuccessSampleRate uint32

	// ProxyTracingEnabled adds an OpenTelemetry tracer to every proxy HCM so the
	// data plane generates/propagates W3C trace context and exports spans.
	ProxyTracingEnabled bool
	// ProxyTraceSampleRate is the fraction (0.0-1.0) of requests traced. Keep low
	// at high data-plane QPS.
	ProxyTraceSampleRate float64

	// SpireEnabled controls whether the SPIRE bridge is started
	SpireEnabled bool
	// SpireAdminSocketPath is the path to the SPIRE agent admin socket
	SpireAdminSocketPath string
	// SpireWorkloadSocketPath is the path to the SPIRE Workload API UDS socket
	SpireWorkloadSocketPath string

	// CNIServerConfig holds CNI server configuration
	CNIServerConfig *cniServer.CNIServerConfig

	// RemoveStartupTaint removes the aether.io/agent-not-ready node taint once the
	// CNI server is serving (node proxy only). Default true.
	RemoveStartupTaint bool

	// EdgeHTTPPort is the port the edge proxy's public-facing HTTP listener
	// binds (edge subcommand only).
	EdgeHTTPPort uint32

	// RouteNamespace is the namespace the edge watches its Gateways/HTTPRoutes in
	// (edge subcommand only); empty means the edge pod's own namespace.
	RouteNamespace string

	// Gamma enables GAMMA east-west L7 routing on the node proxy: the agent watches
	// HTTPRoutes parented to a Service and enriches the outbound routes (proposal
	// 018, Phase 2). Default off — a no-op until enabled.
	Gamma bool

	// ImportConfig enables cross-cluster config import (proposal 026): the agent polls
	// the registrar for GAMMA config projections peer clusters exported and materializes
	// the imported routes into the cache (merged with local; local wins). Default off;
	// no-op when the registry backend has no cross-cluster config plane (etcd only).
	ImportConfig bool

	// AuthzSidecar enables the node-local external-authorization sidecar entry
	// (proposal 027): a disabled ext_authz HCM filter targeting the chart's static
	// authz_sidecar UDS cluster; HTTPFilter (extAuthz) opts routes in.
	AuthzSidecar bool
	// AuthzSidecarTimeout is the per-check gRPC timeout.
	AuthzSidecarTimeout time.Duration
	// AuthzSidecarFailureModeAllow selects fail-open (true) vs fail-closed (false,
	// requests on opted-in routes are denied when the sidecar is unreachable).
	AuthzSidecarFailureModeAllow bool

	// ControlCluster, when set, names the single authorized config-exporting cluster
	// (proposal 026 EM3, Option E): the importer then trusts ONLY config originating
	// there and ignores all other origins. Empty = federated (trust any peer origin).
	ControlCluster string

	// L4Routes enables L4 route types (TCPRoute/TLSRoute/UDPRoute parentRef=Service,
	// proposal 018, Phase 3b): the agent watches these route types and projects
	// weighted TCP floor chains and SNI-routed TLS chains onto the capture listener.
	// Requires --transparent-capture to be meaningful. Default off.
	// NOTE: UDPRoute is control-plane only until the CNI UDP redirect lands.
	L4Routes bool

	// TransparentCapture enables transparent capture (proposal 018, Phase 3a): the
	// agent generates per-pod capture listeners + the cap_http route table and
	// watches the generated mesh Services for their cluster.local authorities. Pairs
	// with the registrar's --generate-mesh-services and the CNI dst-18081 redirect.
	// Default off.
	TransparentCapture bool

	// CaptureRedirectAll adds the dormant ORIGINAL_DST passthrough DefaultFilterChain
	// to capture listeners (proposal 022 M2a spike). Harmless for pods that are not
	// redirect-all'd (only :18081 reaches their capture listener, which matches the
	// HCM/mesh chain, so the passthrough never fires). The per-pod CNI redirect-all
	// (gated by the capture.aether.io/redirect-all annotation) is what actually sends
	// all egress into the listener. Default off.
	CaptureRedirectAll bool

	// MeshDNS enables the per-pod mesh-DNS listener (proposal 018, mesh-global FQDN):
	// the agent answers <svc>.<meshDomain> from the generated mesh Services' ClusterIPs
	// and forwards the rest to MeshDNSUpstream. Pairs with the CNI :53 redirect.
	// Default off.
	MeshDNS bool
	// MeshDNSUpstream is the upstream resolver(s) (host[:port]) the mesh-DNS filter
	// forwards non-mesh queries to — the cluster kube-dns.
	MeshDNSUpstream []string
	// GatewayClassName is the GatewayClass whose Gateways this edge serves.
	GatewayClassName string

	// EdgeServiceName is the name of the edge's own LoadBalancer Service (in the
	// edge's namespace). The edge resolves its assigned LB address from that
	// Service's status and publishes it as every class-aether Gateway's
	// status.addresses (proposal 021 Phase 1, shared edge address). Empty disables
	// address publication (edge subcommand only).
	EdgeServiceName string

	// EdgeTLS enables downstream TLS termination: the edge serves an HTTPS
	// listener on EdgeHTTPSPort (certs per Gateway listener via SDS) and an
	// HTTP->HTTPS redirect on the plain port (edge subcommand only).
	EdgeTLS bool
	// EdgeHTTPSPort is the port the edge TLS listener binds when EdgeTLS is set.
	EdgeHTTPSPort uint32

	// EdgeReadinessPort is the port the dedicated always-bound readiness listener
	// binds; the kubelet readiness probe targets it over plain HTTP. Independent of
	// the public listeners so the probe survives proposal 021 Phase 2 (edge only).
	EdgeReadinessPort uint32

	// EdgePerGatewayAddressing enables proposal 021 Phase 2: a per-Gateway
	// LoadBalancer Service + internal-port demux so each class-aether Gateway
	// gets its own external IP. When false, falls back to Phase 1 (single shared
	// edge LB IP for all Gateways). Default: true (edge subcommand only).
	EdgePerGatewayAddressing bool
}

// NewAgentConfig creates a new AgentConfig with default values.
func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Config: manager.Config{
			HealthProbeBindAddress: ":8082",
			MetricsEnabled:         true,
			MetricsBindAddress:     ":8080",
		},
		MeshConfigPath:           DefaultMeshConfigPath,
		ProxyServiceNodeID:       constants.DefaultProxyID,
		EdgeHTTPPort:             proxy.DefaultEdgeHTTPPort,
		EdgeHTTPSPort:            proxy.DefaultEdgeHTTPSPort,
		EdgeReadinessPort:        proxy.DefaultEdgeReadinessPort,
		EdgePerGatewayAddressing: true,
		GatewayClassName:         "aether",
		CNIServerConfig:          cniServer.NewCNIServerConfig(),
		MountedLocalStorageDir:   constants.DefaultHostCNIRegistryDir,
		RegistrarAddress:         "aether-registrar.aether-system.svc:443",
		MeshDomain:               commonconstants.DefaultMeshDomain,
		SpireEnabled:             true,
		SpireAdminSocketPath:     constants.DefaultSpireAdminSocketPath,
		SpireWorkloadSocketPath:  constants.DefaultSpireWorkloadSocketPath,
		RemoveStartupTaint:       true,
	}
}
