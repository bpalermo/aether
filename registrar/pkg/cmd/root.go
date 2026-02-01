package cmd

import (
	"context"
	"time"

	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registrar/internal/awsconfig"
	"github.com/bpalermo/aether/registrar/internal/registry/ddb"
	"github.com/bpalermo/aether/registrar/pkg/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

const (
	// name is the controller name used for logging
	name = "aether-registrar"
)

var (
	cfg    = NewRegisterConfig()
	logger logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "registrar",
	Short:        "Runs the aether register service.",
	SilenceUsage: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		logger = log.NewLogger(cfg.Debug).WithName("register")
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
	rootCmd.Flags().DurationVar(&cfg.ShutdownTimeout, "shutdownTimeout", 30*time.Second, "Shutdown timeout for graceful shutdown")

	_ = rootCmd.MarkPersistentFlagRequired("cluster")
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runRegistrar(ctx context.Context) error {
	logger.Info("starting register server", "debug", cfg.Debug)

	awsCfg, err := awsconfig.LoadConfig(ctx)
	if err != nil {
		return err
	}

	reg := ddb.NewDynamoDBRegistry(logger, awsCfg)

	srv, err := server.NewRegistrarServer(cfg.srvCfg, reg, logger)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)

	if err = reg.Start(ctx, errCh); err != nil {
		logger.Error(nil, "failed to start registrar registry")
		return err
	}

	if err = srv.Start(errCh); err != nil {
		logger.Error(err, "failed to start registrar server")
		return err
	}

	select {
	case chErr := <-errCh:
		logger.Error(chErr, "initialization error")
		return chErr
	case <-ctx.Done():
		logger.V(1).Info("received shutdown signal")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if shutdownErr := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error(shutdownErr, "shutdown error")
			return shutdownErr
		}
		return nil
	}
}
