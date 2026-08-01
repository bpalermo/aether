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

// watchRetryInterval is how often we re-attempt an fsnotify watch that could not be
// established (usually: the agent has not yet created the snapshot dir), and how often
// we poll ReloadFromSnapshot while blind. Short enough that a fresh cluster converges
// quickly, long enough to be free at steady state.
const watchRetryInterval = 10 * time.Second

// resolvConfPath is the node resolv.conf the default forward upstream is read from.
// The DaemonSet runs ClusterFirstWithHostNet, so this points at the cluster kube-dns.
const resolvConfPath = "/etc/resolv.conf"

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

	// Metrics + log push, best-effort (push-only OTel like the proxy-supervisor): the
	// daemon runs in the host netns with no controller-runtime manager and no scrape
	// endpoint, and a surge predecessor/successor would collide on a scrape port. Set
	// up BEFORE the logger and NewServer, so slog records reach the OTLP bridge and the
	// global meter provider is live when meters are created. Telemetry failures are
	// never fatal — serving DNS is the job.
	tel, telErr := meshdns.SetupTelemetry(ctx, otlpEndpoint, Version)
	defer tel.Shutdown()

	// Fan the daemon's slog output to stderr (kubectl logs) AND, when telemetry is up,
	// to the OTLP LoggerProvider — before #586 these logs died with the pod on every
	// roll, which mattered because some failure modes only ever show up in a log line.
	l := log.Named(log.NewLoggerWithHandler(debug, tel.LogHandler), "mesh-dns")
	if telErr != nil {
		l.Error("failed to set up mesh-DNS telemetry; continuing without metrics or log export", "error", telErr)
	}
	for _, w := range tel.Warnings {
		l.Warn("mesh-DNS telemetry degraded", "error", w)
	}

	hostIP := os.Getenv("HOST_IP")
	addr := fmt.Sprintf("%s:%d", hostIP, meshconst.ProxyDNSResolverPort)

	server := meshdns.NewServerWithOptions(
		meshDomain, addr, snapshotPath, l,
		meshdns.WithReusePort(true),
		meshdns.WithReadyMarker(readyMarker),
	)
	server.SetUpstreams(resolveUpstreams(l, resolvConfPath))

	go watchSnapshot(ctx, server, snapshotPath, l)

	return server.Start(ctx)
}

// resolveUpstreams returns the forward upstreams: the explicit --mesh-dns-upstream
// flags, else the node resolv.conf's nameservers.
//
// An empty result is a LOUD failure, not a note: the CNI DNATs every managed pod's
// :53 here, so with no upstream every non-mesh query (cluster.local AND external)
// becomes a forward_error — a full DNS outage for the node's mesh pods. Behaviour is
// unchanged (fail open: mesh names still resolve), only the reporting is: ERROR plus
// aether.mesh_dns.upstreams_configured=0, instead of an INFO line reading
// "upstreams=[]".
func resolveUpstreams(l *slog.Logger, resolvConf string) []string {
	if len(upstreams) > 0 {
		return upstreams
	}
	up := meshdns.NameserversFromResolvConf(resolvConf)
	if len(up) == 0 {
		l.Error("mesh-DNS has NO upstream resolver: no --mesh-dns-upstream given and no nameserver could be read; every non-mesh query (cluster.local and external) will fail until this is fixed",
			"resolvConf", resolvConf)
		return nil
	}
	l.Info("mesh-DNS upstream defaulted from resolv.conf", "resolvConf", resolvConf, "upstreams", up)
	return up
}

// watchSnapshot watches the snapshot file's PARENT directory (not the file) so the
// atomic-rename the agent uses to update records is caught — fsnotify loses the watch
// on the replaced inode, exactly as agent/storage handles it. Events on the snapshot
// file are debounced and coalesced into a single ReloadFromSnapshot.
// It RETRIES establishing the watch instead of giving up for the process lifetime.
// The snapshot dir is created by the AGENT (this daemon mounts the volume read-only),
// so on a fresh cluster the dir legitimately does not exist yet — treating that as
// permanent left the daemon Ready but record-less forever (#589). While the watch is
// down we still poll ReloadFromSnapshot, so records land even if fsnotify never works.
func watchSnapshot(ctx context.Context, server *meshdns.Server, path string, l *slog.Logger) {
	for ctx.Err() == nil {
		if w := newSnapshotWatcher(path, l); w != nil {
			server.SetWatchActive(true)
			// The agent may have written between our last reload and the watch being
			// established; reload once so that window is never missed.
			server.ReloadFromSnapshot()
			runWatchLoop(ctx, w, server, path, l)
			server.SetWatchActive(false)
			_ = w.Close()
		} else {
			// Not watching yet: pick up records by polling until the dir appears.
			server.ReloadFromSnapshot()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchRetryInterval):
		}
	}
}

// runWatchLoop services one established watcher until the context ends or the watcher
// closes its channels (after which watchSnapshot re-establishes it).
func runWatchLoop(ctx context.Context, w *fsnotify.Watcher, server *meshdns.Server, path string, l *slog.Logger) {
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

// newSnapshotWatcher creates an fsnotify watcher on the snapshot file's parent dir, or
// nil (logged) when it can't be set up yet. The caller RETRIES: a missing dir just
// means the agent has not written its first snapshot, which is normal on a fresh
// cluster, so this is warned rather than errored.
func newSnapshotWatcher(path string, l *slog.Logger) *fsnotify.Watcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		l.Warn("mesh-DNS snapshot watcher unavailable; retrying", "error", err, "retryIn", watchRetryInterval)
		return nil
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		l.Warn("mesh-DNS snapshot dir not watchable yet (the agent creates it on its first write); retrying",
			"error", err, "dir", dir, "retryIn", watchRetryInterval)
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
