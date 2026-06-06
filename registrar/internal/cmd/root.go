package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/must"
	"github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	name = "aether-registrar"
)

// Version is set at build time via -ldflags (Bazel x_defs).
var Version = "dev"

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
		l = manager.SetupLogging(cfg.Debug, cmd.Name())
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
	manager.RegisterFlags(rootCmd, &cfg.Config)

	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster-name", "", "Kubernetes cluster name (required)")
	rootCmd.Flags().StringVar(&cfg.RegistryBackend, "registry-backend", cfg.RegistryBackend, "Registry backend (kubernetes, dynamodb, etcd, or cloudmap)")
	rootCmd.Flags().StringSliceVar(&cfg.EtcdEndpoints, "etcd-endpoints", cfg.EtcdEndpoints, "Comma-separated etcd endpoints")
	rootCmd.Flags().StringVar(&cfg.CloudMapNamespace, "cloudmap-namespace", cfg.CloudMapNamespace, "AWS Cloud Map HTTP namespace name")
	rootCmd.Flags().DurationVar(&cfg.SyncInterval, "sync-interval", cfg.SyncInterval, "How often to sync from the registry")
	rootCmd.Flags().StringVar(&cfg.GRPCAddress, "grpc-address", cfg.GRPCAddress, "gRPC listen address")

	rootCmd.Flags().BoolVar(&cfg.SpireEnabled, "spire-enabled", cfg.SpireEnabled, "Enable SPIRE mTLS for the gRPC server")
	rootCmd.Flags().StringVar(&cfg.SpireWorkloadSocketPath, "spire-workload-socket", cfg.SpireWorkloadSocketPath, "Path to the SPIRE Workload API UDS socket")
	rootCmd.Flags().StringVar(&cfg.SpireTrustDomain, "spire-trust-domain", cfg.SpireTrustDomain, "SPIFFE trust domain authorized for mTLS peers")

	must.NoError(rootCmd.MarkFlagRequired("cluster-name"))
}

func runRegistrar(ctx context.Context) (retErr error) {
	l.Info("starting aether registrar",
		"clusterName", cfg.ClusterName,
		"registryBackend", cfg.RegistryBackend,
		"syncInterval", cfg.SyncInterval,
		"grpcAddress", cfg.GRPCAddress,
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
		"spireEnabled", cfg.SpireEnabled,
	)

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

	reg, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reg.Close()) }()

	snapshot := server.NewSnapshot()
	broadcaster := server.NewBroadcaster(l)

	syncer := server.NewSyncer(reg, snapshot, broadcaster, cfg.SyncInterval, l)
	if err = m.Add(syncer); err != nil {
		return fmt.Errorf("failed to add syncer: %w", err)
	}

	var grpcOpts []grpc.ServerOption
	if cfg.SpireEnabled {
		trustDomain, tdErr := spiffeid.TrustDomainFromString(cfg.SpireTrustDomain)
		if tdErr != nil {
			return fmt.Errorf("invalid SPIRE trust domain %q: %w", cfg.SpireTrustDomain, tdErr)
		}
		src, srcErr := spire.NewSource(ctx, cfg.SpireWorkloadSocketPath)
		if srcErr != nil {
			return srcErr
		}
		defer func() { retErr = errors.Join(retErr, src.Close()) }()
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(spire.ServerTLSConfig(src, trustDomain))))
		l.Info("SPIRE mTLS enabled for gRPC server", "socket", cfg.SpireWorkloadSocketPath, "trustDomain", cfg.SpireTrustDomain)
	} else {
		l.Info("SPIRE disabled, gRPC server will use insecure transport")
	}

	grpcSrv := server.NewRegistrarServer(reg, snapshot, broadcaster, cfg.GRPCAddress, l, grpcOpts...)
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
		return nil, errors.Join(fmt.Errorf("failed to initialize registry: %w", err), reg.Close())
	}

	return reg, nil
}
