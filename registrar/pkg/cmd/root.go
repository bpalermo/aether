package cmd

import (
	"context"
	"time"

	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registrar/internal/awsconfig"
	"github.com/bpalermo/aether/registrar/internal/registry"
	"github.com/bpalermo/aether/registrar/internal/registry/ddb"
	"github.com/bpalermo/aether/registrar/internal/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// name is the controller name used for logging
	name = "aether-registrar"
)

var (
	cfg = NewRegisterConfig()

	l logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "registrar",
	Short:        "Runs the aether register service.",
	SilenceUsage: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		l = log.NewLogger(cfg.Debug).WithName("registrar")
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
	rootCmd.Flags().StringVar(&cfg.srvCfg.ClusterName, "cluster", "unknown", "Cluster name. It will be used to push registration info to the registry.")
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
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		return err
	}

	reg, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}

	if err = setupRegistrar(m, reg); err != nil {
		return err
	}

	return m.Start(ctx)
}

func setupRegistrar(m ctrl.Manager, reg registry.Registry) error {
	srv, err := server.NewRegistrarServer(cfg.srvCfg, reg, l)
	if err != nil {
		return err
	}

	if err = m.Add(srv); err != nil {
		return err
	}

	return nil
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
