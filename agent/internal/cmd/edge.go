package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	"github.com/bpalermo/aether/common/manager"
	commonspire "github.com/bpalermo/aether/common/spire"
	"github.com/spf13/cobra"
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

	edgeCmd.Flags().Uint32Var(&cfg.EdgeHTTPPort, "edge-http-port", cfg.EdgeHTTPPort, "Port the edge proxy's public-facing HTTP listener binds")
	edgeCmd.Flags().StringSliceVar(&cfg.EdgeExposes, "expose", nil, "Mesh service names the edge routes to (comma-separated or repeated); seeds the exposed set until the EdgeRoute CRD watch lands")
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

	result, err := manager.Bootstrap(ctx, cfg.Config, edgeName, Version)
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
	// Seed the exposed set (PR1: a static flag; a later PR swaps in an EdgeRoute
	// watch). The scoped registrar watch and cluster snapshot follow this set.
	snapshotCache.SetStaticDependencies(cfg.EdgeExposes)

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

	l.DebugContext(ctx, "waiting for local storage to be ready")
	if err = emptyStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.DebugContext(ctx, "starting edge manager")
	return m.Start(ctx)
}
