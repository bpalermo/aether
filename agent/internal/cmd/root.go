// Package cmd provides command-line interface and configuration for the Aether agent.
//
// The agent is a Kubernetes DaemonSet component that runs on each node to manage
// an Envoy xDS control plane and a CNI plugin for transparent traffic interception.
// It connects to the in-cluster Registrar service for endpoint registration and discovery,
// and generates Envoy configuration (listeners, clusters, endpoints, routes) for local pods.
//
// Command-line flags control the agent's initialization:
//   - debug: Enable debug-level logging
//   - node-name: Kubernetes node name where the agent runs (required)
//   - cluster-name: Kubernetes cluster name (required)
//   - proxy-id: xDS node identifier for the Envoy proxy instance (required)
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
	"os"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/capture"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/internal/gamma"
	"github.com/bpalermo/aether/agent/internal/l4route"
	"github.com/bpalermo/aether/agent/internal/meshdns"
	"github.com/bpalermo/aether/agent/internal/node"
	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/bpalermo/aether/common/config"
	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/must"
	commonspire "github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/registry"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
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

	// Cold-start gate (node proxy only): remove the aether.io/agent-not-ready
	// startup taint from this node once the CNI server is serving.
	rootCmd.Flags().BoolVar(&cfg.RemoveStartupTaint, "remove-startup-taint", cfg.RemoveStartupTaint, "Remove the aether.io/agent-not-ready node taint once the CNI server is serving (needs nodes patch RBAC)")
	rootCmd.Flags().BoolVar(&cfg.Gamma, "gamma", false, "Enable GAMMA east-west L7 routing: watch HTTPRoutes parented to a Service and enrich the node proxy's outbound routes (proposal 018, Phase 2)")
	rootCmd.Flags().BoolVar(&cfg.L4Routes, "l4-routes", false, "Enable L4 route types (TCPRoute/TLSRoute/UDPRoute) parented to a Service: weighted TCP floor chains and SNI-routed TLS chains on the capture listener (proposal 018, Phase 3b). NOTE: UDPRoute is control-plane only until the CNI UDP redirect lands.")
	rootCmd.Flags().BoolVar(&cfg.TransparentCapture, "transparent-capture", false, "Enable transparent capture: per-pod capture listeners + cap_http route table from the generated mesh Services (proposal 018, Phase 3a)")
	rootCmd.Flags().BoolVar(&cfg.MeshDNS, "mesh-dns", false, "Enable per-pod mesh DNS: answer <svc>.<mesh-domain> from the generated mesh Services and forward the rest to --mesh-dns-upstream (proposal 018, mesh-global FQDN)")
	rootCmd.Flags().StringSliceVar(&cfg.MeshDNSUpstream, "mesh-dns-upstream", cfg.MeshDNSUpstream, "Upstream resolver(s) (host[:port]) the mesh-DNS filter forwards non-mesh queries to (the cluster kube-dns)")
}

// registerSharedFlags registers the flags common to the node agent (root) and
// the edge subcommand: cluster/proxy identity, the registrar address, the mesh
// system config inherited from the umbrella chart, and the SPIRE workload
// socket. Each command binds them onto the shared cfg; cobra runs the root's
// PersistentPreRunE (mesh-config load + logging) for the subcommand too.
// registerSharedFlags binds the identity/registrar/SPIRE/system flags shared by
// the node agent and the edge. requirePodIdentity marks node-name and proxy-id
// required (the node agent — node-name is the K8s node, not derivable); the edge
// passes false and derives both from the POD_NAME downward-API env instead.
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
	cmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "Unique identifier for this Envoy proxy instance in the xDS server (required)")

	// Registrar configuration
	cmd.Flags().StringVar(&cfg.RegistrarAddress, "registrar-address", cfg.RegistrarAddress, "gRPC address of the in-cluster Registrar service")

	// SPIRE workload socket (per-instance; the SPIRE on/off policy is system-wide).
	cmd.Flags().StringVar(&cfg.SpireWorkloadSocketPath, "spire-workload-socket", constants.DefaultSpireWorkloadSocketPath, "Path to the SPIRE Workload API UDS socket used for registrar mTLS")

	// These calls only fail if the flag name is not registered, which would be a programming error.
	must.NoError(cmd.MarkFlagRequired("cluster-name"))
	if requirePodIdentity {
		must.NoError(cmd.MarkFlagRequired("node-name"))
		must.NoError(cmd.MarkFlagRequired("proxy-id"))
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
	l.InfoContext(ctx, "starting aether agent",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
	)

	// Flush and stop the OTLP log exporter last (registered first → runs last),
	// so records emitted during the rest of shutdown are still exported. No-op
	// when OTLP logging is disabled.
	if logShutdown != nil {
		defer func() {
			if shutdownErr := logShutdown(ctx); shutdownErr != nil {
				l.ErrorContext(ctx, "failed to flush OTel logs", "error", shutdownErr)
			}
		}()
	}

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
	if result.Shutdown != nil {
		defer func() {
			if shutdownErr := result.Shutdown(ctx); shutdownErr != nil {
				l.ErrorContext(ctx, "failed to shutdown telemetry", "error", shutdownErr)
			}
		}()
	}

	m := result.Manager

	localStorage, err := setupStorage(ctx, cfg.MountedLocalStorageDir)
	if err != nil {
		return err
	}

	// When SPIRE is enabled, open the Workload API source and resolve the real
	// trust domain SPIRE issues into (e.g. "aether.internal"). That trust domain
	// names the SDS resources / SPIFFE IDs the agent programs into Envoy so they
	// match the secrets the SPIRE bridge delivers. Peer authorization uses the
	// mesh domain — addressing (<svc>.<mesh-domain>) and identity
	// (spiffe://<mesh-domain>/...) are one domain by design, never split.
	var spireSource *commonspire.Source
	identityTrustDomain := cfg.MeshDomain
	if cfg.SpireEnabled {
		spireSource, err = commonspire.NewSource(ctx, cfg.SpireWorkloadSocketPath)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, spireSource.Close()) }()

		identityTrustDomain, err = commonspire.TrustDomainFromSource(spireSource)
		if err != nil {
			return fmt.Errorf("failed to resolve SPIRE trust domain: %w", err)
		}
		l.InfoContext(ctx, "resolved workload trust domain from SPIRE", "trustDomain", identityTrustDomain)
	}

	reg, err := setupRegistrarClient(ctx, spireSource)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reg.Close()) }()

	snapshotCache := cache.NewSnapshotCache(cfg.NodeName, l)
	snapshotCache.SetMeshDomain(cfg.MeshDomain)
	snapshotCache.SetEmitStatsPod(cfg.EmitStatsPod)
	snapshotCache.SetCaptureEnabled(cfg.TransparentCapture)
	if cfg.MeshDNS {
		// In-process DNS resolver (Istio-style): a host-local miekg/dns server that
		// answers <svc>.<meshDomain> and forwards the rest to kube-dns. The CNI DNATs
		// each pod's :53 straight to it at HOST_IP:resolverPort (no Envoy DNS layer,
		// no setns, no privilege increase).
		hostIP := os.Getenv("HOST_IP")
		resolverPort := uint32(commonconstants.ProxyDNSResolverPort)
		dnsServer := meshdns.NewServer(cfg.MeshDomain, fmt.Sprintf("%s:%d", hostIP, resolverPort), l)
		// Default the forward upstream to the agent's own resolv.conf (kube-dns, via
		// ClusterFirstWithHostNet) when --mesh-dns-upstream is unset, so mesh DNS is
		// safe to enable by default without per-cluster configuration.
		upstreams := cfg.MeshDNSUpstream
		if len(upstreams) == 0 {
			upstreams = meshdns.NameserversFromResolvConf("/etc/resolv.conf")
			l.Info("mesh-DNS upstream defaulted from /etc/resolv.conf", "upstreams", upstreams)
		}
		dnsServer.SetUpstreams(upstreams)
		if err = m.Add(dnsServer); err != nil {
			return fmt.Errorf("failed to add mesh-DNS resolver: %w", err)
		}
		snapshotCache.SetMeshDNSServer(dnsServer)
	}
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

	// Tracks Envoy's delta-xDS ACK/NACKs so the CNI server can confirm (and
	// diagnose) config delivery without polling the Envoy admin interface.
	ackTracker := ack.NewTracker(l)

	// Optionally create and start the SPIRE bridge for SDS
	var spireBridge *spire.Bridge
	if cfg.SpireEnabled {
		spireBridge = spire.NewBridge(cfg.SpireAdminSocketPath, snapshotCache, spireSource, l)
		if err = m.Add(spireBridge); err != nil {
			return fmt.Errorf("failed to add SPIRE bridge: %w", err)
		}
	} else {
		l.InfoContext(ctx, "SPIRE integration disabled")
	}

	if err = setXDSServer(ctx, m, reg, localStorage, snapshotCache, ackTracker, identityTrustDomain); err != nil {
		return err
	}

	if err = setupCNIServer(m, localStorage, reg, snapshotCache, ackTracker, spireBridge, identityTrustDomain); err != nil {
		return err
	}

	// Remove the aether startup taint from this node once the CNI server is
	// serving, so workload pods (which don't tolerate it) can schedule here. The
	// operator registers the taint via the kubelet; this closes the cold-start
	// window where a pod's CNI ADD would arrive before the agent is ready.
	if cfg.RemoveStartupTaint {
		if err = m.Add(&node.TaintRemover{
			Client:     m.GetClient(),
			NodeName:   cfg.NodeName,
			SocketPath: cfg.CNIServerConfig.SocketPath,
			Log:        l,
		}); err != nil {
			return fmt.Errorf("failed to add startup-taint remover: %w", err)
		}
	}

	// Rebuild the cluster/endpoint/route snapshot when the registry reports
	// endpoint changes, so services registered after startup become routable
	// without restarting the agent. No-op if the registry can't notify.
	refresher := xdsServer.NewRegistryRefresher(cfg.ClusterName, cfg.NodeName, snapshotCache, reg, l)
	if err = m.Add(refresher); err != nil {
		return fmt.Errorf("failed to add registry refresher: %w", err)
	}

	// GAMMA east-west L7 routing (proposal 018, Phase 2): watch HTTPRoutes parented
	// to a Service and enrich the outbound routes. Default off — registers nothing
	// (and adds no gateway-api watch) unless --gamma is set.
	if cfg.Gamma {
		if err = gatewayv1.Install(m.GetScheme()); err != nil {
			return fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
		}
		gammaReconciler := &gamma.Reconciler{
			Client:     m.GetClient(),
			Sink:       snapshotCache,
			MeshDomain: cfg.MeshDomain,
			Log:        l,
		}
		if err = gammaReconciler.SetupWithManager(m); err != nil {
			return fmt.Errorf("failed to set up GAMMA reconciler: %w", err)
		}
	}

	// L4 route types (proposal 018, Phase 3b): watch TCPRoutes, TLSRoutes, and
	// UDPRoutes parented to a Service and project weighted TCP floor chains /
	// per-SNI TLS chains onto the capture listener. Default off (--l4-routes).
	// Requires --transparent-capture to be meaningful (routes need capture chains).
	// NOTE: UDPRoute is control-plane only until the CNI UDP redirect lands.
	if cfg.L4Routes {
		if err = gatewayv1alpha2.Install(m.GetScheme()); err != nil {
			return fmt.Errorf("register gateway.networking.k8s.io v1alpha2 scheme: %w", err)
		}
		l4Reconciler := &l4route.Reconciler{
			Client:     m.GetClient(),
			Sink:       snapshotCache,
			MeshDomain: cfg.MeshDomain,
			Log:        l,
		}
		if err = l4Reconciler.SetupWithManager(m); err != nil {
			return fmt.Errorf("failed to set up L4 route reconciler: %w", err)
		}
	}

	// Transparent capture (Phase 3a) and/or mesh DNS (mesh-global FQDN) both read the
	// generated mesh Services: capture wants their cluster.local authorities (cap_http),
	// mesh DNS wants their ClusterIPs (the DnsTable). One reconciler feeds both; run it
	// if either feature is on. Default off — no Service watch otherwise.
	if cfg.TransparentCapture || cfg.MeshDNS {
		captureReconciler := &capture.Reconciler{
			Client: m.GetClient(),
			Sink:   snapshotCache,
			Log:    l,
		}
		if err = captureReconciler.SetupWithManager(m); err != nil {
			return fmt.Errorf("failed to set up transparent-capture reconciler: %w", err)
		}
	}

	l.DebugContext(ctx, "waiting for local storage to be ready")
	if err = localStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.DebugContext(ctx, "local storage is ready, starting manager")
	return m.Start(ctx)
}

// setXDSServer creates and registers an Agent xDS server as a runnable with the Manager.
// The server listens on a Unix domain socket and serves Envoy discovery service requests
// (LDS, CDS, EDS, RDS, ADS) with resource snapshots generated from local pod storage and the registry.
func setXDSServer(ctx context.Context, m ctrl.Manager, registry registry.Registry, localStorage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache, ackTracker *ack.Tracker, trustDomain string) error {
	// Create xDS server
	xdsSrv, err := xdsServer.NewAgentXdsServer(ctx, cfg.ClusterName, cfg.ProxyServiceNodeID, trustDomain, registry, localStorage, snapshotCache, ackTracker.Callbacks(), l)
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
		cfg.ProxyServiceNodeID,
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
	regCfg := registry.RegistrarConfig{
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

	reg := registry.NewRegistrarRegistry(l, regCfg)

	if err := reg.Initialize(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to initialize registry: %w", err), reg.Close())
	}

	return reg, nil
}

// must panics if err is non-nil. Use only for programming errors that should never occur at runtime.
