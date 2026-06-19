package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bpalermo/aether/agent/internal/edge"
	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/manager"
	commonspire "github.com/bpalermo/aether/common/spire"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
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
	registerSharedFlags(edgeCmd)

	// The edge has no CNI/pod storage; it points the (always-empty) local store
	// at a pod-local emptyDir so PreListen's load is a no-op.
	edgeCmd.Flags().StringVar(&cfg.MountedLocalStorageDir, "mounted-registry-dir", "/var/lib/aether/registry", "Pod-local directory for the edge's (empty) local store")
	edgeCmd.Flags().Uint32Var(&cfg.EdgeHTTPPort, "edge-http-port", cfg.EdgeHTTPPort, "Port the edge proxy's public-facing HTTP listener binds")
	edgeCmd.Flags().StringSliceVar(&cfg.EdgeExposes, "expose", nil, "Mesh service names the edge always routes to at their mesh FQDN (comma-separated or repeated); merged with the EdgeRoute CRs")
	edgeCmd.Flags().StringVar(&cfg.EdgeRouteNamespace, "edge-route-namespace", "", "Namespace to watch EdgeRoute CRs in (empty = the edge pod's own namespace)")
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
	l.InfoContext(ctx, "starting aether edge proxy control plane",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
		"edgeHTTPPort", cfg.EdgeHTTPPort,
		"exposes", cfg.EdgeExposes,
	)

	if logShutdown != nil {
		defer func() {
			if shutdownErr := logShutdown(ctx); shutdownErr != nil {
				l.ErrorContext(ctx, "failed to flush OTel logs", "error", shutdownErr)
			}
		}()
	}

	// Watch EdgeRoute CRs in the edge's own namespace (or an override). Scope the
	// manager's informer cache to that namespace so the edge needs only namespaced
	// RBAC, not cluster-wide.
	routeNamespace := cfg.EdgeRouteNamespace
	if routeNamespace == "" {
		routeNamespace = currentNamespace()
	}
	cfg.CacheOptions = &ctrlcache.Options{
		DefaultNamespaces: map[string]ctrlcache.Config{routeNamespace: {}},
	}

	// Manager scheme = client-go built-ins + the typed EdgeRoute CRD so the
	// reconciler reads typed objects (no unstructured).
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register client-go scheme: %w", err)
	}
	if err := crdv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register config.aether.io scheme: %w", err)
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, edgeName, Version, func(o *ctrl.Options) { o.Scheme = scheme })
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
	snapshotCache.SetEdgeMode(cfg.EdgeHTTPPort)
	snapshotCache.SetEdgeIdentity(edgeSpiffeID, identityTrustDomain)
	// Static seed: --expose services routed at their mesh FQDN. The EdgeRoute
	// reconciler merges these with the CRs and re-applies on every change; seed
	// them once up front so the edge exposes them before the first reconcile (and
	// with zero EdgeRoute CRs, when no reconcile fires).
	seedRoutes := make([]cache.EdgeRoute, 0, len(cfg.EdgeExposes))
	for _, svc := range cfg.EdgeExposes {
		seedRoutes = append(seedRoutes, cache.EdgeRoute{Service: svc})
	}
	snapshotCache.SetEdgeRoutes(seedRoutes)

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

	// Watch EdgeRoute CRs and project them (merged with the static seed) into the
	// cache as the edge's routes + scoped dependency set.
	edgeReconciler := &edge.Reconciler{
		Client:    m.GetClient(),
		Sink:      snapshotCache,
		Namespace: routeNamespace,
		Seed:      seedRoutes,
		Log:       l,
	}
	if err = edgeReconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up EdgeRoute reconciler: %w", err)
	}

	l.DebugContext(ctx, "waiting for local storage to be ready")
	if err = emptyStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.DebugContext(ctx, "starting edge manager")
	return m.Start(ctx)
}

// currentNamespace returns the namespace the edge pod runs in (the default
// namespace to watch EdgeRoutes in). It reads POD_NAMESPACE (set via the
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
