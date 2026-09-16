// Package supervisorcmd builds the cobra command for the Envoy hot-restart
// supervisor (proposal 001).
//
// It lives here rather than in //agent/internal/cmd so that //agent/cmd/proxy-supervisor
// can be its own binary (#772 phase B2). The proxy pod stages this binary onto a
// shared volume and runs it as PID 1, and while it was a subcommand of the agent
// that meant staging and running the agent's entire 65MiB link set —
// controller-runtime, client-go, go-control-plane, SPIRE, Gateway API, miekg/dns —
// to fork a child process. The supervisor's real work is
// //agent/internal/proxy/hotrestart plus //common/readymarker: ~22 modules, no
// Kubernetes, no xDS, no SPIRE.
//
// The single line of coupling was manager.SetupLogging, and its only heavy act
// (ctrl.SetLogger, which drags in //common/manager at 85 modules) is meaningless
// in a process with no controller-runtime manager. The underlying
// log.Named(log.NewLogger(debug), name) from //common/log is what is used here,
// so the emitted log stream is byte-identical.
//
// Keep this package free of heavyweight imports:
// //agent/cmd/proxy-supervisor:deps_test and
// scripts/check-proxy-supervisor-deps.sh both fail the build if that changes.
package supervisorcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aethermesh.dev/agent/internal/proxy/hotrestart"
	"aethermesh.dev/common/file"
	"aethermesh.dev/common/log"
	"aethermesh.dev/common/readymarker"
	"github.com/spf13/cobra"
)

// readinessBinarySource is where //agent/cmd/proxy-ready lands inside the image
// this command ships in (an extra tars_layer on both
// //agent/cmd/proxy-supervisor:image and, for the deprecated `agent
// proxy-supervisor` alias, //agent/cmd/agent:image). It rides alongside the
// supervisor rather than in its own image because the only thing that ever reads
// it is the install-supervisor initContainer, which already runs this image — so
// it costs no second pull per node, and the probe and the process that writes the
// marker it reads are built from one commit.
const readinessBinarySource = "/proxy-ready"

// config is the flag-bound configuration for one supervisor command. It is built
// per New() call rather than held in package-level vars so the standalone binary
// and the agent's deprecated alias cannot share (or fight over) state.
type config struct {
	supervisor           hotrestart.Config
	telemetry            hotrestart.TelemetryConfig
	debug                bool
	installPath          string
	readinessInstallPath string
	readinessCheck       bool
	version              string
}

// New returns the `proxy-supervisor` command. version is stamped into the OTel
// service.version on the supervisor's pushed hot-restart metrics.
//
// //agent/cmd/proxy-supervisor runs it as its root command; //agent/internal/cmd
// adds it to the agent as a deprecated alias so a chart predating #772 still has
// a working initContainer against a newer agent image.
func New(version string) *cobra.Command {
	cfg := &config{version: version}

	cmd := &cobra.Command{
		Use:          "proxy-supervisor",
		Short:        "Supervises the Envoy proxy with hot-restart support.",
		Long:         "Runs as the aether-proxy container entrypoint, forking and hot-restarting Envoy across restart epochs so bootstrap-config and binary upgrades happen without dropping connections.",
		SilenceUsage: true,
		RunE:         cfg.run,
	}

	bindFlags(cmd, cfg)
	return cmd
}

// run dispatches between the initContainer staging mode, the deprecated exec
// probe and the supervisor itself.
func (c *config) run(cmd *cobra.Command, _ []string) error {
	// --install-path / --install-readiness-path let the initContainer stage
	// binaries out of this image onto a shared volume, so the runtime container
	// can be the Envoy image (which carries Envoy and its shared libraries) with
	// these injected alongside it.
	if c.installPath != "" || c.readinessInstallPath != "" {
		return runInstall(c.installPath, c.readinessInstallPath)
	}
	// --readiness-check is the legacy exec readiness probe: exit 0 iff the
	// supervisor's pod-local ready marker is present. Since #673 the chart execs
	// //agent/cmd/proxy-ready instead — a ~1.7MB stdlib-only binary whose package
	// init() is a rounding error next to this one's, when re-exec'd every 2s per
	// pod. Kept working (and only deprecated) so a chart predating #673 still has
	// a probe against a newer image.
	if c.readinessCheck {
		return readymarker.Check(c.supervisor.ReadyMarkerPath)
	}

	l := log.Named(log.NewLogger(c.debug), cmd.Name())

	// Metrics are the supervisor's crash forensics: the wedge watchdog exits
	// the process non-zero, so the deferred Shutdown flush is what gets the
	// wedge counter out before the pod is recreated. Push-only via the OTel
	// SDK (no Prometheus registry — the supervisor has no controller-runtime
	// manager and no scrape endpoint); enabled iff --otlp-endpoint is set.
	// Telemetry failures are never fatal — the supervisor's job is keeping
	// Envoy alive.
	var metrics *hotrestart.SupervisorMetrics
	if c.telemetry.OTLPEndpoint != "" {
		c.telemetry.ServiceVersion = c.version
		telemetry, telErr := hotrestart.NewTelemetry(cmd.Context(), c.telemetry)
		if telErr != nil {
			l.Error("failed to set up supervisor telemetry; continuing without metrics", "error", telErr)
		} else {
			defer func() {
				if shutdownErr := telemetry.Shutdown(); shutdownErr != nil {
					l.Error("failed to flush supervisor metrics", "error", shutdownErr)
				}
			}()
			if metrics, telErr = hotrestart.NewSupervisorMetrics(telemetry.Meter()); telErr != nil {
				l.Error("failed to create supervisor metrics; continuing without metrics", "error", telErr)
			}
		}
	}

	return hotrestart.New(c.supervisor, l, metrics).Run(cmd.Context())
}

// runInstall stages the initContainer's binaries onto the shared volume.
//
// Both destinations are optional and independent, but a requested one that
// cannot be produced is a hard error by design: a missing /proxy-ready means the
// chart is paired with an image that predates #673, and failing the initContainer
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

// bindFlags registers the supervisor's flag set. Every name and default here is
// part of the chart's contract (charts/aether/templates/agent-proxy-daemonset.yaml
// passes them literally) — changing one is a chart change.
func bindFlags(cmd *cobra.Command, c *config) {
	f := cmd.Flags()
	f.BoolVar(&c.debug, "debug", false, "Enable debug-level logging")
	f.StringVar(&c.installPath, "install-path", "", "If set, copy this binary to the given path and exit (for initContainer self-install onto a shared volume)")
	f.StringVar(&c.readinessInstallPath, "install-readiness-path", "", "If set, copy the bundled proxy-ready readiness prober ("+readinessBinarySource+" in this image) to the given path and exit; combinable with --install-path. Hard-fails when the source is absent, which is how an image predating #673 is caught")
	f.StringVar(&c.supervisor.EnvoyPath, "envoy-path", "/usr/local/bin/envoy", "Path to the Envoy binary")
	f.StringVar(&c.supervisor.ConfigPath, "config", "/etc/envoy/envoy.yaml", "Envoy bootstrap config path (-c); a change to this file triggers a hot restart when --watch-config is set")
	f.Uint32Var(&c.supervisor.BaseID, "base-id", 0, "Envoy --base-id, pinned so successive epochs share one shared-memory segment")
	f.DurationVar(&c.supervisor.DrainTime, "drain-time", 45*time.Second, "Envoy --drain-time-s: graceful connection-close window for the draining epoch")
	f.DurationVar(&c.supervisor.ParentShutdownTime, "parent-shutdown-time", 60*time.Second, "Envoy --parent-shutdown-time-s: when the previous epoch is terminated (must exceed --drain-time)")
	f.StringArrayVar(&c.supervisor.ExtraArgs, "envoy-arg", nil, "Extra argument appended to every Envoy invocation (repeatable); keep --concurrency constant across epochs")
	f.BoolVar(&c.supervisor.WatchConfig, "watch-config", true, "Watch --config and self-trigger a hot restart when the bootstrap config changes")
	f.StringVar(&c.supervisor.StateDir, "state-dir", "/run/aether/hotrestart", "Shared-hostPath dir for the per-node epoch heartbeat that drives cross-pod hot restart")
	f.StringVar(&c.supervisor.ReadyMarkerPath, "ready-marker", "/var/run/aether-proxy/ready", "Pod-local path for the readiness marker maintained while Envoy is live at the newest epoch")
	f.StringVar(&c.supervisor.AdminAddress, "admin-address", "127.0.0.1:9901", "Envoy admin host:port used for the readiness check")
	f.BoolVar(&c.readinessCheck, "readiness-check", false, "DEPRECATED (#673): exit 0 iff the --ready-marker file exists (exec readiness probe mode). Re-execing a supervisor binary every 2s per pod cost >=31% of the supervisor container's CPU in package init alone; the chart execs the stdlib-only proxy-ready prober instead. Retained so a chart predating #673 keeps a working probe against a newer image")
	// Deprecated since #673, but still functional: a chart older than #673 probes
	// with it. pflag prints "Flag --readiness-check has been deprecated, ..." on use,
	// so an operator on a stale chart is told what to move to. The error can only be
	// a missing flag name — it was registered on the line above.
	_ = f.MarkDeprecated("readiness-check", "exec the stdlib-only proxy-ready prober staged by --install-readiness-path instead (#673)")
	f.DurationVar(&c.supervisor.HandoffDeadline, "handoff-deadline", 0, "Watchdog: max time a hot-restart epoch may stay not-LIVE after launch before the supervisor exits non-zero (0 = 2m default)")
	f.DurationVar(&c.supervisor.AdminUnresponsiveDeadline, "admin-unresponsive-deadline", 0, "Watchdog: max time the Envoy admin may be unreachable (once previously LIVE) before the supervisor exits non-zero (0 = 30s default)")
	f.DurationVar(&c.supervisor.TerminationGrace, "termination-grace", 0, "This pod's terminationGracePeriodSeconds (the chart passes its own value). Bounds the mid-handoff wait for a successor so a termination that can never have one — node shutdown, scale-down, DaemonSet delete, a replacement stuck Pending — still drains Envoy before the kubelet's SIGKILL (#771). 0 = unknown: wait indefinitely")
	f.StringVar(&c.telemetry.OTLPEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint for hot-restart lifecycle metrics push (e.g. collector:4317); empty disables telemetry")
}
