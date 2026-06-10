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
//   - spire-trust-domain: SPIFFE trust domain for the cluster
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

	"github.com/bpalermo/aether/agent/constants"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/must"
	commonspire "github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// name is the controller name used for logging
	name = "aether-agent"
)

// Version is set at build time via -ldflags (Bazel x_defs).
var Version = "dev"

var (
	cfg = NewAgentConfig()

	l logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "agent",
	Short:        "Runs the aether node agent.",
	Long:         "Runs the Aether agent on a Kubernetes node to manage an Envoy xDS control plane and transparent traffic interception via CNI.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = manager.SetupLogging(cfg.Debug, cmd.Name())
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

	// Kubernetes and cluster identity (required)
	rootCmd.Flags().StringVar(&cfg.NodeName, "node-name", "", "Kubernetes node name where the agent runs (required)")
	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster-name", "", "Kubernetes cluster name, used for service discovery (required)")
	rootCmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "Unique identifier for this Envoy proxy instance in the xDS server (required)")

	// Local storage configuration
	rootCmd.Flags().StringVar(&cfg.MountedLocalStorageDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where pod data is stored locally for the CNI plugin")

	// Registrar configuration
	rootCmd.Flags().StringVar(&cfg.RegistrarAddress, "registrar-address", cfg.RegistrarAddress, "gRPC address of the in-cluster Registrar service")

	// SPIRE and security configuration
	rootCmd.Flags().BoolVar(&cfg.SpireEnabled, "spire-enabled", true, "Whether to enable SPIRE integration for X.509 SVID management and mTLS")
	rootCmd.Flags().StringVar(&cfg.SpireTrustDomain, "spire-trust-domain", constants.DefaultSpireTrustDomain, "SPIFFE trust domain for the cluster, used for service identity")
	rootCmd.Flags().StringVar(&cfg.SpireAdminSocketPath, "spire-admin-socket", constants.DefaultSpireAdminSocketPath, "Path to SPIRE agent admin socket for X.509 certificate delegation")
	rootCmd.Flags().StringVar(&cfg.SpireWorkloadSocketPath, "spire-workload-socket", constants.DefaultSpireWorkloadSocketPath, "Path to the SPIRE Workload API UDS socket used for registrar mTLS")

	// These calls only fail if the flag name is not registered, which would be a programming error.
	must.NoError(rootCmd.MarkFlagRequired("cluster-name"))
	must.NoError(rootCmd.MarkFlagRequired("node-name"))
	must.NoError(rootCmd.MarkFlagRequired("proxy-id"))
}

// runAgent initializes and runs the Aether agent. It sets up the controller-runtime Manager,
// initializes local storage and registry backends, creates and registers the xDS server,
// CNI gRPC server, and optionally the SPIRE bridge as runnables. The agent then waits
// for local storage to become ready before starting the manager's event loop.
func runAgent(ctx context.Context) (retErr error) {
	l.Info("starting aether agent",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
	)

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
				l.Error(shutdownErr, "failed to shutdown telemetry")
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
	// match the secrets the SPIRE bridge delivers. The configured
	// --spire-trust-domain governs only peer authorization and may be the
	// RootCATrustDomain "authorize any" sentinel, which is not a real trust domain.
	var spireSource *commonspire.Source
	identityTrustDomain := cfg.SpireTrustDomain
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
		l.Info("resolved workload trust domain from SPIRE", "trustDomain", identityTrustDomain)
	}

	reg, err := setupRegistrarClient(ctx, spireSource)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reg.Close()) }()

	snapshotCache := cache.NewSnapshotCache(cfg.NodeName, l)

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
		l.Info("SPIRE integration disabled")
	}

	if err = setXDSServer(ctx, m, reg, localStorage, snapshotCache, ackTracker, identityTrustDomain); err != nil {
		return err
	}

	if err = setupCNIServer(m, localStorage, reg, snapshotCache, ackTracker, spireBridge, identityTrustDomain); err != nil {
		return err
	}

	// Rebuild the cluster/endpoint/route snapshot when the registry reports
	// endpoint changes, so services registered after startup become routable
	// without restarting the agent. No-op if the registry can't notify.
	refresher := xdsServer.NewRegistryRefresher(cfg.ClusterName, cfg.NodeName, snapshotCache, reg, l)
	if err = m.Add(refresher); err != nil {
		return fmt.Errorf("failed to add registry refresher: %w", err)
	}

	l.V(1).Info("waiting for local storage to be ready")
	if err = localStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.V(1).Info("local storage is ready, starting manager")
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
		// Peer authorization uses the configured trust domain (which may be the
		// RootCATrustDomain "authorize any" sentinel), independent of the workload
		// trust domain used for SDS resource naming.
		tlsCfg, err := commonspire.ClientTLSConfig(src, cfg.SpireTrustDomain)
		if err != nil {
			return nil, err
		}
		regCfg.DialOptions = []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
		l.Info("registrar client using SPIRE mTLS", "socket", cfg.SpireWorkloadSocketPath, "trustDomain", cfg.SpireTrustDomain)
	} else {
		l.Info("registrar client using insecure transport")
	}

	reg := registry.NewRegistrarRegistry(l, regCfg)

	if err := reg.Initialize(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to initialize registry: %w", err), reg.Close())
	}

	return reg, nil
}

// must panics if err is non-nil. Use only for programming errors that should never occur at runtime.
