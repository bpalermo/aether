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

	// NodeName is the Kubernetes node name where the agent runs (the edge derives
	// it from POD_NAME). It doubles as the xDS node ID of the co-located Envoy —
	// the separate --proxy-id every caller set identically was retired (proposal
	// 031); the snapshot cache and xDS server must agree on one identity.
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

	// EdgeHTTPPort is the port the edge proxy's public-facing HTTP listener
	// binds (edge subcommand only).
	EdgeHTTPPort uint32

	// RouteNamespace is the edge's default namespace for Gateway TLS certificate
	// Secrets (edge subcommand only); empty means the edge pod's own namespace.
	// Gateway/route watching itself is cluster-wide regardless.
	RouteNamespace string

	// Gamma enables GAMMA east-west L7 routing on the node proxy: the agent watches
	// HTTPRoutes parented to a Service and enriches the outbound routes (proposal
	// 018, Phase 2). Default on since proposal 031 (kept as a kill switch); the
	// reconciler degrades gracefully when the Gateway API CRDs are absent.
	Gamma bool

	// ImportConfig enables cross-cluster config import (proposal 026): the agent polls
	// the registrar for GAMMA config projections peer clusters exported and materializes
	// the imported routes into the cache (merged with local; local wins). Default off;
	// no-op when the registry backend has no cross-cluster config plane (etcd only).
	ImportConfig bool

	// GeoipCityDB is the MaxMind city-type mmdb path for the edge geoip filter
	// (proposal 028); empty = geoip off (the x-geo-* strip still applies).
	GeoipCityDB string
	// GeoipHeaders selects which x-geo-* headers the edge emits.
	GeoipHeaders []string
	// XffNumTrustedHops is the edge topology fact (proxies in front): feeds both
	// the HCM and the geoip filter's XFF config.
	XffNumTrustedHops uint32

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

	// EastWestWaypoint enables the split-horizon east/west waypoint rewrite
	// (proposal 019): an endpoint in another cluster is dialed at its node's
	// routable IP + the fixed tunnel port (proxy.DefaultEastWestTunnelPort)
	// instead of its pod IP, and that node's host-network proxy SNI-forwards to
	// the local pod. Intra-cluster traffic stays direct pod-to-pod. Default off.
	//
	// NOTE (proposal 031): transparent capture, the dormant redirect-all
	// passthrough chain, and the L4 route types are UNCONDITIONAL — their old
	// flags encoded install-ordering fears now handled by CRD detection. The
	// remaining capture knobs are the per-pod capture.aether.io/* annotations
	// and the CNI's --capture-redirect-all-default.
	EastWestWaypoint bool

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
}

// NewAgentConfig creates a new AgentConfig with default values.
func NewAgentConfig() *AgentConfig {
	return &AgentConfig{
		Config: manager.Config{
			HealthProbeBindAddress: ":8082",
			MetricsEnabled:         true,
			MetricsBindAddress:     ":8080",
		},
		MeshConfigPath:          DefaultMeshConfigPath,
		EdgeHTTPPort:            proxy.DefaultEdgeHTTPPort,
		EdgeHTTPSPort:           proxy.DefaultEdgeHTTPSPort,
		GatewayClassName:        "aether",
		CNIServerConfig:         cniServer.NewCNIServerConfig(),
		MountedLocalStorageDir:  constants.DefaultHostCNIRegistryDir,
		RegistrarAddress:        "aether-registrar.aether-system.svc:443",
		MeshDomain:              commonconstants.DefaultMeshDomain,
		SpireEnabled:            true,
		SpireAdminSocketPath:    constants.DefaultSpireAdminSocketPath,
		SpireWorkloadSocketPath: constants.DefaultSpireWorkloadSocketPath,
	}
}
