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
	"github.com/spf13/cobra"
)

// readinessBinarySource is where //agent/cmd/proxy-ready lands inside the image
// this command ships in (an extra tars_layer on
// //agent/cmd/proxy-supervisor:image). It rides alongside the
// supervisor rather than in its own image because the only thing that ever reads
// it is the install-supervisor initContainer, which already runs this image — so
// it costs no second pull per node, and the probe and the process that writes the
// marker it reads are built from one commit.
const readinessBinarySource = "/proxy-ready"

// config is the flag-bound configuration for one supervisor command. It is built
// per New() call rather than held in package-level vars, so tests can build
// independent commands without sharing (or fighting over) state.
type config struct {
	supervisor           hotrestart.Config
	telemetry            hotrestart.TelemetryConfig
	debug                bool
	installPath          string
	readinessInstallPath string
	version              string
}

// New returns the `proxy-supervisor` command. version is stamped into the OTel
// service.version on the supervisor's pushed hot-restart metrics.
//
// //agent/cmd/proxy-supervisor runs it as its root command. It was also an
// `agent proxy-supervisor` alias until that was removed after #772.
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

// run dispatches between the initContainer staging mode and the supervisor
// itself.
func (c *config) run(cmd *cobra.Command, _ []string) error {
	// --install-path / --install-readiness-path let the initContainer stage
	// binaries out of this image onto a shared volume, so the runtime container
	// can be the Envoy image (which carries Envoy and its shared libraries) with
	// these injected alongside it.
	if c.installPath != "" || c.readinessInstallPath != "" {
		return runInstall(c.installPath, c.readinessInstallPath)
	}
	// The readiness probe is not this binary: the chart execs the stdlib-only
	// //agent/cmd/proxy-ready (#673) staged by --install-readiness-path, whose
	// package init() is a rounding error next to this one's when re-exec'd every
	// 2s per pod. The old --readiness-check exec-probe mode was removed.

	l := log.Named(log.NewLogger(c.debug), cmd.Name())

	// The supervisor passes a handful of Envoy flags itself: the bootstrap
	// path, --base-id, --restart-epoch, the two hot-restart timers, and
	// --admin-address-path (its admin identity, which keeps a state-changing
	// admin request off another proxy pod's Envoy, #1127). Envoy rejects a
	// flag given twice, so an --envoy-arg that repeats one would fail every
	// fork. Fail once, here, with an error that names it (#1376). The same
	// check refuses the other arguments the pinned Envoy rejects at every
	// fork, or accepts and then cannot hand off with (#1407, #1408, #1409).
	if err := checkEnvoyArgs(c.supervisor.ExtraArgs); err != nil {
		return err
	}
	// POD_NAME is the downward-API env the chart already sets on the proxy
	// container; it only labels the identity for logs.
	c.supervisor.PodName = os.Getenv("POD_NAME")

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

// checkEnvoyArgs refuses an --envoy-arg that would make Envoy reject its
// command line on every fork (a flag the supervisor passes itself, a repeated
// chart flag, a --flag=value spelling, a --concurrency without a usable value)
// or start and then fail a handoff (--use-dynamic-base-id and its like). The
// lists live with the code that builds the command line
// (hotrestart.CheckExtraArgs), so the two cannot drift.
func checkEnvoyArgs(args []string) error {
	return hotrestart.CheckExtraArgs(args)
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
	f.StringArrayVar(&c.supervisor.ExtraArgs, "envoy-arg", nil, "Extra argument appended to every Envoy invocation (repeatable). A flag and its value are two items (--envoy-arg=--concurrency --envoy-arg=2): the pinned Envoy does not parse --flag=value, -f=value or -fvalue. Refused at startup: that spelling; an Envoy flag the supervisor passes itself (-c/--config-path, --base-id, --restart-epoch, --drain-time-s, --parent-shutdown-time-s, --admin-address-path, --mode), because Envoy rejects a flag given twice; a flag that breaks a handoff or stops Envoy from serving (--use-dynamic-base-id, --disable-hot-restart, --socket-path, --hot-restart-version, --version, -h/--help, --, --ignore_rest); a second --concurrency, -l/--log-level, --service-cluster, --service-node, --service-zone, --drain-strategy or --skip-hot-restart-parent-stats; and a --concurrency whose value is missing or is not a whole number in digits (0 means one worker). A --concurrency that differs from a live predecessor's is a drain + fresh start, not a hot restart (see --hot-restart-on-concurrency-change)")
	f.BoolVar(&c.supervisor.WatchConfig, "watch-config", true, "Watch --config and self-trigger a hot restart when the bootstrap config changes")
	f.StringVar(&c.supervisor.StateDir, "state-dir", "/run/aether/hotrestart", "Shared-hostPath dir for the per-node epoch heartbeat that drives cross-pod hot restart")
	f.StringVar(&c.supervisor.ReadyMarkerPath, "ready-marker", "/var/run/aether-proxy/ready", "Pod-local path for the readiness marker maintained while Envoy is live at the newest epoch")
	f.StringVar(&c.supervisor.AdminAddress, "admin-address", "127.0.0.1:9901", "Envoy admin host:port used for the readiness check")
	f.DurationVar(&c.supervisor.HandoffDeadline, "handoff-deadline", 0, "Watchdog: max time a hot-restart epoch may stay not-LIVE with its admin silent (counted from launch, or from its last admin answer at its own epoch: a successor still answering is waiting on xDS, not wedged, #1085) before the supervisor exits non-zero (0 = 2m default)")
	f.DurationVar(&c.supervisor.AdminUnresponsiveDeadline, "admin-unresponsive-deadline", 0, "Watchdog: max time the Envoy admin may be unreachable (once previously LIVE) before the supervisor exits non-zero (0 = 30s default)")
	f.DurationVar(&c.supervisor.TerminationGrace, "termination-grace", 0, "This pod's terminationGracePeriodSeconds (the chart passes its own value). Bounds the mid-handoff wait for a successor so a termination that can never have one — node shutdown, scale-down, DaemonSet delete, a replacement stuck Pending — still drains Envoy before the kubelet's SIGKILL (#771). 0 = unknown: wait indefinitely")
	f.BoolVar(&c.supervisor.ShutdownDrainImmediately, "shutdown-drain-immediately", false, "On SIGTERM, drain Envoy's listeners straight away instead of first waiting (bounded by --termination-grace) for a surge replacement to hot-restart us. Escape hatch for deployments where no replacement can overlap this pod; leaving it false is what keeps pod delete, node drain, eviction and preemption hitless (#795)")
	f.BoolVar(&c.supervisor.HotRestartOnConcurrencyChange, "hot-restart-on-concurrency-change", false, "Hot-restart from a live predecessor even when its Envoy worker count differs from the --concurrency this supervisor's Envoy will run with. By default such a predecessor is drained (graceful drain over --drain-time, then stopped) and a fresh Envoy started at epoch 0, because a hot restart across a worker-count change re-steers the predecessor's QUIC connections by the new count and resets about half of them (#1136)")
	f.DurationVar(&c.supervisor.StallSampleInterval, "stall-sample-interval", hotrestart.DefaultStallSampleInterval, "Envoy thread-stall sampler interval (#1093): reads each Envoy main/worker thread's schedstat and wchan from /proc and logs 'envoy thread stall' when one is starved of CPU, blocked or busy for --stall-threshold within a second, with the node's CPU/softirq/PSI picture for that second; 0 disables")
	f.DurationVar(&c.supervisor.StallThreshold, "stall-threshold", hotrestart.DefaultStallThreshold, "Per-second starved/blocked/busy time that makes the thread-stall sampler report an Envoy thread (default: Envoy's worker watchdog miss threshold)")
	f.StringVar(&c.telemetry.OTLPEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint for hot-restart lifecycle metrics push (e.g. collector:4317); empty disables telemetry")
}
