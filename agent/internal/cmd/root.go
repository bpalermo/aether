// Package cmd provides command-line interface and configuration for the Aether agent.
//
// The agent is a Kubernetes DaemonSet component that runs on each node to manage
// an Envoy xDS control plane and a CNI plugin for transparent traffic interception.
// It connects to the in-cluster Registrar service for endpoint registration and discovery,
// and generates Envoy configuration (listeners, clusters, endpoints, routes) for local pods.
//
// Command-line flags control the agent's initialization:
//   - debug: Enable debug-level logging
//   - node-name: Kubernetes node name where the agent runs (required); also the
//     xDS node identity of the co-located Envoy proxy
//   - cluster-name: Kubernetes cluster name (required)
//   - mounted-registry-dir: Directory where pod data is stored locally
//   - registrar-address: gRPC address of the in-cluster Registrar service
//   - spire-enabled: Whether to enable SPIRE integration for mTLS via SDS
//   - spire-admin-socket: Path to SPIRE agent admin socket
//
// The agent uses controller-runtime Manager to orchestrate multiple runnables:
// xDS server (serves Envoy discovery requests over Unix domain socket), CNI gRPC server
// (handles pod registration/deregistration), SPIRE bridge (optional, manages X.509 certificates),
// and registry (service endpoint discovery).
package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/capture"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/internal/configimport"
	"github.com/bpalermo/aether/agent/internal/gamma"
	"github.com/bpalermo/aether/agent/internal/l4route"
	"github.com/bpalermo/aether/agent/internal/node"
	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/config"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/must"
	commonspire "github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/registry"
	"github.com/bpalermo/aether/registry/registrarclient"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	// name is the controller name used for logging
	name = "aether-agent"
)

// Version is set at build time via -ldflags (Bazel x_defs).
var Version = "dev"

var (
	cfg = NewAgentConfig()

	l           *slog.Logger
	logShutdown func(context.Context) error
)

var rootCmd = &cobra.Command{
	Use:          "agent",
	Short:        "Runs the aether node agent.",
	Long:         "Runs the Aether agent on a Kubernetes node to manage an Envoy xDS control plane and transparent traffic interception via CNI.",
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) (err error) {
		// Load mesh-wide policy before logging setup: the OTLP endpoint and log
		// export it configures both live in the MeshConfig now.
		mc, err := config.Load(cfg.MeshConfigPath)
		if err != nil {
			return err
		}
		applyMeshConfig(mc)

		l, logShutdown, err = manager.SetupManagerLogging(cmd.Context(), cfg.Config, name, Version)
		return err
	},
	RunE: func(cmd *cobra.Command, _ []string) (err error) {
		return runAgent(cmd.Context())
	},
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func init() {
	manager.RegisterFlags(rootCmd, &cfg.Config)
	registerSharedFlags(rootCmd, true)

	// Local storage configuration (node proxy only — the edge runs no CNI/storage).
	rootCmd.Flags().StringVar(&cfg.MountedLocalStorageDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where pod data is stored locally for the CNI plugin")

	// SPIRE admin socket (node proxy only — delegated identity for many local
	// workloads; the edge presents a single identity served by SPIRE directly).
	rootCmd.Flags().StringVar(&cfg.SpireAdminSocketPath, "spire-admin-socket", constants.DefaultSpireAdminSocketPath, "Path to SPIRE agent admin socket for X.509 certificate delegation")

	rootCmd.Flags().BoolVar(&cfg.Gamma, "gamma", true, "GAMMA east-west L7 routing: watch HTTPRoutes parented to a Service and enrich the node proxy's outbound routes (proposal 018, Phase 2). Default on (kill switch, proposal 031); degrades gracefully when the Gateway API CRDs are absent")
	rootCmd.Flags().BoolVar(&cfg.ImportConfig, "import-config", false, "Enable cross-cluster config import: poll the registrar for GAMMA config projections peer clusters exported and materialize them into the node proxy's routes (proposal 026). No-op unless the registry backend has a cross-cluster config plane (etcd)")
	rootCmd.Flags().BoolVar(&cfg.AuthzSidecar, "authz-sidecar", false, "Enable the node-local external-authorization sidecar entry (proposal 027): a disabled ext_authz HCM filter targeting the static authz_sidecar UDS cluster; HTTPFilter (extAuthz) opts routes in")
	rootCmd.Flags().DurationVar(&cfg.AuthzSidecarTimeout, "authz-sidecar-timeout", 200*time.Millisecond, "Per-check gRPC timeout for the authz sidecar")
	rootCmd.Flags().BoolVar(&cfg.AuthzSidecarFailureModeAllow, "authz-sidecar-failure-mode-allow", false, "Fail-open: allow requests when the authz sidecar is unreachable (default fail-closed: deny)")
	rootCmd.Flags().StringVar(&cfg.ControlCluster, "control-cluster", "", "Name of the single authorized config-exporting cluster (proposal 026 EM3, Option E). When set, imported config is trusted ONLY from this origin; empty = federated (trust any peer)")
	rootCmd.Flags().BoolVar(&cfg.EastWestWaypoint, "east-west-waypoint", false, "Enable the split-horizon east/west waypoint (proposal 019): dial cross-cluster endpoints at their node's routable IP + the fixed tunnel port (18009) instead of their pod IP, and SNI-forward to the local pod. Intra-cluster stays direct pod-to-pod. Needs cross-cluster endpoint visibility (shared etcd) and a shared SPIRE trust domain.")
	rootCmd.Flags().BoolVar(&cfg.MeshDNS, "mesh-dns", false, "Enable per-pod mesh DNS: answer <svc>.<mesh-domain> from the generated mesh Services and forward the rest to --mesh-dns-upstream (proposal 018, mesh-global FQDN)")
	rootCmd.Flags().StringSliceVar(&cfg.MeshDNSUpstream, "mesh-dns-upstream", cfg.MeshDNSUpstream, "Upstream resolver(s) (host[:port]) the mesh-DNS filter forwards non-mesh queries to (the cluster kube-dns)")
	rootCmd.Flags().StringVar(&cfg.MeshDNSSnapshotPath, "mesh-dns-snapshot-path", cfg.MeshDNSSnapshotPath, "Host-persistent file the in-process mesh-DNS resolver persists its last-known record table to and warm-loads at boot, closing the agent-roll cold window (proposal 018, mesh-global FQDN). Defaults under the CNI registry hostPath so it survives a rolling restart; empty disables persistence")
}

// registerSharedFlags registers the flags common to the node agent (root) and
// the edge subcommand: cluster/node identity, the registrar address, the mesh
// system config inherited from the umbrella chart, and the SPIRE workload
// socket. Each command binds them onto the shared cfg; cobra runs the root's
// PersistentPreRunE (mesh-config load + logging) for the subcommand too.
// requirePodIdentity marks node-name required (the node agent — node-name is
// the K8s node, not derivable); the edge passes false and derives it from the
// POD_NAME downward-API env instead. node-name doubles as the xDS node ID —
// the two were one flag pair (--proxy-id) that every caller set identically,
// retired by proposal 031.
func registerSharedFlags(cmd *cobra.Command, requirePodIdentity bool) {
	// Aether system config (OTEL enable/endpoint via manager.RegisterFlags, mesh
	// domain, SPIRE on/off) is inherited from the aether umbrella chart's globals
	// as flags. The proxy data plane can override its own observability via the
	// MeshConfig ConfigMap.
	cmd.Flags().StringVar(&cfg.MeshConfigPath, "mesh-config", cfg.MeshConfigPath, "Path to the mounted proxy MeshConfig YAML (ConfigMap) with proxy observability overrides")
	cmd.Flags().StringVar(&cfg.MeshDomain, "mesh-domain", cfg.MeshDomain, "DNS-style domain mesh authorities live under (clients call <service>.<mesh-domain>)")
	cmd.Flags().BoolVar(&cfg.SpireEnabled, "spire-enabled", cfg.SpireEnabled, "Enable SPIRE integration for X.509 SVID management and mTLS")

	// Kubernetes and cluster identity (required)
	cmd.Flags().StringVar(&cfg.NodeName, "node-name", "", "Kubernetes node name (node agent) or edge pod identity (edge); used as the xDS/watch identity (required)")
	cmd.Flags().StringVar(&cfg.ClusterName, "cluster-name", "", "Kubernetes cluster name, used for service discovery (required)")

	// Registrar configuration
	cmd.Flags().StringVar(&cfg.RegistrarAddress, "registrar-address", cfg.RegistrarAddress, "gRPC address of the in-cluster Registrar service")

	// SPIRE workload socket (per-instance; the SPIRE on/off policy is system-wide).
	cmd.Flags().StringVar(&cfg.SpireWorkloadSocketPath, "spire-workload-socket", constants.DefaultSpireWorkloadSocketPath, "Path to the SPIRE Workload API UDS socket used for registrar mTLS")

	// These calls only fail if the flag name is not registered, which would be a programming error.
	must.NoError(cmd.MarkFlagRequired("cluster-name"))
	if requirePodIdentity {
		must.NoError(cmd.MarkFlagRequired("node-name"))
	}
}

// applyMeshConfig applies the proxy observability overrides from the MeshConfig
// onto the agent config. Unset overrides inherit the aether system config: the
// enable toggles default off, the trace sample rate inherits --trace-sample-rate,
// and the access-log success sample rate defaults to 100. System config (mesh
// domain, OTEL, SPIRE) comes from flags and is not touched here.
func applyMeshConfig(mc *configv1.MeshConfigSpec) {
	p := mc.GetProxy()

	cfg.AccessLogsEnabled = p.GetAccessLogsEnabled()
	cfg.AccessLogSuccessSampleRate = 100
	if p != nil && p.HasAccessLogSuccessSampleRate() {
		cfg.AccessLogSuccessSampleRate = p.GetAccessLogSuccessSampleRate()
	}

	cfg.ProxyTracingEnabled = p.GetTracingEnabled()
	cfg.ProxyTraceSampleRate = cfg.TraceSampleRate // inherit the aether trace rate
	if p != nil && p.HasTraceSampleRate() {
		cfg.ProxyTraceSampleRate = p.GetTraceSampleRate()
	}

	cfg.EmitStatsPod = p.GetEmitStatsPod()
}

// runAgent initializes and runs the Aether agent. It sets up the controller-runtime Manager,
// initializes local storage and registry backends, creates and registers the xDS server,
// CNI gRPC server, and optionally the SPIRE bridge as runnables. The agent then waits
// for local storage to become ready before starting the manager's event loop.
func runAgent(ctx context.Context) (retErr error) {
	l.InfoContext(
		ctx, "starting aether agent",
		"nodeName", cfg.NodeName,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
	)

	// Flush and stop the OTLP log exporter last (registered first → runs last),
	// so records emitted during the rest of shutdown are still exported. No-op
	// when OTLP logging is disabled.
	defer deferLogShutdown(ctx)

	// Scope the manager's Pod informer to this node: the agent only ever reads
	// (CNI ADD enrichment) and watches (termination drain) its own node's pods,
	// and an unscoped Pod cache on a DaemonSet means every agent caching every
	// pod in the cluster.
	cfg.CacheOptions = &ctrlcache.Options{
		ByObject: map[client.Object]ctrlcache.ByObject{
			&corev1.Pod{}: {Field: fields.OneTermEqualSelector("spec.nodeName", cfg.NodeName)},
		},
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, name, Version)
	if err != nil {
		return err
	}
	defer deferTelemetryShutdown(ctx, result.Shutdown)

	m := result.Manager

	localStorage, err := setupStorage(ctx, cfg.MountedLocalStorageDir)
	if err != nil {
		return err
	}

	spireSource, identityTrustDomain, err := setupSpireSource(ctx)
	if err != nil {
		return err
	}
	if spireSource != nil {
		defer func() { retErr = errors.Join(retErr, spireSource.Close()) }()
	}

	reg, err := setupRegistrarClient(ctx, spireSource)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reg.Close()) }()

	snapshotCache, err := configureSnapshotCache(ctx, m)
	if err != nil {
		return err
	}

	ackTracker := ack.NewTracker(l)

	spireBridge, err := wireSpireBridge(ctx, m, snapshotCache, spireSource)
	if err != nil {
		return err
	}

	if err = setXDSServer(ctx, m, reg, localStorage, snapshotCache, ackTracker, identityTrustDomain); err != nil {
		return err
	}

	if err = setupCNIServer(m, localStorage, reg, snapshotCache, ackTracker, spireBridge, identityTrustDomain); err != nil {
		return err
	}

	// Remove the aether startup taint from this node whenever it is present and
	// the CNI server is serving, so workload pods (which don't tolerate it) can
	// schedule here. This is a reconciler on this agent's own Node, so it also
	// drops the taint the controller's node-taint guard re-applies after a reboot
	// or agent outage (issue #569, gaps G1/G2), not just the one the kubelet
	// registered at boot. Unconditional (proposal 031): a no-op when the taint
	// isn't present. Best-effort: NeedLeaderElection=false, never fails startup.
	if err = (&node.TaintRemover{
		Client:     m.GetClient(),
		NodeName:   cfg.NodeName,
		SocketPath: cfg.CNIServerConfig.SocketPath,
		Log:        l,
	}).SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up startup-taint remover: %w", err)
	}

	// Rebuild the cluster/endpoint/route snapshot when the registry reports
	// endpoint changes, so services registered after startup become routable
	// without restarting the agent. No-op if the registry can't notify.
	refresher := xdsServer.NewRegistryRefresher(cfg.ClusterName, cfg.NodeName, snapshotCache, reg, l)
	if err = m.Add(refresher); err != nil {
		return fmt.Errorf("failed to add registry refresher: %w", err)
	}

	if err = wireGAMMA(m, snapshotCache); err != nil {
		return err
	}

	wireAuthzAndConfigImport(ctx, m, reg, snapshotCache)

	if err = wireL4Routes(m, snapshotCache); err != nil {
		return err
	}

	if err = wireCaptureReconciler(m, snapshotCache); err != nil {
		return err
	}

	l.DebugContext(ctx, "waiting for local storage to be ready")
	if err = localStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.DebugContext(ctx, "local storage is ready, starting manager")
	return m.Start(ctx)
}

// setupSpireSource opens the SPIRE Workload API source and resolves the trust domain.
// When SPIRE is disabled, it returns nil source and the mesh domain as trust domain.
// The caller is responsible for closing the returned source via defer.
func setupSpireSource(ctx context.Context) (*commonspire.Source, string, error) {
	if !cfg.SpireEnabled {
		return nil, cfg.MeshDomain, nil
	}
	// When SPIRE is enabled, open the Workload API source and resolve the real
	// trust domain SPIRE issues into (e.g. "aether.internal"). That trust domain
	// names the SDS resources / SPIFFE IDs the agent programs into Envoy so they
	// match the secrets the SPIRE bridge delivers. Peer authorization uses the
	// mesh domain — addressing (<svc>.<mesh-domain>) and identity
	// (spiffe://<mesh-domain>/...) are one domain by design, never split.
	spireSource, err := commonspire.NewSource(ctx, cfg.SpireWorkloadSocketPath)
	if err != nil {
		return nil, "", err
	}
	identityTrustDomain, err := commonspire.TrustDomainFromSource(spireSource)
	if err != nil {
		_ = spireSource.Close()
		return nil, "", fmt.Errorf("failed to resolve SPIRE trust domain: %w", err)
	}
	l.InfoContext(ctx, "resolved workload trust domain from SPIRE", "trustDomain", identityTrustDomain)
	return spireSource, identityTrustDomain, nil
}

// configureSnapshotCache creates and configures the xDS snapshot cache, including
// optional mesh-DNS server wiring. Global access-log and tracing configs are also
// set here before the cache builds any listener.
func configureSnapshotCache(ctx context.Context, m ctrl.Manager) (*cache.SnapshotCache, error) {
	snapshotCache := cache.NewSnapshotCache(cfg.NodeName, l)
	snapshotCache.SetMeshDomain(cfg.MeshDomain)
	snapshotCache.SetEmitStatsPod(cfg.EmitStatsPod)
	// With SPIRE off no SVIDs/SDS exist; the cache builds the per-pod inbound
	// listener cleartext so the mesh hop stays routable (the outbound clusters
	// already go cleartext without a node SVID). Production keeps mTLS.
	snapshotCache.SetSpireEnabled(cfg.SpireEnabled)
	// Capture + the dormant redirect-all passthrough chain are unconditional
	// (proposal 031); per-pod behavior is decided by the CNI (annotations +
	// --capture-redirect-all-default). The tunnel port is the fixed constant.
	snapshotCache.SetCaptureEnabled(true)
	snapshotCache.SetCaptureRedirectAll(true)
	snapshotCache.SetWaypointConfig(cfg.EastWestWaypoint, proxy.DefaultEastWestTunnelPort)

	wireMeshDNS(snapshotCache)

	// Global access-log config, set once before the cache builds any listener.
	proxy.SetAccessLogConfig(proxy.AccessLogConfig{
		Enabled:           cfg.AccessLogsEnabled,
		SuccessSampleRate: cfg.AccessLogSuccessSampleRate,
	})
	// Global proxy data-plane tracing config, likewise set once up front.
	proxy.SetTracingConfig(proxy.TracingConfig{
		Enabled:    cfg.ProxyTracingEnabled,
		SampleRate: cfg.ProxyTraceSampleRate,
	})

	return snapshotCache, nil
}

// wireMeshDNS wires the snapshot cache to PERSIST the mesh service->IP record table
// to the host-persistent snapshot file when --mesh-dns is enabled. It is a no-op
// when disabled.
//
// The agent no longer serves DNS itself (issue #578): the resolver was decoupled
// into the standalone aether-mesh-dns DaemonSet, which watches this snapshot file
// (fsnotify), serves HOST_IP:18054, and — being surge-capable with SO_REUSEPORT —
// hands off hitlessly across its own rolls (which an agent roll could never do,
// since deleting the agent pod tore down the resolver with it). The agent's only
// remaining role is to keep writing the snapshot from its capture reconciler.
func wireMeshDNS(snapshotCache *cache.SnapshotCache) {
	if !cfg.MeshDNS {
		return
	}
	snapshotCache.SetMeshDNSSnapshotPath(cfg.MeshDNSSnapshotPath)
}

// wireSpireBridge optionally creates and registers the SPIRE bridge for SDS when
// SPIRE is enabled. Returns the bridge (nil when SPIRE is disabled).
func wireSpireBridge(ctx context.Context, m ctrl.Manager, snapshotCache *cache.SnapshotCache, spireSource *commonspire.Source) (*spire.Bridge, error) {
	if !cfg.SpireEnabled {
		l.InfoContext(ctx, "SPIRE integration disabled")
		return nil, nil
	}
	// Optionally create and start the SPIRE bridge for SDS
	spireBridge := spire.NewBridge(cfg.SpireAdminSocketPath, snapshotCache, spireSource, l)
	if err := m.Add(spireBridge); err != nil {
		return nil, fmt.Errorf("failed to add SPIRE bridge: %w", err)
	}
	return spireBridge, nil
}

// wireGAMMA registers the GAMMA east-west L7 routing reconciler when --gamma is set
// (proposal 018, Phase 2). It installs the required Gateway API and config.aether.io
// schemes and sets up the reconciler with the manager.
func wireGAMMA(m ctrl.Manager, snapshotCache *cache.SnapshotCache) error {
	if !cfg.Gamma {
		return nil
	}
	// GAMMA east-west L7 routing (proposal 018, Phase 2): watch HTTPRoutes parented
	// to a Service and enrich the outbound routes. Default off — registers nothing
	// (and adds no gateway-api watch) unless --gamma is set.
	if err := gatewayv1.Install(m.GetScheme()); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
	}
	// ReferenceGrant (v1beta1) gates cross-namespace backendRefs on GAMMA routes.
	if err := gatewayv1beta1.Install(m.GetScheme()); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io/v1beta1 scheme: %w", err)
	}
	// HTTPFilter (config.aether.io, proposal 025): the gamma reconciler watches it
	// for ExtensionRef/targetRef escape-hatch filters. MUST be in the scheme —
	// without it the watch fails ("no kind is registered") and the manager
	// crash-loops on cache sync, killing the CNI socket with it. The RESTMapper
	// gate (#453) does not cover this: it checks the CRD on the API server, not
	// the client scheme.
	if err := configapisv1.AddToScheme(m.GetScheme()); err != nil {
		return fmt.Errorf("register config.aether.io scheme: %w", err)
	}
	gammaReconciler := &gamma.Reconciler{
		Client:     m.GetClient(),
		Sink:       snapshotCache,
		MeshDomain: cfg.MeshDomain,
		Log:        l,
	}
	if err := gammaReconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up GAMMA reconciler: %w", err)
	}
	return nil
}

// wireAuthzAndConfigImport applies the authz-sidecar config to the snapshot cache
// and registers the cross-cluster config importer when --import-config is set.
// Cross-cluster config import (proposal 026): poll the registrar for config
// projections peer clusters exported and materialize the imported GAMMA routes
// into the cache (merged with local routes; local wins). Default off
// (--import-config). No-op when the registry backend has no cross-cluster config
// plane (kubernetes/dynamodb don't implement registry.ConfigImporter).
func wireAuthzAndConfigImport(ctx context.Context, m ctrl.Manager, reg registry.Registry, snapshotCache *cache.SnapshotCache) {
	if cfg.AuthzSidecar {
		snapshotCache.SetAuthzSidecar(cfg.AuthzSidecarTimeout, cfg.AuthzSidecarFailureModeAllow)
	}
	if !cfg.ImportConfig {
		return
	}
	imp, ok := reg.(registry.ConfigImporter)
	if !ok {
		l.Warn("config import enabled but the registry backend has no cross-cluster config plane; skipping")
		return
	}
	importer := configimport.NewImporter(imp, snapshotCache, cfg.ClusterName, cfg.ControlCluster, 0, l)
	if err := m.Add(importer); err != nil {
		l.ErrorContext(ctx, "failed to add config importer", "error", err)
	}
}

// wireL4Routes installs Gateway API schemes and registers the L4 route reconciler
// (proposal 018, Phase 3b). Unconditional since proposal 031: the reconciler
// CRD-detects each type and degrades when absent.
func wireL4Routes(m ctrl.Manager, snapshotCache *cache.SnapshotCache) error {
	// L4 route types (proposal 018, Phase 3b): watch TCPRoutes, TLSRoutes, and
	// UDPRoutes parented to a Service and project weighted TCP floor chains /
	// per-SNI TLS chains onto the capture listener. Unconditional since proposal
	// 031: the reconciler CRD-detects each type and degrades when absent.
	// NOTE: UDPRoute is control-plane only until the CNI UDP redirect lands.
	// v1 (all route types since gateway-api 1.6) + v1beta1 (ReferenceGrant).
	if err := gatewayv1.Install(m.GetScheme()); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
	}
	if err := gatewayv1beta1.Install(m.GetScheme()); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io/v1beta1 scheme: %w", err)
	}
	l4Reconciler := &l4route.Reconciler{
		Client:     m.GetClient(),
		Sink:       snapshotCache,
		MeshDomain: cfg.MeshDomain,
		Log:        l,
	}
	if err := l4Reconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up L4 route reconciler: %w", err)
	}
	return nil
}

// wireCaptureReconciler sets up the transparent-capture reconciler. It watches the
// generated mesh Services: capture wants their cluster.local authorities (cap_http),
// mesh DNS wants their ClusterIPs (the DnsTable). Unconditional (proposal 031).
func wireCaptureReconciler(m ctrl.Manager, snapshotCache *cache.SnapshotCache) error {
	// Transparent capture (Phase 3a) and mesh DNS (mesh-global FQDN) both read the
	// generated mesh Services: capture wants their cluster.local authorities (cap_http),
	// mesh DNS wants their ClusterIPs (the DnsTable). One reconciler feeds both;
	// capture is unconditional (proposal 031), so it always runs.
	captureReconciler := &capture.Reconciler{
		Client: m.GetClient(),
		Sink:   snapshotCache,
		Log:    l,
	}
	if err := captureReconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up transparent-capture reconciler: %w", err)
	}
	return nil
}

// setXDSServer creates and registers an Agent xDS server as a runnable with the Manager.
// The server listens on a Unix domain socket and serves Envoy discovery service requests
// (LDS, CDS, EDS, RDS, ADS) with resource snapshots generated from local pod storage and the registry.
func setXDSServer(ctx context.Context, m ctrl.Manager, registry registry.Registry, localStorage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache, ackTracker *ack.Tracker, trustDomain string) error {
	// Create xDS server
	xdsSrv, err := xdsServer.NewAgentXdsServer(ctx, cfg.ClusterName, cfg.NodeName, trustDomain, registry, localStorage, snapshotCache, ackTracker.Callbacks(), l)
	if err != nil {
		return err
	}
	if err = m.Add(xdsSrv); err != nil {
		return fmt.Errorf("failed to add xDS server: %w", err)
	}

	return nil
}

// setupCNIServer creates and registers a CNI gRPC server as a runnable with the Manager.
// The server listens on a Unix domain socket and handles pod registration/deregistration
// requests from the CNI plugin binary. It stores pod data locally and triggers xDS snapshot updates.
func setupCNIServer(m ctrl.Manager, localStorage storage.Storage[*cniv1.CNIPod], registry registry.Registry, snapshotCache *cache.SnapshotCache, ackTracker *ack.Tracker, spireBridge *spire.Bridge, trustDomain string) error {
	// Create a registry and CNI server
	cniSrv, err := cniServer.NewCNIServer(
		cfg.ClusterName,
		cfg.NodeName,
		trustDomain,
		localStorage,
		registry,
		snapshotCache,
		ackTracker,
		spireBridge,
		l,
		m.GetClient(),
		m.GetCache(),
		cfg.CNIServerConfig,
	)
	if err != nil {
		return err
	}
	if err = m.Add(cniSrv); err != nil {
		return fmt.Errorf("failed to add CNI server: %w", err)
	}

	return nil
}

// setupStorage initializes the local file-based storage for pod data.
// It creates a cached local storage instance that watches the filesystem for changes
// and maintains an in-memory cache synchronized with disk.
func setupStorage(ctx context.Context, path string) (storage.Storage[*cniv1.CNIPod], error) {
	s := storage.NewCachedLocalStorage[*cniv1.CNIPod](
		path,
		func() *cniv1.CNIPod { return &cniv1.CNIPod{} },
	)

	if err := s.Initialize(ctx); err != nil {
		return nil, err
	}

	return s, nil
}

// setupRegistrarClient creates and initializes a registrar-backed registry.
// The agent connects to the in-cluster Registrar service for all endpoint
// registration and discovery operations. When SPIRE is enabled, the connection
// uses mTLS with an X.509 SVID fetched over the SPIRE Workload API socket;
// otherwise, insecure transport is used.
func setupRegistrarClient(ctx context.Context, src *commonspire.Source) (registry.Registry, error) {
	regCfg := registrarclient.Config{
		Address:     cfg.RegistrarAddress,
		ClusterName: cfg.ClusterName,
		NodeName:    cfg.NodeName,
	}

	if cfg.SpireEnabled {
		// Peer authorization is scoped to the mesh domain: the registrar must
		// present an SVID in the mesh's own trust domain.
		tlsCfg, err := commonspire.ClientTLSConfig(src, cfg.MeshDomain)
		if err != nil {
			return nil, err
		}
		regCfg.DialOptions = []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
		l.InfoContext(ctx, "registrar client using SPIRE mTLS", "socket", cfg.SpireWorkloadSocketPath, "trustDomain", cfg.MeshDomain)
	} else {
		l.InfoContext(ctx, "registrar client using insecure transport")
	}

	reg := registrarclient.New(l, regCfg)

	if err := reg.Initialize(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to initialize registry: %w", err), reg.Close())
	}

	return reg, nil
}

// deferLogShutdown flushes and stops the OTLP log exporter. No-op when
// logShutdown is nil (OTLP logging is disabled).
func deferLogShutdown(ctx context.Context) {
	if logShutdown == nil {
		return
	}
	if err := logShutdown(ctx); err != nil {
		l.ErrorContext(ctx, "failed to flush OTel logs", "error", err)
	}
}

// deferTelemetryShutdown runs the telemetry shutdown returned by manager.Bootstrap.
// No-op when shutdown is nil.
func deferTelemetryShutdown(ctx context.Context, shutdown func(context.Context) error) {
	if shutdown == nil {
		return
	}
	if err := shutdown(ctx); err != nil {
		l.ErrorContext(ctx, "failed to shutdown telemetry", "error", err)
	}
}

// must panics if err is non-nil. Use only for programming errors that should never occur at runtime.
