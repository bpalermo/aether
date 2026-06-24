package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/must"
	"github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/registrar/internal/mcs"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/bpalermo/aether/registrar/internal/services"
	"github.com/bpalermo/aether/registry"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"

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
	rootCmd.Flags().StringVar(&cfg.Region, "region", cfg.Region, "Region owning this registrar's etcd partition (etcd backend; proposal 006). MUST be unique per regional etcd cluster: one region = one etcd. Pointing two etcds at the same region splits the registry; pointing two regions at one etcd collides their writes.")
	rootCmd.Flags().StringVar(&cfg.RegistryBackend, "registry-backend", cfg.RegistryBackend, "Registry backend (kubernetes, dynamodb, or etcd)")
	rootCmd.Flags().StringSliceVar(&cfg.EtcdEndpoints, "etcd-endpoints", cfg.EtcdEndpoints, "Comma-separated etcd endpoints")
	rootCmd.Flags().DurationVar(&cfg.SyncInterval, "sync-interval", cfg.SyncInterval, "How often to sync from the registry")
	rootCmd.Flags().BoolVar(&cfg.GenerateMeshServices, "generate-mesh-services", false, "Project the mesh catalog into selectorless k8s Services on the mesh port (transparent-capture VIPs, proposal 018 Phase 3a)")
	rootCmd.Flags().BoolVar(&cfg.EnableMCS, "enable-mcs", false, "Enable Multi-Cluster Services (MCS-API) phase 1: export ServiceExports to the registry and materialize ServiceImports + clusterset VIPs (proposals 018 + 006; requires the etcd backend)")
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
		// region names this registrar's etcd partition (proposal 006). Surfaced at
		// startup because the region<->etcd 1:1 invariant is not locally enforceable
		// (a registrar can't see a peer pointing a DIFFERENT etcd at the same region):
		// an operator spots a misconfiguration by comparing this across registrars.
		"region", cfg.Region,
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

	var bootstrapOpts []func(*ctrl.Options)
	if cfg.EnableMCS {
		// MCS phase 1 reconciles typed ServiceExport/ServiceImport objects, so the
		// manager scheme must carry client-go built-ins + the MCS-API group.
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			return fmt.Errorf("register client-go scheme: %w", err)
		}
		if err := mcsv1alpha1.AddToScheme(scheme); err != nil {
			return fmt.Errorf("register MCS-API scheme: %w", err)
		}
		bootstrapOpts = append(bootstrapOpts, func(o *ctrl.Options) { o.Scheme = scheme })
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, name, Version, bootstrapOpts...)
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

	// Transparent-capture VIPs (proposal 018, Phase 3a): the leader registrar
	// projects the mesh catalog into selectorless k8s Services on the mesh port.
	// Default off — adds nothing (and no Service-write RBAC) unless enabled.
	if cfg.GenerateMeshServices {
		gen := &services.Generator{
			Client:   m.GetClient(),
			Snapshot: snapshot,
			MeshPort: int32(constants.ProxyOutboundPort),
			Interval: cfg.SyncInterval,
			Log:      l,
		}
		if err = m.Add(gen); err != nil {
			return fmt.Errorf("failed to add mesh-Service generator: %w", err)
		}
	}

	// Multi-Cluster Services (MCS-API) phase 1 (proposals 018 + 006): the leader
	// registrar exports local ServiceExports to the registry and materializes
	// ServiceImports + clusterset VIPs from the clusterset-wide export view. It
	// needs a registry backend with a cross-cluster export plane (etcd); other
	// backends don't implement registry.ServiceExporter, so this is a hard error
	// rather than a silent no-op (an operator enabling MCS on a non-etcd backend
	// has misconfigured the deployment).
	if cfg.EnableMCS {
		exporter, ok := reg.(registry.ServiceExporter)
		if !ok {
			return fmt.Errorf("--enable-mcs requires a registry backend with a cross-cluster export plane (etcd); backend %q does not implement it", cfg.RegistryBackend)
		}
		exportCtl := &mcs.ExportController{
			Client:   m.GetClient(),
			Exporter: exporter,
			Log:      l,
		}
		if err = exportCtl.SetupWithManager(m); err != nil {
			return fmt.Errorf("failed to set up MCS ServiceExport controller: %w", err)
		}
		importGen := &mcs.ImportGenerator{
			Client:   m.GetClient(),
			Exporter: exporter,
			MeshPort: int32(constants.ProxyOutboundPort),
			Interval: cfg.SyncInterval,
			Log:      l,
		}
		if err = m.Add(importGen); err != nil {
			return fmt.Errorf("failed to add MCS ServiceImport generator: %w", err)
		}
		l.InfoContext(ctx, "MCS-API phase 1 enabled (ServiceExport->registry, registry->ServiceImport+clusterset VIP)")
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
