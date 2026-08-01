package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// defaultRefreshDebounce is how long the refresher waits for change signals to
// settle before rebuilding the cluster snapshot, collapsing bursts (e.g. a full
// snapshot replay) into a single reload.
const defaultRefreshDebounce = 250 * time.Millisecond

// observedPruneInterval is how often time-driven expiry runs: ODCDS-observed
// dependencies against their idle TTL, and retained absent services against
// the retention grace (worst-case stale-retention window = grace + this).
const observedPruneInterval = time.Minute

// RegistryRefresher is a controller-runtime runnable that rebuilds the xDS
// cluster/endpoint/route snapshot whenever the registry reports endpoint
// changes. It bridges a registry.ChangeNotifier to the snapshot cache's
// LoadClustersFromRegistry, so services registered after the agent started
// become routable without restarting the agent.
//
// If the registry does not implement registry.ChangeNotifier, the refresher is
// a no-op for the lifetime of the process (the initial snapshot built during
// the xDS server's PreListen still applies).
type RegistryRefresher struct {
	log      *slog.Logger
	cache    *cache.SnapshotCache
	registry registry.Registry

	clusterName string
	nodeName    string
	debounce    time.Duration

	// coalesced counts change signals absorbed into an already-armed debounce
	// window; refreshErrors counts failed reloads, each of which leaves the
	// cluster snapshot stale until the next change signal (errors are not
	// retried). Either may be nil (instrumentation disabled).
	coalesced     metric.Int64Counter
	refreshErrors metric.Int64Counter
}

// AssertWatchFilter pushes the cache's current dependency set to the registry
// as the watch service filter (registry.WatchScoper capability), so the
// registrar fans out endpoint changes for this node's dependencies only.
// No-op for registries without watch scoping or when the filter is unchanged.
func AssertWatchFilter(c *cache.SnapshotCache, reg registry.Registry) {
	scoper, ok := reg.(registry.WatchScoper)
	if !ok {
		return
	}
	deps := c.DependencySet()
	services := make([]string, 0, len(deps))
	for svc := range deps {
		services = append(services, svc)
	}
	scoper.SetServiceFilter(services)
}

// NeedLeaderElection returns false so the refresher runs on EVERY replica, not
// just the leader. Each edge/agent pod feeds its own co-located Envoy via the
// snapshot cache; leader-gating this runnable would starve all non-leader
// proxies of cluster/endpoint updates and break data-plane HA. The reconciler
// (status writer) is the only component that must be a singleton — it stays
// leader-election-aware via the controller-runtime builder. On the node agent
// (leader election off) this method is a no-op.
func (r *RegistryRefresher) NeedLeaderElection() bool { return false }

// NewRegistryRefresher creates a RegistryRefresher.
func NewRegistryRefresher(clusterName, nodeName string, snapshotCache *cache.SnapshotCache, reg registry.Registry, log *slog.Logger) *RegistryRefresher {
	r := &RegistryRefresher{
		log:         commonlog.Named(log, "registry-refresher"),
		cache:       snapshotCache,
		registry:    reg,
		clusterName: clusterName,
		nodeName:    nodeName,
		debounce:    defaultRefreshDebounce,
	}

	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the refresher.
	meter := otel.Meter("aether/agent-xds-refresher")
	var err error
	if r.coalesced, err = meter.Int64Counter("aether.agent.refresher.coalesced",
		metric.WithDescription("Registry change signals absorbed into an already-armed debounce window")); err != nil {
		r.log.Error("failed to create coalesced counter; continuing without instrumentation", "error", err)
	}
	if r.refreshErrors, err = meter.Int64Counter("aether.agent.refresher.errors",
		metric.WithDescription("Failed cluster reloads from the registry (snapshot left stale until the next change signal)")); err != nil {
		r.log.Error("failed to create refresh errors counter; continuing without instrumentation", "error", err)
	}

	return r
}

// Start blocks until ctx is cancelled, rebuilding the cluster snapshot from the
// registry on each (debounced) change notification — registry endpoint changes
// and node dependency-set changes (pod add/remove or changed declared
// upstreams) both funnel into the same debounced reload. It implements
// controller-runtime's manager.Runnable.
func (r *RegistryRefresher) Start(ctx context.Context) error {
	depChanges := r.cache.DependencyChanges()

	notifier, ok := r.registry.(registry.ChangeNotifier)
	var changes <-chan struct{}
	if ok {
		changes = notifier.Changes()
	} else {
		r.log.InfoContext(ctx, "registry does not support change notifications; clusters refresh on dependency-set changes only")
	}

	// Debounce timer, created stopped: it is (re)armed on each change signal and
	// fires once the signals settle, triggering a single reload.
	timer := time.NewTimer(r.debounce)
	if !timer.Stop() {
		<-timer.C
	}

	// Periodically expire ODCDS-observed dependencies idle past their TTL;
	// an expiry signals a dependency change, funneling into the same reload.
	pruneTicker := time.NewTicker(observedPruneInterval)
	defer pruneTicker.Stop()

	r.log.DebugContext(ctx, "watching registry for endpoint changes")

	loop := &refreshLoop{r: r, timer: timer}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			loop.arm(ctx)
		case <-depChanges:
			loop.onDependencyChange(ctx)
		case <-pruneTicker.C:
			// Each signals a dependency change (handled above) when anything
			// expired: observed (ODCDS) entries past their idle TTL, and
			// retained absent services past the retention grace — the latter
			// receive no watch events in steady state under scoped watches,
			// so expiry must be time-driven, not event-driven.
			r.cache.PruneObservedDependencies()
			r.cache.SignalIfRetentionExpired()
		case <-timer.C:
			loop.reload(ctx)
		}
	}
}

// refreshLoop carries the debounce state shared between Start's select loop
// and its helpers.
type refreshLoop struct {
	r          *RegistryRefresher
	timer      *time.Timer
	lastReload time.Time
}

// arm (re)arms the debounce window. Stop+drain before Reset so a prior
// expiry doesn't leave a stale tick queued. Stop reporting true means the
// timer was already armed — this signal coalesced.
func (l *refreshLoop) arm(ctx context.Context) {
	if l.timer.Stop() {
		if l.r.coalesced != nil {
			l.r.coalesced.Add(ctx, 1)
		}
	} else {
		select {
		case <-l.timer.C:
		default:
		}
	}
	l.timer.Reset(l.r.debounce)
}

// reload rebuilds the scoped snapshot now (shared by the debounce expiry
// and the dependency-change leading edge).
func (l *refreshLoop) reload(ctx context.Context) {
	l.lastReload = time.Now()
	// Re-assert the watch filter from the current dependency set
	// before reloading: a grown set must reach the registrar so the
	// watch cache receives the newly in-scope services (no-op when
	// unchanged; the resulting watch events trigger the next reload).
	AssertWatchFilter(l.r.cache, l.r.registry)
	if err := l.r.cache.LoadClustersFromRegistry(ctx, l.r.clusterName, l.r.nodeName, l.r.registry); err != nil {
		l.r.log.ErrorContext(ctx, "failed to refresh clusters from registry", "error", err)
		if l.r.refreshErrors != nil {
			l.r.refreshErrors.Add(ctx, 1)
		}
		return
	}
	l.r.log.DebugContext(ctx, "refreshed clusters from registry")
}

// onDependencyChange handles a node dependency-set change (pod add/remove,
// changed declared upstreams, or an ODCDS observation). Unlike registry
// event bursts, this is a single latency-critical event — an ODCDS
// observation has a PAUSED REQUEST behind it — so it fires the reload
// immediately on the leading edge; only when reloads are already hot (within
// one debounce window) does it fall back to the trailing debounce to
// coalesce storms.
func (l *refreshLoop) onDependencyChange(ctx context.Context) {
	if time.Since(l.lastReload) >= l.r.debounce {
		l.reload(ctx)
	} else {
		l.arm(ctx)
	}
}
