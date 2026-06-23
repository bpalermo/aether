package cmd

import (
	"context"

	"github.com/bpalermo/aether/cni/internal/constants"
	"github.com/bpalermo/aether/cni/internal/install"
	"github.com/bpalermo/aether/common/log"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

var (
	// The cni-install init container has no request spans, so it stays on logr
	// (the install package uses the controller-runtime global logger). The base
	// logger is still the shared slog handler, bridged to logr for the installer.
	l logr.Logger

	cfg = install.NewInstallerConfig()
)

var rootCmd = &cobra.Command{
	Use:          "cni-install",
	Short:        "Installs the CNI binaries into the current host.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = logr.FromSlogHandler(log.Named(log.NewLogger(cfg.Debug), cmd.Name()).Handler())
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
	rootCmd.Flags().StringVar(&cfg.OTLPEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint written into the netconf so the CNI plugin pushes traces and metrics (e.g. collector:4317); empty disables plugin telemetry")
	rootCmd.Flags().BoolVar(&cfg.TransparentCaptureEnabled, "transparent-capture", false, "Write transparent_capture_enabled into the netconf so the CNI plugin installs the per-pod capture redirect (proposal 018, Phase 3a)")
	rootCmd.Flags().BoolVar(&cfg.MeshDNSEnabled, "mesh-dns", false, "Write mesh_dns_enabled into the netconf so the CNI plugin installs the per-pod :53 DNAT (proposal 018, mesh-global FQDN)")
	rootCmd.Flags().StringVar(&cfg.HostIP, "host-ip", "", "Node IP written into the netconf as the mesh-DNS DNAT target (the agent's host-local resolver)")
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
