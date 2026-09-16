package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aethermesh.dev/agent/internal/proxy/hotrestart"
	"aethermesh.dev/common/file"
	"aethermesh.dev/common/manager"
	"aethermesh.dev/common/readymarker"
	"github.com/spf13/cobra"
)

// readinessBinarySource is where //agent/cmd/proxy-ready lands inside the agent
// image (an extra tars_layer on agent_image). It rides in this image rather than
// its own because the only thing that ever reads it is the install-supervisor
// initContainer, which already runs this image — so shipping it here costs no
// second pull per node.
const readinessBinarySource = "/proxy-ready"

// supervisorCfg holds the flag-bound configuration for the proxy-supervisor
// subcommand. SPIKE: see docs/proposals/001_proxy-hot-restart.md.
var (
	supervisorCfg                  hotrestart.Config
	supervisorTelemetryCfg         hotrestart.TelemetryConfig
	supervisorDebug                bool
	supervisorInstallPath          string
	supervisorReadinessInstallPath string
	supervisorReadinessCheck       bool
)

var proxySupervisorCmd = &cobra.Command{
	Use:          "proxy-supervisor",
	Short:        "Supervises the Envoy proxy with hot-restart support (SPIKE).",
	Long:         "Runs as the aether-proxy container entrypoint, forking and hot-restarting Envoy across restart epochs so bootstrap-config and binary upgrades happen without dropping connections.",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// --install-path / --install-readiness-path let the initContainer stage
		// binaries out of the agent image onto a shared volume, so the runtime
		// container can be the Envoy image (which carries Envoy and its shared
		// libraries) with these injected alongside it.
		if supervisorInstallPath != "" || supervisorReadinessInstallPath != "" {
			return runInstall(supervisorInstallPath, supervisorReadinessInstallPath)
		}
		// --readiness-check is the legacy exec readiness probe: exit 0 iff the
		// supervisor's pod-local ready marker is present. Since #673 the chart execs
		// //agent/cmd/proxy-ready instead — a ~1.7MB stdlib-only binary rather than
		// this 67MB one, whose package init() dominated the supervisor container's
		// CPU when re-exec'd every 2s. Kept working (and only deprecated) so a chart
		// predating #673 still has a probe against a newer image.
		if supervisorReadinessCheck {
			return readymarker.Check(supervisorCfg.ReadyMarkerPath)
		}
		log := manager.SetupLogging(supervisorDebug, cmd.Name())

		// Metrics are the supervisor's crash forensics: the wedge watchdog exits
		// the process non-zero, so the deferred Shutdown flush is what gets the
		// wedge counter out before the pod is recreated. Push-only via the OTel
		// SDK (no Prometheus registry — the supervisor has no controller-runtime
		// manager and no scrape endpoint); enabled iff --otlp-endpoint is set.
		// Telemetry failures are never fatal — the supervisor's job is keeping
		// Envoy alive.
		var metrics *hotrestart.SupervisorMetrics
		if supervisorTelemetryCfg.OTLPEndpoint != "" {
			supervisorTelemetryCfg.ServiceVersion = Version
			telemetry, telErr := hotrestart.NewTelemetry(cmd.Context(), supervisorTelemetryCfg)
			if telErr != nil {
				log.Error("failed to set up supervisor telemetry; continuing without metrics", "error", telErr)
			} else {
				defer func() {
					if shutdownErr := telemetry.Shutdown(); shutdownErr != nil {
						log.Error("failed to flush supervisor metrics", "error", shutdownErr)
					}
				}()
				if metrics, telErr = hotrestart.NewSupervisorMetrics(telemetry.Meter()); telErr != nil {
					log.Error("failed to create supervisor metrics; continuing without metrics", "error", telErr)
				}
			}
		}

		return hotrestart.New(supervisorCfg, log, metrics).Run(cmd.Context())
	},
}

// runInstall stages the initContainer's binaries onto the shared volume.
//
// Both destinations are optional and independent, but a requested one that
// cannot be produced is a hard error by design: a missing /proxy-ready means the
// chart is paired with a pre-#673 agent image, and failing the initContainer
// surfaces that skew immediately (CrashLoopBackOff, maxUnavailable:0 keeps the
// old pods serving) instead of starting a pod whose readiness probe can never
// succeed.
func runInstall(supervisorDest, readinessDest string) error {
	if supervisorDest != "" {
		// The supervisor is this (statically linked) binary, self-copied.
		if err := installFile("/proc/self/exe", supervisorDest); err != nil {
			return err
		}
	}
	if readinessDest != "" {
		if err := installFile(readinessBinarySource, readinessDest); err != nil {
			return err
		}
	}
	return nil
}

// installFile copies source to dest (0o755) atomically. Linux-only when source
// is /proc/self/exe; the supervisor only ever runs on Linux nodes.
//
// It goes through common/file rather than a hand-rolled copy. What it publishes
// is an EXECUTABLE that another container then runs as its entrypoint, and the
// hand-rolled version did the two things an atomic write must not do: it named
// its temporary file `dest + ".tmp"`, a fixed path any concurrent writer in the
// same directory would collide on, and it renamed without ever fsyncing, so the
// directory entry could outlive the data it points at across a node crash —
// leaving a truncated supervisor binary for the proxy container to exec.
// common/file uses os.CreateTemp for a unique name and flushes before the
// rename.
func installFile(source, dest string) error {
	src, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("opening %s: %w", source, err)
	}
	defer func() { _ = src.Close() }()

	if err := file.AtomicWriteReader(filepath.Clean(dest), src, 0o755); err != nil {
		return fmt.Errorf("installing to %s: %w", dest, err)
	}
	return nil
}

func init() {
	f := proxySupervisorCmd.Flags()
	f.BoolVar(&supervisorDebug, "debug", false, "Enable debug-level logging")
	f.StringVar(&supervisorInstallPath, "install-path", "", "If set, copy this binary to the given path and exit (for initContainer self-install onto a shared volume)")
	f.StringVar(&supervisorReadinessInstallPath, "install-readiness-path", "", "If set, copy the bundled proxy-ready readiness prober ("+readinessBinarySource+" in the agent image) to the given path and exit; combinable with --install-path. Hard-fails when the source is absent, which is how an agent image predating #673 is caught")
	f.StringVar(&supervisorCfg.EnvoyPath, "envoy-path", "/usr/local/bin/envoy", "Path to the Envoy binary")
	f.StringVar(&supervisorCfg.ConfigPath, "config", "/etc/envoy/envoy.yaml", "Envoy bootstrap config path (-c); a change to this file triggers a hot restart when --watch-config is set")
	f.Uint32Var(&supervisorCfg.BaseID, "base-id", 0, "Envoy --base-id, pinned so successive epochs share one shared-memory segment")
	f.DurationVar(&supervisorCfg.DrainTime, "drain-time", 45*time.Second, "Envoy --drain-time-s: graceful connection-close window for the draining epoch")
	f.DurationVar(&supervisorCfg.ParentShutdownTime, "parent-shutdown-time", 60*time.Second, "Envoy --parent-shutdown-time-s: when the previous epoch is terminated (must exceed --drain-time)")
	f.StringArrayVar(&supervisorCfg.ExtraArgs, "envoy-arg", nil, "Extra argument appended to every Envoy invocation (repeatable); keep --concurrency constant across epochs")
	f.BoolVar(&supervisorCfg.WatchConfig, "watch-config", true, "Watch --config and self-trigger a hot restart when the bootstrap config changes")
	f.StringVar(&supervisorCfg.StateDir, "state-dir", "/run/aether/hotrestart", "Shared-hostPath dir for the per-node epoch heartbeat that drives cross-pod hot restart")
	f.StringVar(&supervisorCfg.ReadyMarkerPath, "ready-marker", "/var/run/aether-proxy/ready", "Pod-local path for the readiness marker maintained while Envoy is live at the newest epoch")
	f.StringVar(&supervisorCfg.AdminAddress, "admin-address", "127.0.0.1:9901", "Envoy admin host:port used for the readiness check")
	f.BoolVar(&supervisorReadinessCheck, "readiness-check", false, "DEPRECATED (#673): exit 0 iff the --ready-marker file exists (exec readiness probe mode). Re-execing this 67MB binary every 2s cost >=31% of the supervisor container's CPU in package init alone; the chart execs the stdlib-only proxy-ready prober instead. Retained so a chart predating #673 keeps a working probe against a newer image")
	// Deprecated since #673, but still functional: a chart older than #673 probes
	// with it. pflag prints "Flag --readiness-check has been deprecated, ..." on use,
	// so an operator on a stale chart is told what to move to. The error can only be
	// a missing flag name — it was registered on the line above.
	_ = f.MarkDeprecated("readiness-check", "exec the stdlib-only proxy-ready prober staged by --install-readiness-path instead (#673)")
	f.DurationVar(&supervisorCfg.HandoffDeadline, "handoff-deadline", 0, "Watchdog: max time a hot-restart epoch may stay not-LIVE after launch before the supervisor exits non-zero (0 = 2m default)")
	f.DurationVar(&supervisorCfg.AdminUnresponsiveDeadline, "admin-unresponsive-deadline", 0, "Watchdog: max time the Envoy admin may be unreachable (once previously LIVE) before the supervisor exits non-zero (0 = 30s default)")
	f.DurationVar(&supervisorCfg.TerminationGrace, "termination-grace", 0, "This pod's terminationGracePeriodSeconds (the chart passes its own value). Bounds the mid-handoff wait for a successor so a termination that can never have one — node shutdown, scale-down, DaemonSet delete, a replacement stuck Pending — still drains Envoy before the kubelet's SIGKILL (#771). 0 = unknown: wait indefinitely")
	f.StringVar(&supervisorTelemetryCfg.OTLPEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint for hot-restart lifecycle metrics push (e.g. collector:4317); empty disables telemetry")

	rootCmd.AddCommand(proxySupervisorCmd)
}
