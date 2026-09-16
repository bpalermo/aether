package cmd

import (
	"fmt"

	"aethermesh.dev/common/log"
	"aethermesh.dev/prober/internal/prober"
	"github.com/spf13/cobra"
)

// Version is stamped at build time (see BUILD.bazel x_defs).
var Version = "dev"

// GetCommand builds the prober root command.
func GetCommand() *cobra.Command {
	cfg := prober.DefaultConfig()
	cmd := &cobra.Command{
		Use:   "prober",
		Short: "Synthetic mesh-availability prober (proposal 013)",
		Long: "Black-box probes the mesh data plane from the client side and emits its own " +
			"pass/fail SLI, capturing the connection-level failures the proxy-emitted " +
			"aether_stats metric is structurally blind to during hot restarts.",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			// //common/log (slog), not controller-runtime's zap wrapper: that
			// import was the prober's second path to controller-runtime and a
			// leftover from the logr/zap -> slog migration (issue #772, phase B1).
			// The JSON record keeps the "level", "caller" and "error" keys; the
			// timestamp and message keys are slog's ("timestamp"/"message" rather
			// than zap's "ts"/"msg"). Nothing consumes prober log lines — the SLI
			// and every alert read the aether_probe_* metrics.
			logger := log.NewLogger(false)
			p, err := prober.New(c.Context(), cfg, logger, Version)
			if err != nil {
				return fmt.Errorf("init prober: %w", err)
			}
			return p.Run(c.Context())
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.Egress, "egress", cfg.Egress, "local mesh egress listener host:port")
	f.StringVar(&cfg.LivenessPath, "liveness-path", cfg.LivenessPath, "proxy local-reply liveness route path")
	f.StringVar(&cfg.LivenessAuthority, "liveness-authority", cfg.LivenessAuthority, "reserved Host authority for the liveness probe (must not be a real service)")
	f.StringVar(&cfg.MeshDomain, "mesh-domain", cfg.MeshDomain, "mesh authority suffix for reachability targets")
	f.StringSliceVar(&cfg.ReachabilityTargets, "reachability-targets", cfg.ReachabilityTargets, "optional echo upstream service names for the reachability tier")
	f.StringSliceVar(&cfg.MeshDNSTargets, "mesh-dns-targets", cfg.MeshDNSTargets, "optional namespace-qualified FQDN authorities (host[:port], default port 18081) for the mesh_dns tier; these are resolved through the mesh DNS path rather than Host-overridden")
	f.Float64Var(&cfg.Rate, "rate", cfg.Rate, "probes per second per target")
	f.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "per-probe timeout")
	f.IntVar(&cfg.MaxConcurrent, "max-concurrent", cfg.MaxConcurrent, "max in-flight probes per target")
	f.StringVar(&cfg.OTLPEndpoint, "otlp-endpoint", cfg.OTLPEndpoint, "OTLP gRPC collector endpoint host:port; empty disables telemetry")
	return cmd
}
