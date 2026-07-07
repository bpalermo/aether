package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/edge/gatewayapi"
	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/manager"
	commonspire "github.com/bpalermo/aether/common/spire"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// edgeName is the controller/logging name for the edge proxy control plane.
const edgeName = "aether-edge"

var edgeCmd = &cobra.Command{
	Use:   "edge",
	Short: "Runs the aether edge (north-south ingress) proxy control plane.",
	Long: "Runs the Aether agent in edge mode: a single-identity ingress gateway sidecar that serves xDS to a " +
		"public-facing Envoy, routing external traffic directly to mesh pods over mTLS. It runs no CNI server, " +
		"local storage, or per-pod listeners, and the Envoy fetches its SVID straight from SPIRE (no agent bridge).",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runEdge(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(edgeCmd)

	// The edge needs the manager flags (logging, metrics, health probe, OTEL)
	// and the shared identity/registrar/SPIRE flags, bound onto the same cfg.
	manager.RegisterFlags(edgeCmd, &cfg.Config)
	registerSharedFlags(edgeCmd, false)

	// The edge has no CNI/pod storage; it points the (always-empty) local store
	// at a pod-local emptyDir so PreListen's load is a no-op.
	edgeCmd.Flags().StringVar(&cfg.MountedLocalStorageDir, "mounted-registry-dir", "/var/lib/aether/registry", "Pod-local directory for the edge's (empty) local store")
	edgeCmd.Flags().Uint32Var(&cfg.EdgeHTTPPort, "edge-http-port", cfg.EdgeHTTPPort, "Port the edge proxy's public-facing HTTP listener binds")
	edgeCmd.Flags().StringVar(&cfg.RouteNamespace, "route-namespace", "", "Namespace to watch Gateways/HTTPRoutes in (empty = the edge pod's own namespace)")
	edgeCmd.Flags().BoolVar(&cfg.EdgeTLS, "edge-tls", false, "Terminate downstream TLS: serve an HTTPS listener (certs per Gateway listener via SDS) + an HTTP->HTTPS redirect")
	edgeCmd.Flags().Uint32Var(&cfg.EdgeHTTPSPort, "edge-https-port", cfg.EdgeHTTPSPort, "Port the edge TLS listener binds when --edge-tls is set")
	edgeCmd.Flags().Uint32Var(&cfg.EdgeReadinessPort, "edge-readiness-port", cfg.EdgeReadinessPort, "Port the dedicated always-bound readiness listener binds; the readiness probe targets it (independent of the public listeners)")
	edgeCmd.Flags().StringVar(&cfg.GatewayClassName, "gateway-class", cfg.GatewayClassName, "GatewayClass name whose Gateways this edge serves")
	edgeCmd.Flags().StringVar(&cfg.GeoipCityDB, "geoip-city-db", "", "Path to a MaxMind city-type mmdb (proposal 028). When set, the edge emits x-geo-* headers on routed requests; the reserved x-geo-* namespace is stripped from client requests regardless")
	edgeCmd.Flags().StringSliceVar(&cfg.GeoipHeaders, "geoip-headers", []string{"country"}, "Which geo headers to emit: country, city")
	edgeCmd.Flags().Uint32Var(&cfg.XffNumTrustedHops, "xff-num-trusted-hops", 0, "Trusted proxies in front of the edge (topology fact): feeds BOTH the HCM client-address resolution and the geoip filter's XFF config")
	edgeCmd.Flags().StringVar(&cfg.EdgeServiceName, "edge-service-name", "", "Name of the edge's own LoadBalancer Service (in the edge namespace); its assigned LB address is published as every class-aether Gateway's status.addresses. Empty disables address publication")
	edgeCmd.Flags().BoolVar(&cfg.EdgePerGatewayAddressing, "edge-per-gateway-addressing", true, "Enable proposal 021 Phase 2: create a per-Gateway LoadBalancer Service + internal-port demux so each Gateway gets its own external IP (default: true). Set false to fall back to Phase 1 (shared IP)")
}

// runEdge initializes and runs the Aether edge proxy control plane. It is a
// subtractive variant of runAgent: the controller-runtime manager hosts only
// the xDS server and the registry refresher (no CNI server, no local pod
// storage, no SPIRE bridge). The snapshot cache is put in edge mode — one
// public-facing listener, single-identity service clusters whose SDS points at
// SPIRE directly, no ODCDS catch-all — and its dependency set is seeded with
// the statically exposed services so the registrar watch is scoped to exactly
// those.
func runEdge(ctx context.Context) (retErr error) {
	// The edge's identity is its pod name: derive proxy-id / node-name from the
	// POD_NAME downward-API env (set by the chart) unless explicitly overridden,
	// so the deployment needn't wire them. node-name == proxy-id (the edge has no
	// distinct K8s node identity).
	if name := currentPodName(); name != "" {
		if cfg.ProxyServiceNodeID == "" || cfg.ProxyServiceNodeID == constants.DefaultProxyID {
			cfg.ProxyServiceNodeID = name
		}
		if cfg.NodeName == "" {
			cfg.NodeName = cfg.ProxyServiceNodeID
		}
	}
	if cfg.ProxyServiceNodeID == "" || cfg.NodeName == "" {
		return fmt.Errorf("edge identity unresolved: set POD_NAME (downward API) or pass --proxy-id/--node-name")
	}

	cfg.MountedLocalStorageDir = edgeStorageDir(cfg.MountedLocalStorageDir)

	l.InfoContext(ctx, "starting aether edge proxy control plane",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
		"edgeHTTPPort", cfg.EdgeHTTPPort,
	)

	if logShutdown != nil {
		defer func() {
			if shutdownErr := logShutdown(ctx); shutdownErr != nil {
				l.ErrorContext(ctx, "failed to flush OTel logs", "error", shutdownErr)
			}
		}()
	}

	// Watch Gateway API objects CLUSTER-WIDE. The edge reconciles every Gateway of
	// our GatewayClass wherever it lives (namespace-agnostic): the conformance suite
	// creates its Gateways/Routes in its own namespaces, so a namespace-scoped cache
	// would never see them and they would never reach Accepted/Programmed. Leaving
	// CacheOptions nil makes the manager cache every watched kind across all
	// namespaces; the ClusterRoles grant the matching cluster-wide list/watch.
	//
	// routeNamespace is still resolved (the edge's own namespace) and used as the
	// default namespace for the Secret provider's TLS cert lookups — Gateway TLS
	// secrets are expected alongside the edge by default.
	routeNamespace := cfg.RouteNamespace
	if routeNamespace == "" {
		routeNamespace = currentNamespace()
	}
	cfg.CacheOptions = nil

	// The edge runs NO leader election. It is a 2-replica Deployment where each
	// replica hosts its own xDS server + SnapshotCache feeding its co-located Envoy,
	// and the Gateway API reconciler injects per-Gateway listeners into that LOCAL
	// cache via SetEdgeGateways. The reconciler — and the xDS server and registry
	// refresher — must therefore run on EVERY replica: a leader-only reconciler
	// left the follower's Envoy with no listeners on the allocated internal ports,
	// causing "connection refused" for the ~half of connections kube-proxy/MetalLB
	// routed to it. K8s writes (Service CreateOrUpdate, Gateway status patches) are
	// idempotent / optimistic-concurrency-safe under concurrent reconciliation
	// across replicas (status writes retry on conflict). The NeedLeaderElection()
	// ==false opt-outs on the runnables are defensive — they preserve this
	// invariant if leader election is ever enabled on the manager.

	// Manager scheme = client-go built-ins + the Gateway API types so the
	// reconciler reads typed Gateways/HTTPRoutes (no unstructured).
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register client-go scheme: %w", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
	}
	// TCPRoute/TLSRoute live in v1alpha2 (not yet promoted to v1 in gateway-api v1.5.1).
	if err := gatewayv1alpha2.Install(scheme); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io/v1alpha2 scheme: %w", err)
	}
	// ReferenceGrant is served as v1beta1 (the storage version) — needed to admit
	// cross-namespace backendRefs.
	if err := gatewayv1beta1.Install(scheme); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io/v1beta1 scheme: %w", err)
	}
	// EdgeConfig / MeshConfig / HTTPFilter (config.aether.io) — the edge resolves
	// per-Gateway EdgeConfig via the parametersRef chain (proposal 029).
	if err := configapisv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register config.aether.io scheme: %w", err)
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, edgeName, Version, func(o *ctrl.Options) {
		o.Scheme = scheme
	})
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

	// Open the SPIRE Workload API source for registrar mTLS and to resolve the
	// edge's own identity (trust domain + SVID name). The Envoy fetches its SVID
	// from SPIRE directly over the spire_agent SDS cluster, so the agent runs no
	// SPIRE bridge — it only needs the names to program into the clusters.
	var spireSource *commonspire.Source
	identityTrustDomain := cfg.MeshDomain
	edgeSpiffeID := ""
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
		svid, err := spireSource.GetX509SVID()
		if err != nil {
			return fmt.Errorf("failed to read edge SVID: %w", err)
		}
		edgeSpiffeID = svid.ID.String()
		l.InfoContext(ctx, "resolved edge identity from SPIRE", "trustDomain", identityTrustDomain, "spiffeID", edgeSpiffeID)
	}

	reg, err := setupRegistrarClient(ctx, spireSource)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reg.Close()) }()

	snapshotCache := cache.NewSnapshotCache(cfg.ProxyServiceNodeID, l)
	snapshotCache.SetMeshDomain(cfg.MeshDomain)
	snapshotCache.SetEmitStatsPod(cfg.EmitStatsPod)
	snapshotCache.SetEdgeGeoip(proxy.GeoipConfig{
		CityDBPath:        cfg.GeoipCityDB,
		Headers:           cfg.GeoipHeaders,
		XffNumTrustedHops: cfg.XffNumTrustedHops,
	}, cfg.XffNumTrustedHops)
	snapshotCache.SetEdgeMode(cfg.EdgeHTTPPort)
	snapshotCache.SetEdgeReadinessPort(cfg.EdgeReadinessPort)
	if cfg.EdgeTLS {
		snapshotCache.SetEdgeTLSMode(cfg.EdgeHTTPSPort)
	}
	snapshotCache.SetEdgeIdentity(edgeSpiffeID, identityTrustDomain)
	// Routes come exclusively from Gateway API HTTPRoutes via the reconciler below;
	// the initial snapshot (PreListen) serves a 404-only edge route table until the
	// first reconcile. The edge exposes ONLY explicitly-routed services.

	proxy.SetAccessLogConfig(proxy.AccessLogConfig{
		Enabled:           cfg.AccessLogsEnabled,
		SuccessSampleRate: cfg.AccessLogSuccessSampleRate,
	})
	proxy.SetTracingConfig(proxy.TracingConfig{
		Enabled:    cfg.ProxyTracingEnabled,
		SampleRate: cfg.ProxyTraceSampleRate,
	})

	ackTracker := ack.NewTracker(l)

	// The edge runs no local workloads, so it loads listeners from an empty
	// storage: PreListen's LoadListenersFromStorage finds zero pods and the
	// edge-mode Listeners() returns the single public-facing listener instead.
	emptyStorage, err := setupStorage(ctx, cfg.MountedLocalStorageDir)
	if err != nil {
		return err
	}

	xdsSrv, err := xdsServer.NewAgentXdsServer(ctx, cfg.ClusterName, cfg.ProxyServiceNodeID, identityTrustDomain, reg, emptyStorage, snapshotCache, ackTracker.Callbacks(), l)
	if err != nil {
		return err
	}
	if err = m.Add(xdsSrv); err != nil {
		return fmt.Errorf("failed to add xDS server: %w", err)
	}

	refresher := xdsServer.NewRegistryRefresher(cfg.ClusterName, cfg.ProxyServiceNodeID, snapshotCache, reg, l)
	if err = m.Add(refresher); err != nil {
		return fmt.Errorf("failed to add registry refresher: %w", err)
	}

	// Watch Gateways/HTTPRoutes and project them into the cache as the edge's
	// virtual hosts + scoped dependency set. With TLS enabled, also resolve each
	// Gateway listener's cert via the SecretProvider registry (kubernetes provider)
	// and watch their Secrets.
	var secretRegistry *secret.Registry
	if cfg.EdgeTLS {
		secretRegistry = secret.NewRegistry(secret.NewKubernetesProvider(m.GetClient(), routeNamespace))
	}
	gwReconciler := &gatewayapi.Reconciler{
		Client:               m.GetClient(),
		APIReader:            m.GetAPIReader(),
		Sink:                 snapshotCache,
		Namespace:            routeNamespace,
		EdgeServiceName:      cfg.EdgeServiceName,
		GatewayClassName:     cfg.GatewayClassName,
		MeshDomain:           cfg.MeshDomain,
		Secrets:              secretRegistry,
		PerGatewayAddressing: cfg.EdgePerGatewayAddressing,
		Log:                  l,
	}
	if err = gwReconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up Gateway API reconciler: %w", err)
	}

	l.DebugContext(ctx, "waiting for local storage to be ready")
	if err = emptyStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.DebugContext(ctx, "starting edge manager")
	return m.Start(ctx)
}

// edgeStorageDir resolves the edge's (always-empty) pod-local registry dir. The
// shared --mounted-registry-dir flag is registered by BOTH the root and edge
// commands on one cfg field; the root's hostPath default wins the init ordering,
// so an un-overridden edge would inherit the node hostPath that doesn't exist in
// its pod. When the value is that host default, fall back to the pod-local dir;
// an explicit override is preserved.
func edgeStorageDir(configured string) string {
	if configured == constants.DefaultHostCNIRegistryDir {
		return constants.DefaultEdgeRegistryDir
	}
	return configured
}

// currentPodName returns the edge pod's name from the POD_NAME downward-API env
// (set by the chart); empty if unset. Used as the edge's xDS/watch identity.
func currentPodName() string {
	return os.Getenv("POD_NAME")
}

// currentNamespace returns the namespace the edge pod runs in (the default
// namespace to watch Gateways/HTTPRoutes in). It reads POD_NAMESPACE (set via the
// downward API by the chart) and falls back to the service-account namespace
// file.
func currentNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(data)
	}
	return "default"
}
