package cmd

import (
	"context"
	"log/slog"

	"aethermesh.dev/cni/internal/constants"
	"aethermesh.dev/cni/internal/install"
	"aethermesh.dev/common/buildinfo"
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
	// `--version` (#1429): this binary's GNU build ID, read from its own ELF
	// (//common/buildinfo). Cobra answers it before any hook runs, so nothing
	// is copied to the host.
	rootCmd.Version = buildinfo.Version()
	rootCmd.SetVersionTemplate(buildinfo.Describe("cni-install") + "\n")

	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().StringVar(&cfg.CNIBinSourceDir, "cni-bin-dir", constants.DefaultCNIBinDir, "Directory from where the CNI binaries should be copied")
	rootCmd.Flags().StringVar(&cfg.CNIBinTargetDir, "cni-bin-target-dir", constants.DefaultHostCNIBinDir, "Directory into which to copy the CNI binaries")
	rootCmd.Flags().StringVar(&cfg.MountedCNINetDir, "mounted-cni-net-dir", constants.DefaultHostCNINetDir, "Directory where CNI network configuration files are located")
	registerRetiredOTLPFlags(rootCmd)
	rootCmd.Flags().BoolVar(&cfg.CaptureRedirectAllDefault, "capture-redirect-all-default", false, "Write capture_redirect_all_default into the netconf so redirect-all is the default for managed pods (proposal 022, M2-default), opt-out via capture.aether.io/redirect-all=false")
	rootCmd.Flags().BoolVar(&cfg.MeshDNSEnabled, "mesh-dns", false, "Write mesh_dns_enabled into the netconf so the CNI plugin installs the per-pod :53 DNAT (proposal 018, mesh-global FQDN)")
	rootCmd.Flags().StringVar(&cfg.HostIP, "host-ip", "", "Node IP written into the netconf as the mesh-DNS DNAT target (the agent's host-local resolver)")
}

// registerRetiredOTLPFlags keeps --otlp-endpoint and --otlp-pin-endpoint
// parseable for one release, as deprecated no-ops, so a chart that predates
// #1166 still starts a newer cni-install image. The CNI plugin exports no
// telemetry any more: it forwards its timings to the agent over the CNI gRPC
// socket, and the agent exports them with its own.
func registerRetiredOTLPFlags(cmd *cobra.Command) {
	const msg = "the CNI plugin no longer exports telemetry (#1166); its timings reach the collector through the agent. The flag is ignored"
	var endpoint string
	var pin bool
	cmd.Flags().StringVar(&endpoint, "otlp-endpoint", "", "Ignored (#1166)")
	cmd.Flags().BoolVar(&pin, "otlp-pin-endpoint", true, "Ignored (#1166)")
	for _, name := range []string{"otlp-endpoint", "otlp-pin-endpoint"} {
		if err := cmd.Flags().MarkDeprecated(name, msg); err != nil {
			panic(err)
		}
	}
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
