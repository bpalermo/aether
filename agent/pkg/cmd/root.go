// Package cmd provides command-line interface and configuration for the Aether agent.
//
// The agent is a Kubernetes DaemonSet component that runs on each node to manage
// an Envoy xDS control plane and a CNI plugin for transparent traffic interception.
// It registers services in a pluggable registry backend (kubernetes, dynamodb, etcd, or cloudmap)
// and generates Envoy configuration (listeners, clusters, endpoints, routes) for local pods.
//
// Command-line flags control the agent's initialization:
//   - debug: Enable debug-level logging
//   - node-name: Kubernetes node name where the agent runs (required)
//   - cluster-name: Kubernetes cluster name (required)
//   - proxy-id: xDS node identifier for the Envoy proxy instance (required)
//   - mounted-registry-dir: Directory where pod data is stored locally
//   - registry-backend: Registry backend selection (kubernetes, dynamodb, etcd, cloudmap)
//   - etcd-endpoints: etcd connection endpoints for etcd backend
//   - cloudmap-namespace: AWS Cloud Map HTTP namespace name
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
	"fmt"

	"github.com/bpalermo/aether/agent/internal/awsconfig"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// name is the controller name used for logging
	name = "aether-agent"
)

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
		l = log.NewLogger(cfg.Debug).WithName(cmd.Name())
		ctrl.SetLogger(l)
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
	// Logging and debugging
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug-level logging for troubleshooting")

	// Kubernetes and cluster identity (required)
	rootCmd.Flags().StringVar(&cfg.NodeName, "node-name", constants.DefaultProxyID, "Kubernetes node name where the agent runs (required)")
	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster-name", constants.DefaultProxyID, "Kubernetes cluster name, used for service discovery (required)")
	rootCmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "Unique identifier for this Envoy proxy instance in the xDS server (required)")

	// Local storage configuration
	rootCmd.Flags().StringVar(&cfg.MountedLocalStorageDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where pod data is stored locally for the CNI plugin")

	// Service registry configuration
	rootCmd.Flags().StringVar(&cfg.RegistryBackend, "registry-backend", "kubernetes", "Registry backend for service discovery (kubernetes, dynamodb, etcd, or cloudmap)")
	rootCmd.Flags().StringSliceVar(&cfg.EtcdEndpoints, "etcd-endpoints", []string{"localhost:2379"}, "Comma-separated etcd endpoints, used when registry-backend is 'etcd'")
	rootCmd.Flags().StringVar(&cfg.CloudMapNamespace, "cloudmap-namespace", constants.DefaultCloudMapNamespace, "AWS Cloud Map HTTP namespace name, used when registry-backend is 'cloudmap'")

	// SPIRE and security configuration
	rootCmd.Flags().BoolVar(&cfg.SpireEnabled, "spire-enabled", true, "Whether to enable SPIRE integration for X.509 SVID management and mTLS")
	rootCmd.Flags().StringVar(&cfg.SpireTrustDomain, "spire-trust-domain", constants.DefaultSpireTrustDomain, "SPIFFE trust domain for the cluster, used for service identity")
	rootCmd.Flags().StringVar(&cfg.SpireAdminSocketPath, "spire-admin-socket", constants.DefaultSpireAdminSocketPath, "Path to SPIRE agent admin socket for X.509 certificate delegation")

	// These calls only fail if the flag name is not registered, which would be a programming error.
	must(rootCmd.MarkFlagRequired("cluster-name"))
	must(rootCmd.MarkFlagRequired("node-name"))
	must(rootCmd.MarkFlagRequired("proxy-id"))
}

// runAgent initializes and runs the Aether agent. It sets up the controller-runtime Manager,
// initializes local storage and registry backends, creates and registers the xDS server,
// CNI gRPC server, and optionally the SPIRE bridge as runnables. The agent then waits
// for local storage to become ready before starting the manager's event loop.
func runAgent(ctx context.Context) error {
	l.Info("starting aether agent",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
	)

	// Create a controller manager
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		return err
	}

	localStorage, err := setupStorage(ctx, cfg.MountedLocalStorageDir)
	if err != nil {
		return err
	}

	reg, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}
	defer reg.Close()

	snapshotCache := cache.NewSnapshotCache(cfg.NodeName, l)

	// Optionally create and start the SPIRE bridge for SDS
	var spireBridge *spire.Bridge
	if cfg.SpireEnabled {
		spireBridge = spire.NewBridge(cfg.SpireAdminSocketPath, snapshotCache, l)
		if err = m.Add(spireBridge); err != nil {
			return fmt.Errorf("failed to add SPIRE bridge: %w", err)
		}
	} else {
		l.Info("SPIRE integration disabled")
	}

	if err = setXDSServer(ctx, m, reg, localStorage, snapshotCache); err != nil {
		return err
	}

	if err = setupCNIServer(m, localStorage, reg, snapshotCache, spireBridge); err != nil {
		return err
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
func setXDSServer(ctx context.Context, m ctrl.Manager, registry registry.Registry, localStorage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache) error {
	// Create xDS server
	xdsSrv, err := xdsServer.NewAgentXdsServer(ctx, cfg.ClusterName, cfg.ProxyServiceNodeID, cfg.SpireTrustDomain, registry, localStorage, snapshotCache, l)
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
func setupCNIServer(m ctrl.Manager, localStorage storage.Storage[*cniv1.CNIPod], registry registry.Registry, snapshotCache *cache.SnapshotCache, spireBridge *spire.Bridge) error {
	// Create a registry and CNI server
	cniSrv, err := cniServer.NewCNIServer(
		cfg.ClusterName,
		cfg.NodeName,
		cfg.ProxyServiceNodeID,
		cfg.SpireTrustDomain,
		localStorage,
		registry,
		snapshotCache,
		spireBridge,
		l,
		m.GetClient(),
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

// setupRegistry creates and initializes a registry backend based on the configured registry-backend flag.
// Supported backends are:
//   - kubernetes: Uses Kubernetes API for service discovery
//   - dynamodb: Uses AWS DynamoDB for service endpoint storage
//   - etcd: Uses etcd for service endpoint storage
//   - cloudmap: Uses AWS Cloud Map for service discovery
func setupRegistry(ctx context.Context, m ctrl.Manager) (registry.Registry, error) {
	var reg registry.Registry

	switch cfg.RegistryBackend {
	case "kubernetes":
		reg = registry.NewKubernetesRegistry(l, m.GetAPIReader(), registry.KubernetesConfig{
			ClusterName: cfg.ClusterName,
		})
	case "dynamodb":
		awsCfg, err := awsconfig.LoadConfig(ctx)
		if err != nil {
			return nil, err
		}
		reg = registry.NewDynamoDBRegistry(l, awsCfg)
	case "etcd":
		reg = registry.NewEtcdRegistry(l, registry.EtcdConfig{
			Endpoints: cfg.EtcdEndpoints,
		})
	case "cloudmap":
		awsCfg, err := awsconfig.LoadConfig(ctx)
		if err != nil {
			return nil, err
		}
		reg = registry.NewCloudMapRegistry(l, awsCfg, cfg.ClusterName,
			registry.WithCloudMapNamespace(cfg.CloudMapNamespace),
		)
	default:
		return nil, fmt.Errorf("unknown registry backend: %s", cfg.RegistryBackend)
	}

	if err := reg.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize registry: %w", err)
	}

	return reg, nil
}

// must panics if err is non-nil. Use only for programming errors that should never occur at runtime.
func must(err error) {
	if err != nil {
		panic(err)
	}
}
