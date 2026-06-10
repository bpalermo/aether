package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bpalermo/aether/agent/internal/proxy/hotrestart"
	"github.com/bpalermo/aether/common/manager"
	"github.com/spf13/cobra"
)

// supervisorCfg holds the flag-bound configuration for the proxy-supervisor
// subcommand. SPIKE: see docs/proposals/001_proxy-hot-restart.md.
var (
	supervisorCfg            hotrestart.Config
	supervisorTelemetryCfg   hotrestart.TelemetryConfig
	supervisorMetricsEnabled bool
	supervisorDebug          bool
	supervisorInstallPath    string
	supervisorReadinessCheck bool
)

var proxySupervisorCmd = &cobra.Command{
	Use:          "proxy-supervisor",
	Short:        "Supervises the Envoy proxy with hot-restart support (SPIKE).",
	Long:         "Runs as the aether-proxy container entrypoint, forking and hot-restarting Envoy across restart epochs so bootstrap-config and binary upgrades happen without dropping connections.",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// --install-path lets this (statically linked) binary copy itself onto a
		// shared volume from an initContainer, so the runtime container can be the
		// Envoy image (which carries Envoy and its shared libraries) with the
		// supervisor injected alongside it.
		if supervisorInstallPath != "" {
			return installSelf(supervisorInstallPath)
		}
		// --readiness-check is the exec readiness probe: exit 0 iff the supervisor's
		// pod-local ready marker is present (the distroless Envoy image has no shell
		// or cat, so the probe re-execs this binary).
		if supervisorReadinessCheck {
			if _, err := os.Stat(supervisorCfg.ReadyMarkerPath); err != nil {
				return fmt.Errorf("not ready: %w", err)
			}
			return nil
		}
		log := manager.SetupLogging(supervisorDebug, cmd.Name())

		// Metrics are the supervisor's crash forensics: the wedge watchdog exits
		// the process non-zero, so the deferred Shutdown flush is what gets the
		// wedge counter out before the pod is recreated. Telemetry failures are
		// never fatal — the supervisor's job is keeping Envoy alive.
		var metrics *hotrestart.SupervisorMetrics
		if supervisorMetricsEnabled {
			supervisorTelemetryCfg.ServiceVersion = Version
			telemetry, telErr := hotrestart.NewTelemetry(cmd.Context(), supervisorTelemetryCfg, log)
			if telErr != nil {
				log.Error(telErr, "failed to set up supervisor telemetry; continuing without metrics")
			} else {
				defer func() {
					if shutdownErr := telemetry.Shutdown(); shutdownErr != nil {
						log.V(1).Error(shutdownErr, "failed to flush supervisor metrics")
					}
				}()
				go telemetry.Serve(cmd.Context())
				if metrics, telErr = hotrestart.NewSupervisorMetrics(telemetry.Meter()); telErr != nil {
					log.Error(telErr, "failed to create supervisor metrics; continuing without metrics")
				}
			}
		}

		return hotrestart.New(supervisorCfg, log, metrics).Run(cmd.Context())
	},
}

// installSelf copies the running executable to dest (0o755). Linux-only via
// /proc/self/exe; the supervisor only ever runs on Linux nodes.
func installSelf(dest string) error {
	src, err := os.Open("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("opening self: %w", err)
	}
	defer func() { _ = src.Close() }()

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying binary: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, filepath.Clean(dest)); err != nil {
		return fmt.Errorf("installing to %s: %w", dest, err)
	}
	return nil
}

func init() {
	f := proxySupervisorCmd.Flags()
	f.BoolVar(&supervisorDebug, "debug", false, "Enable debug-level logging")
	f.StringVar(&supervisorInstallPath, "install-path", "", "If set, copy this binary to the given path and exit (for initContainer self-install onto a shared volume)")
	f.StringVar(&supervisorCfg.EnvoyPath, "envoy-path", "/usr/local/bin/envoy", "Path to the Envoy binary")
	f.StringVar(&supervisorCfg.ConfigPath, "config", "/etc/envoy/envoy.yaml", "Envoy bootstrap config path (-c); a change to this file triggers a hot restart when --watch-config is set")
	f.Uint32Var(&supervisorCfg.BaseID, "base-id", 0, "Envoy --base-id, pinned so successive epochs share one shared-memory segment")
	f.DurationVar(&supervisorCfg.DrainTime, "drain-time", 45*time.Second, "Envoy --drain-time-s: graceful connection-close window for the draining epoch")
	f.DurationVar(&supervisorCfg.ParentShutdownTime, "parent-shutdown-time", 60*time.Second, "Envoy --parent-shutdown-time-s: when the previous epoch is terminated (must exceed --drain-time)")
	f.StringArrayVar(&supervisorCfg.ExtraArgs, "envoy-arg", nil, "Extra argument appended to every Envoy invocation (repeatable); keep --concurrency constant across epochs")
	f.BoolVar(&supervisorCfg.WatchConfig, "watch-config", false, "Watch --config and self-trigger a hot restart when the bootstrap config changes")
	f.StringVar(&supervisorCfg.StateDir, "state-dir", "", "Shared-hostPath dir for the per-node epoch heartbeat (enables Strategy B cross-pod hot restart)")
	f.StringVar(&supervisorCfg.ReadyMarkerPath, "ready-marker", "", "Pod-local path for the readiness marker maintained while Envoy is live at the newest epoch")
	f.StringVar(&supervisorCfg.AdminAddress, "admin-address", "127.0.0.1:9901", "Envoy admin host:port used for the readiness check")
	f.BoolVar(&supervisorReadinessCheck, "readiness-check", false, "Exit 0 iff the --ready-marker file exists (exec readiness probe mode)")
	f.DurationVar(&supervisorCfg.HandoffDeadline, "handoff-deadline", 0, "Watchdog: max time a hot-restart epoch may stay not-LIVE after launch before the supervisor exits non-zero (0 = 2m default)")
	f.DurationVar(&supervisorCfg.AdminUnresponsiveDeadline, "admin-unresponsive-deadline", 0, "Watchdog: max time the Envoy admin may be unreachable (once previously LIVE) before the supervisor exits non-zero (0 = 30s default)")
	f.BoolVar(&supervisorMetricsEnabled, "metrics-enabled", true, "Enable supervisor hot-restart lifecycle metrics")
	// Push-first: OTLP export (--otlp-endpoint) is the primary metrics path. The
	// scrape endpoint stays available for collector-less setups but defaults off:
	// the supervisor shares the host netns, so a fixed port collides between the
	// surge predecessor and successor (retried, but avoidable entirely via push).
	f.StringVar(&supervisorTelemetryCfg.BindAddress, "metrics-bind-address", "", "Optional Prometheus /metrics address (e.g. :9902); empty disables the scrape endpoint in favor of OTLP push (--otlp-endpoint)")
	f.StringVar(&supervisorTelemetryCfg.OTLPEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint for metrics push (e.g. collector:4317); empty disables OTLP export")

	rootCmd.AddCommand(proxySupervisorCmd)
}
