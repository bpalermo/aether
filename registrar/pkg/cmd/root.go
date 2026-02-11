package cmd

import (
	"context"
	"time"

	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registrar/internal/awsconfig"
	"github.com/bpalermo/aether/registrar/internal/controller"
	"github.com/bpalermo/aether/registrar/internal/registry"
	"github.com/bpalermo/aether/registrar/internal/registry/ddb"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	cfg = NewRegistrarConfig()

	l logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "registrar",
	Short:        "Runs the aether register service.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = log.NewLogger(cfg.Debug).WithName(cmd.Name())
		// Set the controller-runtime logger
		ctrl.SetLogger(l)
	},
	RunE: func(cmd *cobra.Command, _ []string) (err error) {
		return runRegistrar(cmd.Context())
	},
	PersistentPostRun: func(_ *cobra.Command, _ []string) {

	},
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster", "unknown", "Cluster name. It will be used to push registration info to the registry.")
	rootCmd.Flags().StringVar(&cfg.srvCfg.Network, "serverNetwork", "tcp", "gRPC server listener network")
	rootCmd.Flags().StringVar(&cfg.srvCfg.Address, "serverAddress", ":50051", "gRPC server listener address")
	rootCmd.Flags().DurationVar(&cfg.srvCfg.ShutdownTimeout, "shutdownTimeout", 30*time.Second, "Shutdown timeout for graceful shutdown")

	_ = rootCmd.MarkPersistentFlagRequired("cluster")
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runRegistrar(ctx context.Context) error {
	l.Info("starting registrar server", "debug", cfg.Debug)

	// Create a controller manager
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		HealthProbeBindAddress: ":8080",
		Metrics: metricsserver.Options{
			BindAddress: ":8081",
		},
	})
	if err != nil {
		return err
	}

	reg, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}

	rSrv, err := setupRegistrar(ctx, m, reg)
	if err != nil {
		return err
	}

	rCtrl := controller.NewRegistrarController(cfg.ClusterName, m.GetClient(), reg, l)
	err = rCtrl.SetupWithManager(m)
	if err != nil {
		return err
	}

	err = m.AddHealthzCheck("xds", rSrv.HealthzCheck)
	if err != nil {
		return err
	}

	err = m.AddReadyzCheck("xds", rSrv.ReadyzCheck)
	if err != nil {
		return err
	}

	return m.Start(ctx)
}

func setupRegistrar(ctx context.Context, m ctrl.Manager, reg registry.Registry) (*server.RegistrarServer, error) {
	srv, err := server.NewRegistrarServer(ctx, cfg.srvCfg, reg, l)
	if err != nil {
		return nil, err
	}

	if err = m.Add(srv); err != nil {
		return nil, err
	}

	return srv, nil
}

func setupRegistry(ctx context.Context, m ctrl.Manager) (registry.Registry, error) {
	awsCfg, err := awsconfig.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}

	reg := ddb.NewDynamoDBRegistry(l, awsCfg)
	if err = m.Add(reg); err != nil {
		return nil, err
	}

	return reg, nil
}
