package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/meshdns"
	meshconst "github.com/bpalermo/aether/common/constants/mesh"
	"github.com/bpalermo/aether/common/manager"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

// meshDnsReloadDebounce coalesces a flurry of fsnotify events (an atomic rename is
// several ops back-to-back) into a single snapshot reload.
const meshDnsReloadDebounce = 200 * time.Millisecond

// meshDnsCfg holds the flag-bound configuration for the mesh-dns subcommand
// (issue #578: the standalone, surge-capable mesh-DNS resolver DaemonSet).
var (
	meshDnsSnapshotPath string
	meshDnsMeshDomain   string
	meshDnsUpstream     []string
	meshDnsDebug        bool
	meshDnsOTLPEndpoint string
)

var meshDnsCmd = &cobra.Command{
	Use:   "mesh-dns",
	Short: "Runs the standalone mesh-DNS resolver (issue #578).",
	Long: "Runs as the aether-mesh-dns DaemonSet: a host-network miekg/dns resolver bound " +
		"to HOST_IP:18054 that answers <svc>.<ns>.<mesh-domain> from the snapshot file the agent " +
		"writes and forwards the rest upstream. It binds with SO_REUSEPORT so a surge rollout hands " +
		"off hitlessly, and reloads records from the snapshot file via fsnotify. No Kubernetes API " +
		"access — records come only from the file.",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runMeshDNS(cmd.Context())
	},
}

// runMeshDNS builds the resolver, wires OTel (best-effort), starts serving, and
// reloads the record table on snapshot-file changes until the context is cancelled.
func runMeshDNS(ctx context.Context) error {
	log := manager.SetupLogging(meshDnsDebug, "mesh-dns")

	hostIP := os.Getenv("HOST_IP")
	addr := fmt.Sprintf("%s:%d", hostIP, meshconst.ProxyDNSResolverPort)

	// Metrics push, best-effort (push-only OTel like the proxy-supervisor): the
	// daemon runs in the host netns with no controller-runtime manager and no scrape
	// endpoint, and a surge predecessor/successor would collide on a scrape port.
	// Set BEFORE NewServer so the global meter provider is live when meters are
	// created. Telemetry failures are never fatal — serving DNS is the job.
	shutdownTelemetry, telErr := meshdns.SetupTelemetry(ctx, meshDnsOTLPEndpoint, Version)
	if telErr != nil {
		log.Error("failed to set up mesh-DNS telemetry; continuing without metrics", "error", telErr)
		shutdownTelemetry = func() {}
	}
	defer shutdownTelemetry()

	server := meshdns.NewServerWithOptions(
		meshDnsMeshDomain, addr, meshDnsSnapshotPath, log,
		meshdns.WithReusePort(true),
	)
	upstreams := meshDnsUpstream
	if len(upstreams) == 0 {
		upstreams = meshdns.NameserversFromResolvConf("/etc/resolv.conf")
		log.Info("mesh-DNS upstream defaulted from /etc/resolv.conf", "upstreams", upstreams)
	}
	server.SetUpstreams(upstreams)

	go watchMeshDNSSnapshot(ctx, server, meshDnsSnapshotPath, log)

	return server.Start(ctx)
}

// watchMeshDNSSnapshot watches the snapshot file's PARENT directory (not the file)
// so the atomic-rename the agent uses to update records is caught — fsnotify loses
// the watch on the replaced inode, exactly as agent/storage handles it. Events on
// the snapshot file are debounced and coalesced into a single ReloadFromSnapshot.
func watchMeshDNSSnapshot(ctx context.Context, server *meshdns.Server, path string, log *slog.Logger) {
	w := newSnapshotWatcher(path, log)
	if w == nil {
		return
	}
	defer func() { _ = w.Close() }()

	deb := &debouncer{delay: meshDnsReloadDebounce}
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
			log.Error("mesh-DNS snapshot watch error", "error", err)
		}
	}
}

// newSnapshotWatcher creates an fsnotify watcher on the snapshot file's parent dir,
// or nil (logged) when the watcher can't be set up — records then only load at start.
func newSnapshotWatcher(path string, log *slog.Logger) *fsnotify.Watcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error("mesh-DNS snapshot watcher disabled; records will not reload until restart", "error", err)
		return nil
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		log.Error("failed to watch mesh-DNS snapshot dir; records will not reload until restart", "error", err, "dir", dir)
		return nil
	}
	log.Info("watching mesh-DNS snapshot for changes", "dir", dir, "path", path)
	return w
}

// isSnapshotWrite reports whether event is a create/write/rename of our snapshot
// file (an atomic rename surfaces as a Create/Rename of that name in the parent dir).
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

func init() {
	f := meshDnsCmd.Flags()
	f.StringVar(&meshDnsSnapshotPath, "snapshot-path", constants.DefaultMeshDNSSnapshotPath, "Host-persistent mesh-DNS record snapshot file the agent writes and this daemon watches")
	f.StringVar(&meshDnsMeshDomain, "mesh-domain", meshconst.DefaultMeshDomain, "DNS-style domain mesh authorities live under (clients call <service>.<mesh-domain>)")
	f.StringArrayVar(&meshDnsUpstream, "mesh-dns-upstream", nil, "Upstream resolver(s) (host[:port]) non-mesh queries are forwarded to; defaults to /etc/resolv.conf")
	f.BoolVar(&meshDnsDebug, "debug", false, "Enable debug-level logging")
	f.StringVar(&meshDnsOTLPEndpoint, "otlp-endpoint", "", "OTLP gRPC collector endpoint for mesh-DNS metrics push (e.g. collector:4317); empty disables telemetry")

	rootCmd.AddCommand(meshDnsCmd)
}
