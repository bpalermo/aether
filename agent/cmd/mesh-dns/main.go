// Command mesh-dns is the standalone, slim mesh-DNS resolver binary run by the
// aether-mesh-dns DaemonSet (issues #578, #583).
//
// It is intentionally self-contained: it imports ONLY the meshdns resolver and its
// direct deps (fsnotify, OTel push telemetry, cobra), and deliberately does NOT pull
// in the full agent (controller-runtime, go-control-plane/xDS, CNI, SPIRE). That
// keeps the binary — and its re-exec cost for the pod-local readiness probe — tiny,
// which is the entire point of #583 (and the CPU-reclaim half of #582): the readiness
// probe re-execs THIS binary to stat a pod-local marker, and a slim binary makes that
// cheap. The in-process httpGet probe #582 proposed is unsafe on a host-network +
// maxSurge DaemonSet (two pods share the host netns), so the provably-pod-local
// ready-marker exec probe from #580 is kept — this binary just makes it cheap.
//
// It has no Kubernetes API access — records come only from the snapshot file the
// agent writes and this daemon watches via fsnotify.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/meshdns"
	meshconst "github.com/bpalermo/aether/common/constants/mesh"
	"github.com/bpalermo/aether/common/log"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags (Bazel x_defs).
var Version = "dev"

// reloadDebounce coalesces a flurry of fsnotify events (an atomic rename is several
// ops back-to-back) into a single snapshot reload.
const reloadDebounce = 200 * time.Millisecond

// cfg holds the flag-bound configuration for the standalone mesh-DNS resolver.
var (
	snapshotPath   string
	meshDomain     string
	upstreams      []string
	debug          bool
	otlpEndpoint   string
	readyMarker    string
	readinessCheck bool
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// rootCmd builds the cobra command that wires the flags and dispatches to either the
// cheap --readiness-check probe branch or the resolver run loop.
func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh-dns",
		Short: "Runs the standalone mesh-DNS resolver (issues #578, #583).",
		Long: "Runs as the aether-mesh-dns DaemonSet: a host-network miekg/dns resolver bound " +
			"to HOST_IP:18054 that answers <svc>.<ns>.<mesh-domain> from the snapshot file the agent " +
			"writes and forwards the rest upstream. It binds with SO_REUSEPORT so a surge rollout hands " +
			"off hitlessly, and reloads records from the snapshot file via fsnotify. No Kubernetes API " +
			"access — records come only from the file.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --readiness-check is the exec readiness probe: exit 0 iff this pod's
			// pod-local ready marker is present (the distroless image has no shell or
			// cat, so the probe re-execs this slim binary — cheap because it's slim).
			// The marker is written only once THIS process has bound its listeners, so
			// the surge rollout keeps the predecessor until the successor truly serves.
			if readinessCheck {
				if _, err := os.Stat(readyMarker); err != nil {
					return fmt.Errorf("not ready: %w", err)
				}
				return nil
			}
			return run(cmd.Context())
		},
	}

	f := cmd.Flags()
	f.StringVar(&snapshotPath, "snapshot-path", constants.DefaultMeshDNSSnapshotPath, "Host-persistent mesh-DNS record snapshot file the agent writes and this daemon watches")
	f.StringVar(&meshDomain, "mesh-domain", meshconst.DefaultMeshDomain, "DNS-style domain mesh authorities live under (clients call <service>.<mesh-domain>)")
	f.StringArrayVar(&upstreams, "mesh-dns-upstream", nil, "Upstream resolver(s) (host[:port]) non-mesh queries are forwarded to; defaults to /etc/resolv.conf")
	f.BoolVar(&debug, "debug", false, "Enable debug-level logging")
	f.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint for mesh-DNS metrics push (e.g. collector:4317); empty disables telemetry")
	f.StringVar(&readyMarker, "ready-marker", "/run/aether/mesh-dns.ready", "Pod-local path for the readiness marker written once the resolver's listeners are bound")
	f.BoolVar(&readinessCheck, "readiness-check", false, "Exit 0 iff the --ready-marker file exists (exec readiness probe mode)")

	return cmd
}

// run builds the resolver, wires OTel (best-effort), starts serving, and reloads the
// record table on snapshot-file changes until the process is signalled to stop.
func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	l := log.Named(log.NewLogger(debug), "mesh-dns")

	hostIP := os.Getenv("HOST_IP")
	addr := fmt.Sprintf("%s:%d", hostIP, meshconst.ProxyDNSResolverPort)

	// Metrics push, best-effort (push-only OTel like the proxy-supervisor): the daemon
	// runs in the host netns with no controller-runtime manager and no scrape endpoint,
	// and a surge predecessor/successor would collide on a scrape port. Set BEFORE
	// NewServer so the global meter provider is live when meters are created.
	// Telemetry failures are never fatal — serving DNS is the job.
	shutdownTelemetry, telErr := meshdns.SetupTelemetry(ctx, otlpEndpoint, Version)
	if telErr != nil {
		l.Error("failed to set up mesh-DNS telemetry; continuing without metrics", "error", telErr)
		shutdownTelemetry = func() {}
	}
	defer shutdownTelemetry()

	server := meshdns.NewServerWithOptions(
		meshDomain, addr, snapshotPath, l,
		meshdns.WithReusePort(true),
		meshdns.WithReadyMarker(readyMarker),
	)
	up := upstreams
	if len(up) == 0 {
		up = meshdns.NameserversFromResolvConf("/etc/resolv.conf")
		l.Info("mesh-DNS upstream defaulted from /etc/resolv.conf", "upstreams", up)
	}
	server.SetUpstreams(up)

	go watchSnapshot(ctx, server, snapshotPath, l)

	return server.Start(ctx)
}

// watchSnapshot watches the snapshot file's PARENT directory (not the file) so the
// atomic-rename the agent uses to update records is caught — fsnotify loses the watch
// on the replaced inode, exactly as agent/storage handles it. Events on the snapshot
// file are debounced and coalesced into a single ReloadFromSnapshot.
func watchSnapshot(ctx context.Context, server *meshdns.Server, path string, l *slog.Logger) {
	w := newSnapshotWatcher(path, l)
	if w == nil {
		return
	}
	defer func() { _ = w.Close() }()

	deb := &debouncer{delay: reloadDebounce}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if isSnapshotWrite(event, path) {
				deb.arm()
			}
		case <-deb.fireC():
			deb.disarm()
			server.ReloadFromSnapshot()
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			l.Error("mesh-DNS snapshot watch error", "error", err)
		}
	}
}

// newSnapshotWatcher creates an fsnotify watcher on the snapshot file's parent dir,
// or nil (logged) when the watcher can't be set up — records then only load at start.
func newSnapshotWatcher(path string, l *slog.Logger) *fsnotify.Watcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		l.Error("mesh-DNS snapshot watcher disabled; records will not reload until restart", "error", err)
		return nil
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		l.Error("failed to watch mesh-DNS snapshot dir; records will not reload until restart", "error", err, "dir", dir)
		return nil
	}
	l.Info("watching mesh-DNS snapshot for changes", "dir", dir, "path", path)
	return w
}

// isSnapshotWrite reports whether event is a create/write/rename of our snapshot file
// (an atomic rename surfaces as a Create/Rename of that name in the parent dir).
func isSnapshotWrite(event fsnotify.Event, path string) bool {
	if filepath.Clean(event.Name) != filepath.Clean(path) {
		return false
	}
	return event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0
}

// debouncer coalesces a flurry of events into a single fire after a quiet delay.
type debouncer struct {
	delay time.Duration
	timer *time.Timer
	c     <-chan time.Time
}

// arm (re)starts the quiet-period timer.
func (d *debouncer) arm() {
	if d.timer == nil {
		d.timer = time.NewTimer(d.delay)
	} else {
		d.timer.Reset(d.delay)
	}
	d.c = d.timer.C
}

// disarm clears the active fire channel after the timer has fired.
func (d *debouncer) disarm() { d.c = nil }

// fireC returns the fire channel (nil until armed, so the select case is inert).
func (d *debouncer) fireC() <-chan time.Time { return d.c }
