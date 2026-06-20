package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/must"
	"github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/bpalermo/aether/registry"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
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

	l           *slog.Logger
	logShutdown func(context.Context) error
)

var rootCmd = &cobra.Command{
	Use:          "registrar",
	Short:        "Runs the aether registrar service.",
	Long:         "Runs the Aether registrar that proxies registry operations, caches endpoints, and streams changes to agents.",
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) (err error) {
		l, logShutdown, err = manager.SetupManagerLogging(cmd.Context(), cfg.Config, name, Version)
		return err
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
	rootCmd.Flags().StringVar(&cfg.Region, "region", cfg.Region, "Region owning this registrar's etcd partition (etcd backend; proposal 006)")
	rootCmd.Flags().StringVar(&cfg.RegistryBackend, "registry-backend", cfg.RegistryBackend, "Registry backend (kubernetes, dynamodb, or etcd)")
	rootCmd.Flags().StringSliceVar(&cfg.EtcdEndpoints, "etcd-endpoints", cfg.EtcdEndpoints, "Comma-separated etcd endpoints")
	rootCmd.Flags().DurationVar(&cfg.SyncInterval, "sync-interval", cfg.SyncInterval, "How often to sync from the registry")
	rootCmd.Flags().StringVar(&cfg.GRPCAddress, "grpc-address", cfg.GRPCAddress, "gRPC listen address")

	// SPIRE on/off is aether system config (inherited from the umbrella globals);
	// the socket path and trust domain are per-instance.
	rootCmd.Flags().BoolVar(&cfg.SpireEnabled, "spire-enabled", cfg.SpireEnabled, "Enable SPIRE mTLS for the gRPC server")
	rootCmd.Flags().StringVar(&cfg.SpireWorkloadSocketPath, "spire-workload-socket", cfg.SpireWorkloadSocketPath, "Path to the SPIRE Workload API UDS socket")
	rootCmd.Flags().StringVar(&cfg.SpireTrustDomain, "spire-trust-domain", cfg.SpireTrustDomain, "SPIFFE trust domain authorized for mTLS peers")

	must.NoError(rootCmd.MarkFlagRequired("cluster-name"))
}

func runRegistrar(ctx context.Context) (retErr error) {
	l.InfoContext(ctx, "starting aether registrar",
		"clusterName", cfg.ClusterName,
		"registryBackend", cfg.RegistryBackend,
		"syncInterval", cfg.SyncInterval,
		"grpcAddress", cfg.GRPCAddress,
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
		"spireEnabled", cfg.SpireEnabled,
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

	reg, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reg.Close()) }()

	// Server metrics ride the global MeterProvider registered by Bootstrap;
	// without --otel-enabled the meter is a no-op, so this is always safe.
	serverMetrics, err := server.NewMetrics(otel.Meter("aether/registrar"))
	if err != nil {
		return fmt.Errorf("failed to create server metrics: %w", err)
	}

	snapshot := server.NewSnapshot()
	broadcaster := server.NewBroadcaster(l, serverMetrics)

	syncer := server.NewSyncer(reg, snapshot, broadcaster, cfg.SyncInterval, l, serverMetrics)
	if err = m.Add(syncer); err != nil {
		return fmt.Errorf("failed to add syncer: %w", err)
	}

	var grpcOpts []grpc.ServerOption
	if cfg.SpireEnabled {
		src, srcErr := spire.NewSource(ctx, cfg.SpireWorkloadSocketPath)
		if srcErr != nil {
			return srcErr
		}
		defer func() { retErr = errors.Join(retErr, src.Close()) }()
		tlsCfg, tlsErr := spire.ServerTLSConfig(src, cfg.SpireTrustDomain)
		if tlsErr != nil {
			return tlsErr
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
		l.InfoContext(ctx, "SPIRE mTLS enabled for gRPC server", "socket", cfg.SpireWorkloadSocketPath, "trustDomain", cfg.SpireTrustDomain)
	} else {
		l.InfoContext(ctx, "SPIRE disabled, gRPC server will use insecure transport")
	}

	grpcSrv := server.NewRegistrarServer(reg, snapshot, broadcaster, cfg.GRPCAddress, l, serverMetrics, grpcOpts...)
	// Snapshot-serving RPCs block until the syncer's first cycle so a freshly
	// rolled registrar never serves an empty/partial world view to agents.
	grpcSrv.GateOnSync(syncer.Synced())
	// Snapshot-first registry mutations: apply + broadcast immediately, flush
	// the external-registry write asynchronously with retries (write-behind;
	// the sync loop overlays pending intents so it never regresses them).
	writeBehind := server.NewWriteBehindQueue(reg, l, serverMetrics)
	grpcSrv.UseWriteBehind(writeBehind)
	syncer.UseWriteBehind(writeBehind)
	if err = m.Add(writeBehind); err != nil {
		return fmt.Errorf("failed to add write-behind queue: %w", err)
	}
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
		// Region comes from the standard AWS chain (AWS_REGION env — set by the
		// chart's aws.region value — shared config, IMDS), falling back to
		// us-east-1 so bare runs keep the historical default.
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		if awsCfg.Region == "" {
			awsCfg.Region = "us-east-1"
		}
		reg = registry.NewDynamoDBRegistry(l, awsCfg)
	case "etcd":
		reg = registry.NewEtcdRegistry(l, registry.EtcdConfig{
			Endpoints: cfg.EtcdEndpoints,
			Region:    cfg.Region,
			Cluster:   cfg.ClusterName,
		})
	default:
		return nil, fmt.Errorf("unsupported registry backend: %s (supported: kubernetes, dynamodb, etcd)", cfg.RegistryBackend)
	}

	if err := reg.Initialize(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to initialize registry: %w", err), reg.Close())
	}

	return reg, nil
}
