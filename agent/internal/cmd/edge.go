package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"aethermesh.dev/agent/constants"
	"aethermesh.dev/agent/internal/edge/gatewayapi"
	"aethermesh.dev/agent/internal/edge/secret"
	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	xdsServer "aethermesh.dev/agent/internal/xds/server"
	configapisv1 "aethermesh.dev/common/apis/config/v1"
	"aethermesh.dev/common/manager"
	commonspire "aethermesh.dev/common/spire"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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

	edgeCmd.Flags().Uint32Var(&cfg.EdgeHTTPPort, "edge-http-port", cfg.EdgeHTTPPort, "Port the edge proxy's public-facing HTTP listener binds")
	edgeCmd.Flags().StringVar(&cfg.RouteNamespace, "route-namespace", "", "Default namespace for Gateway TLS certificate Secrets (empty = the edge pod's own namespace). Gateway/route watching is cluster-wide regardless")
	edgeCmd.Flags().BoolVar(&cfg.EdgeTLS, "edge-tls", false, "Terminate downstream TLS: serve an HTTPS listener (certs per Gateway listener via SDS) + an HTTP->HTTPS redirect")
	edgeCmd.Flags().Uint32Var(&cfg.EdgeHTTPSPort, "edge-https-port", cfg.EdgeHTTPSPort, "Port the edge TLS listener binds when --edge-tls is set")
	edgeCmd.Flags().StringVar(&cfg.GatewayClassName, "gateway-class", cfg.GatewayClassName, "GatewayClass name whose Gateways this edge serves")
	edgeCmd.Flags().StringVar(&cfg.GeoipCityDB, "geoip-city-db", "", "Path to a MaxMind city-type mmdb (proposal 028). When set, the edge emits x-geo-* headers on routed requests; the reserved x-geo-* namespace is stripped from client requests regardless")
	edgeCmd.Flags().StringSliceVar(&cfg.GeoipHeaders, "geoip-headers", []string{"country"}, "Which geo headers to emit: country, city")
	edgeCmd.Flags().Uint32Var(&cfg.XffNumTrustedHops, "xff-num-trusted-hops", 0, "Trusted proxies in front of the edge (topology fact): feeds BOTH the HCM client-address resolution and the geoip filter's XFF config")
	edgeCmd.Flags().StringVar(&cfg.EdgeServiceName, "edge-service-name", "", "Name of the edge's own LoadBalancer Service (in the edge namespace); its assigned LB address is published as every class-aether Gateway's status.addresses. Empty disables address publication (and per-Gateway Services with it)")
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
	// The edge's identity is its pod name: derive node-name (the xDS/watch
	// identity — the edge has no distinct K8s node identity) from the POD_NAME
	// downward-API env (set by the chart) unless explicitly overridden, so the
	// deployment needn't wire it.
	if cfg.NodeName == "" {
		cfg.NodeName = currentPodName()
	}
	if cfg.NodeName == "" {
		return fmt.Errorf("edge identity unresolved: set POD_NAME (downward API) or pass --node-name")
	}

	// The edge has no CNI/pod storage; it points the (always-empty) local store
	// at the fixed pod-local dir so PreListen's load is a no-op.
	cfg.MountedLocalStorageDir = constants.DefaultEdgeRegistryDir

	l.InfoContext(
		ctx, "starting aether edge proxy control plane",
		"podName", cfg.NodeName,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
		"edgeHTTPPort", cfg.EdgeHTTPPort,
	)

	defer deferLogShutdown(ctx)

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

	scheme, err := buildEdgeScheme()
	if err != nil {
		return err
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, edgeName, Version, l, func(o *ctrl.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		return err
	}
	defer deferTelemetryShutdown(ctx, result.Shutdown)
	m := result.Manager

	spireSource, identityTrustDomain, edgeSpiffeID, err := resolveEdgeIdentity(ctx, m)
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

	snapshotCache := configureEdgeSnapshotCache(edgeSpiffeID, identityTrustDomain)

	// Replace the seeds with the identity SPIRE issues, whenever it does (#740).
	wireEdgeIdentity(ctx, spireSource, snapshotCache)

	ackTracker := ack.NewTracker(l)

	// The edge runs no local workloads, so it loads listeners from an empty
	// storage: PreListen's LoadListenersFromStorage finds zero pods and the
	// edge-mode Listeners() returns the single public-facing listener instead.
	emptyStorage, err := setupStorage(ctx, cfg.MountedLocalStorageDir)
	if err != nil {
		return err
	}

	xdsSrv, err := xdsServer.NewAgentXdsServer(ctx, cfg.ClusterName, cfg.NodeName, identityTrustDomain, reg, emptyStorage, snapshotCache, ackTracker.Callbacks(), l)
	if err != nil {
		return err
	}
	if err = m.Add(xdsSrv); err != nil {
		return fmt.Errorf("failed to add xDS server: %w", err)
	}

	refresher := xdsServer.NewRegistryRefresher(cfg.ClusterName, cfg.NodeName, snapshotCache, reg, l)
	if err = m.Add(refresher); err != nil {
		return fmt.Errorf("failed to add registry refresher: %w", err)
	}

	if err = wireGatewayAPIReconciler(m, snapshotCache, routeNamespace); err != nil {
		return err
	}

	l.DebugContext(ctx, "waiting for local storage to be ready")
	if err = emptyStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.DebugContext(ctx, "starting edge manager")
	return m.Start(ctx)
}

// buildEdgeScheme constructs the manager scheme for the edge: client-go built-ins,
// Gateway API types, and config.aether.io (EdgeConfig/MeshConfig/HTTPFilter).
func buildEdgeScheme() (*runtime.Scheme, error) {
	// Manager scheme = client-go built-ins + the Gateway API types so the
	// reconciler reads typed Gateways/HTTPRoutes (no unstructured).
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register client-go scheme: %w", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		return nil, fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
	}
	// ReferenceGrant is served as v1beta1 (the storage version) — needed to admit
	// cross-namespace backendRefs.
	if err := gatewayv1beta1.Install(scheme); err != nil {
		return nil, fmt.Errorf("register gateway.networking.k8s.io/v1beta1 scheme: %w", err)
	}
	// EdgeConfig / MeshConfig / HTTPFilter (config.aether.io) — the edge resolves
	// per-Gateway EdgeConfig via the parametersRef chain (proposal 029).
	if err := configapisv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register config.aether.io scheme: %w", err)
	}
	return scheme, nil
}

// edgeSpireComponent namespaces the edge's SPIRE identity-wait metrics
// (aether.edge.spire.wait_seconds and friends). The edge shares the agent binary
// but is a separate workload with its own SPIRE registration entry and its own
// failure mode, so it gets its own series rather than being collapsed into the
// node agent's (#210: a fleet-collapsed counter makes rate() lie).
const edgeSpireComponent = "edge"

// edgeIdentityManager is the slice of the controller-runtime manager the edge's
// identity wiring needs. A narrow interface rather than ctrl.Manager is what lets
// the "returns immediately" test run without an apiserver.
type edgeIdentityManager interface {
	runnableAdder
	AddReadyzCheck(name string, check healthz.Checker) error
}

// resolveEdgeIdentity starts acquiring the edge's SPIRE identity and RETURNS
// IMMEDIATELY with the SEED values everything downstream is wired from: the mesh
// domain as trust domain and an empty SPIFFE ID. When SPIRE is disabled it
// registers nothing and returns those same seeds, byte for byte what it did
// before (#421). The caller owns the returned source and must Close it.
//
// This slot used to be a blocking, FATAL 25s wait for the first SVID (issue #740).
// The edge needs MORE from SPIRE than the other three — the trust domain AND its
// own SPIFFE ID name the SDS resources and the upstream mTLS its Envoy presents —
// which is exactly why the wait was here, and exactly why dying on it was wrong: a
// north-south gateway that exits during a SPIRE cold boot takes ingress with it,
// while an edge that starts without an identity simply cannot inject upstream mTLS
// yet. SetEdgeIdentity recomputes the mTLS clusters and pushes a new snapshot, so
// the identity folds in late without a restart (wireEdgeIdentity).
//
// The empty SPIFFE ID is not a placeholder that leaks: recomputeMTLSClusters skips
// mTLS injection entirely while nodeSpiffeID is "", so the edge serves plain
// clusters until the SVID lands rather than clusters naming an identity it does
// not hold.
func resolveEdgeIdentity(ctx context.Context, m edgeIdentityManager) (*commonspire.WaitingSource, string, string, error) {
	if !cfg.SpireEnabled {
		return nil, cfg.MeshDomain, "", nil
	}
	// The Envoy fetches its own SVID from SPIRE directly over the spire_agent SDS
	// cluster, so the edge control plane runs no SPIRE bridge — it only needs the
	// names to program into the clusters, and it can learn them late.
	src := commonspire.NewWaitingSource(cfg.SpireWorkloadSocketPath, cfg.SpireWaitWarnAfter, l, commonspire.WithComponent(edgeSpireComponent))
	if err := m.Add(src); err != nil {
		return nil, "", "", fmt.Errorf("failed to add the SPIRE identity source: %w", err)
	}
	// Load-bearing: the edge is behind a LoadBalancer Service, so NotReady takes
	// this replica out of the ingress endpoints rather than letting it advertise
	// an edge whose upstreams have no identity. No dwell (#740 PR 4): the agent's
	// 2m dwell exists only to keep a DaemonSet's NotReady from arming the node
	// taint, and the edge is a Deployment with no such coupling — a second replica
	// goes on serving while this one waits.
	if err := m.AddReadyzCheck(commonspire.ReadyCheckName, commonspire.ReadyChecker(src, commonspire.ServiceNotReadyDwell)); err != nil {
		return nil, "", "", fmt.Errorf("failed to set up the SPIRE identity ready check: %w", err)
	}
	l.InfoContext(ctx, "acquiring the edge's SVID in the background; startup does not wait for SPIRE",
		"socket", cfg.SpireWorkloadSocketPath, "warnAfter", cfg.SpireWaitWarnAfter)
	return src, cfg.MeshDomain, "", nil
}

// edgeIdentitySink is the slice of the snapshot cache wireEdgeIdentity writes to.
type edgeIdentitySink interface {
	UpdateEdgeIdentity(ctx context.Context, spiffeID, trustDomain string) error
}

// wireEdgeIdentity folds the edge's real identity into the snapshot cache once
// SPIRE issues its first SVID, replacing the seeds resolveEdgeIdentity handed out.
// It is a no-op when SPIRE is disabled and returns immediately otherwise: the work
// happens in a goroutine that ends with ctx.
//
// UpdateEdgeIdentity recomputes the cached mTLS clusters and pushes a snapshot, so
// the edge Envoy picks the identity up on the next CDS update rather than on a
// restart — which is what makes acquiring it late acceptable in the first place.
func wireEdgeIdentity(ctx context.Context, src *commonspire.WaitingSource, sink edgeIdentitySink) {
	if src == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-src.Ready():
		}

		svid, err := src.GetX509SVID()
		if err != nil {
			l.WarnContext(ctx, "failed to read the edge SVID after SPIRE issued it", "error", err)
			return
		}
		spiffeID, trustDomain := svid.ID.String(), svid.ID.TrustDomain().Name()
		l.InfoContext(ctx, "resolved edge identity from SPIRE", "trustDomain", trustDomain, "spiffeID", spiffeID)
		if trustDomain != cfg.MeshDomain {
			// The seed everything else was wired from (xDS server, cluster SDS names)
			// is the mesh domain; SPIRE issuing into a different one is a topology
			// aether does not support (addressing and identity are one domain by
			// design). Say so loudly rather than serving a half-updated edge.
			l.WarnContext(ctx, "SPIRE issues into a different trust domain than the mesh domain",
				"meshDomain", cfg.MeshDomain, "trustDomain", trustDomain)
		}
		if err := sink.UpdateEdgeIdentity(ctx, spiffeID, trustDomain); err != nil {
			l.ErrorContext(ctx, "failed to apply the edge identity to the xDS snapshot", "error", err, "spiffeID", spiffeID)
		}
	}()
}

// configureEdgeSnapshotCache creates and configures the xDS snapshot cache for edge
// mode: geoip, edge HTTP/TLS ports, identity, and access-log/tracing globals.
func configureEdgeSnapshotCache(edgeSpiffeID, identityTrustDomain string) *cache.SnapshotCache {
	snapshotCache := cache.NewSnapshotCache(cfg.NodeName, l)
	snapshotCache.SetMeshDomain(cfg.MeshDomain)
	snapshotCache.SetEmitStatsPod(cfg.EmitStatsPod)
	snapshotCache.SetEdgeGeoip(proxy.GeoipConfig{
		CityDBPath:        cfg.GeoipCityDB,
		Headers:           cfg.GeoipHeaders,
		XffNumTrustedHops: cfg.XffNumTrustedHops,
	}, cfg.XffNumTrustedHops)
	snapshotCache.SetEdgeMode(cfg.EdgeHTTPPort)
	snapshotCache.SetEdgeReadinessPort(proxy.DefaultEdgeReadinessPort)
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

	return snapshotCache
}

// wireGatewayAPIReconciler sets up the Gateway API reconciler for the edge. With TLS
// enabled, it also initializes the Secret provider for TLS cert lookups.
func wireGatewayAPIReconciler(m ctrl.Manager, snapshotCache *cache.SnapshotCache, routeNamespace string) error {
	// Watch Gateways/HTTPRoutes and project them into the cache as the edge's
	// virtual hosts + scoped dependency set. With TLS enabled, also resolve each
	// Gateway listener's cert via the SecretProvider registry (kubernetes provider)
	// and watch their Secrets.
	var secretRegistry *secret.Registry
	if cfg.EdgeTLS {
		secretRegistry = secret.NewRegistry(secret.NewKubernetesProvider(m.GetClient(), routeNamespace))
	}
	gwReconciler := &gatewayapi.Reconciler{
		Client:           m.GetClient(),
		APIReader:        m.GetAPIReader(),
		Sink:             snapshotCache,
		Namespace:        routeNamespace,
		EdgeServiceName:  cfg.EdgeServiceName,
		GatewayClassName: cfg.GatewayClassName,
		MeshDomain:       cfg.MeshDomain,
		Secrets:          secretRegistry,
		Log:              l,
	}
	if err := gwReconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up Gateway API reconciler: %w", err)
	}
	return nil
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
