package cmd

import (
	"github.com/bpalermo/aether/log"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
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
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
		logger = ctrl.Log.WithName(name)
	},
	RunE: func(_ *cobra.Command, _ []string) (err error) {
		return runRegister()
	},
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runRegister() error {
	logger.Info("Starting aether register service")
	return nil
}
