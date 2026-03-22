package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/bpalermo/aether/common/must"
	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registry"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	name = "aether-registrar"
)

var (
	cfg = NewRegistrarConfig()

	l logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "registrar",
	Short:        "Runs the aether registrar service.",
	Long:         "Runs the Aether registrar that proxies registry operations, caches endpoints, and streams changes to agents.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = log.NewLogger(cfg.Debug).WithName(cmd.Name())
		ctrl.SetLogger(l)
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runRegistrar(cmd.Context())
	},
}

// GetCommand returns the main cobra.Command for the registrar.
func GetCommand() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug-level logging")
	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster-name", "", "Kubernetes cluster name (required)")
	rootCmd.Flags().StringVar(&cfg.RegistryBackend, "registry-backend", cfg.RegistryBackend, "Registry backend (kubernetes, dynamodb, etcd, or cloudmap)")
	rootCmd.Flags().StringSliceVar(&cfg.EtcdEndpoints, "etcd-endpoints", cfg.EtcdEndpoints, "Comma-separated etcd endpoints")
	rootCmd.Flags().StringVar(&cfg.CloudMapNamespace, "cloudmap-namespace", cfg.CloudMapNamespace, "AWS Cloud Map HTTP namespace name")
	rootCmd.Flags().DurationVar(&cfg.SyncInterval, "sync-interval", cfg.SyncInterval, "How often to sync from the registry")
	rootCmd.Flags().StringVar(&cfg.GRPCAddress, "grpc-address", cfg.GRPCAddress, "gRPC listen address")

	must.NoError(rootCmd.MarkFlagRequired("cluster-name"))
}

func runRegistrar(ctx context.Context) error {
	l.Info("starting aether registrar",
		"clusterName", cfg.ClusterName,
		"registryBackend", cfg.RegistryBackend,
		"syncInterval", cfg.SyncInterval,
		"grpcAddress", cfg.GRPCAddress,
	)

	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		HealthProbeBindAddress: "0", // Disable health probe server
		Metrics: metricsserver.Options{
			BindAddress: "0", // Disable metrics server
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create manager: %w", err)
	}

	reg, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}
	defer reg.Close()

	snapshot := server.NewSnapshot()
	broadcaster := server.NewBroadcaster(l)

	syncer := server.NewSyncer(reg, snapshot, broadcaster, cfg.SyncInterval, l)
	if err = m.Add(syncer); err != nil {
		return fmt.Errorf("failed to add syncer: %w", err)
	}

	grpcSrv := server.NewRegistrarServer(reg, snapshot, broadcaster, cfg.GRPCAddress, l)
	if err = m.Add(grpcSrv); err != nil {
		return fmt.Errorf("failed to add gRPC server: %w", err)
	}

	return m.Start(ctx)
}

func setupRegistry(ctx context.Context, m ctrl.Manager) (registry.Registry, error) {
	var reg registry.Registry

	switch cfg.RegistryBackend {
	case "kubernetes":
		reg = registry.NewKubernetesRegistry(l, m.GetAPIReader(), registry.KubernetesConfig{
			ClusterName: cfg.ClusterName,
		})
	case "dynamodb":
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		reg = registry.NewDynamoDBRegistry(l, awsCfg)
	case "etcd":
		reg = registry.NewEtcdRegistry(l, registry.EtcdConfig{
			Endpoints: cfg.EtcdEndpoints,
		})
	case "cloudmap":
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		reg = registry.NewCloudMapRegistry(l, awsCfg, cfg.ClusterName,
			registry.WithCloudMapNamespace(cfg.CloudMapNamespace),
		)
	default:
		return nil, fmt.Errorf("unsupported registry backend: %s (supported: kubernetes, dynamodb, etcd, cloudmap)", cfg.RegistryBackend)
	}

	if err := reg.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize registry: %w", err)
	}

	return reg, nil
}
