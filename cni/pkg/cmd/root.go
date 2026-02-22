package cmd

import (
	"context"

	"github.com/bpalermo/aether/cni/internal/constants"
	"github.com/bpalermo/aether/cni/internal/install"
	"github.com/bpalermo/aether/log"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

var (
	l logr.Logger

	cfg = install.NewInstallerConfig()
)

var rootCmd = &cobra.Command{
	Use:          "cni-install",
	Short:        "Installs the CNI binaries into the current host.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = log.NewLogger(cfg.Debug).WithName(cmd.Name())
	},
	RunE: func(cmd *cobra.Command, _ []string) (err error) {
		return runInstall(cmd.Context())
	},
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().StringVar(&cfg.CNIBinSourceDir, "cni-bin-dir", constants.DefaultCNIBinDir, "Directory from where the CNI binaries should be copied")
	rootCmd.Flags().StringVar(&cfg.CNIBinTargetDir, "cni-bin-target-dir", constants.DefaultHostCNIBinDir, "Directory into which to copy the CNI binaries")
	rootCmd.Flags().StringVar(&cfg.MountedCNINetDir, "mounted-cni-net-dir", constants.DefaultHostCNINetDir, "Directory where CNI network configuration files are located")
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runInstall(ctx context.Context) error {
	l.Info("installing CNI binaries")
	installer := install.NewInstaller(l, cfg)
	return installer.Run(ctx)
}
