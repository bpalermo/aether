package cmd

import (
	"context"
	"log/slog"

	"aethermesh.dev/cni/internal/constants"
	"aethermesh.dev/cni/internal/install"
	"aethermesh.dev/common/log"
	"github.com/spf13/cobra"
)

var (
	// l is the shared slog logger every line of the install goes through, handed
	// to the installer explicitly. Nothing in cni/ binds controller-runtime's
	// global logger, so a package-level ctrl.Log anywhere under the installer is
	// silently discarded (issue #696).
	l *slog.Logger

	cfg = install.NewInstallerConfig()
)

var rootCmd = &cobra.Command{
	Use:          "cni-install",
	Short:        "Installs the CNI binaries into the current host.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = log.Named(log.NewLogger(cfg.Debug), cmd.Name())
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
	rootCmd.Flags().BoolVar(&cfg.CaptureRedirectAllDefault, "capture-redirect-all-default", false, "Write capture_redirect_all_default into the netconf so redirect-all is the default for managed pods (proposal 022, M2-default), opt-out via capture.aether.io/redirect-all=false")
	rootCmd.Flags().BoolVar(&cfg.MeshDNSEnabled, "mesh-dns", false, "Write mesh_dns_enabled into the netconf so the CNI plugin installs the per-pod :53 DNAT (proposal 018, mesh-global FQDN)")
	rootCmd.Flags().StringVar(&cfg.HostIP, "host-ip", "", "Node IP written into the netconf as the mesh-DNS DNAT target (the agent's host-local resolver)")
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func runInstall(ctx context.Context) error {
	l.InfoContext(ctx, "installing CNI binaries")
	installer := install.NewInstaller(l, cfg)
	return installer.Run(ctx)
}
