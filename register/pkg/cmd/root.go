package cmd

import (
	"context"
	"time"

	"github.com/bpalermo/aether/hook"
	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/register/pkg/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"go.uber.org/zap/zapcore"
)

const (
	// name is the controller name used for logging
	name = "aether-register"
)

var (
	cfg    = NewRegisterConfig()
	debug  bool
	logger logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "register",
	Short:        "Runs the aether register service.",
	SilenceUsage: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		opts := log.DefaultOptions()
		if debug {
			opts.Development = true
			opts.Level = zapcore.DebugLevel
		}
	},
	RunE: func(cmd *cobra.Command, _ []string) (err error) {
		return runRegister(cmd.Context())
	},
	PersistentPostRun: func(_ *cobra.Command, _ []string) {

	},
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().DurationVar(&cfg.ShutdownTimeout, "shutdownTimeout", 30*time.Second, "Shutdown timeout for graceful shutdown")
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runRegister(ctx context.Context) error {

	_, err := server.NewRegisterServer(logger)
	if err != nil {
		return err
	}

	hook.AddShutdownHook(ctx, 30*time.Second, logger)

	return nil
}
