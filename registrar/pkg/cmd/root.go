package cmd

import (
	"context"
	"time"

	"github.com/bpalermo/aether/hook"
	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registrar/pkg/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

const (
	// name is the controller name used for logging
	name = "aether-register"
)

var (
	cfg    = NewRegisterConfig()
	logger logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "register",
	Short:        "Runs the aether register service.",
	SilenceUsage: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		logger = log.NewLogger(cfg.Debug).WithName("register")
	},
	RunE: func(cmd *cobra.Command, _ []string) (err error) {
		return runRegister(cmd.Context())
	},
	PersistentPostRun: func(_ *cobra.Command, _ []string) {

	},
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().StringVar(&cfg.srvCfg.Network, "serverNetwork", "tcp", "gRPC server listener network")
	rootCmd.Flags().StringVar(&cfg.srvCfg.Address, "serverAddress", ":50051", "gRPC server listener address")
	rootCmd.Flags().DurationVar(&cfg.ShutdownTimeout, "shutdownTimeout", 30*time.Second, "Shutdown timeout for graceful shutdown")
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runRegister(ctx context.Context) error {
	logger.Info("starting register server", "debug", cfg.Debug)
	srv, err := server.NewRegisterServer(cfg.srvCfg, logger)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	if err = srv.Start(errCh); err != nil {
		logger.Error(err, "failed to start register server")
		return err
	}

	go func() {
		if err := <-errCh; err != nil {
			logger.Error(err, "gRPC server error")
		}
	}()

	hook.AddShutdownHook(ctx, cfg.ShutdownTimeout, logger, srv)

	return nil
}
