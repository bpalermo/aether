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

	// arm (re)arms the debounce window. Stop+drain before Reset so a prior
	// expiry doesn't leave a stale tick queued. Stop reporting true means the
	// timer was already armed — this signal coalesced.
	arm := func() {
		if timer.Stop() {
			if r.coalesced != nil {
				r.coalesced.Add(ctx, 1)
			}
		} else {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(r.debounce)
	}

	// reload rebuilds the scoped snapshot now (shared by the debounce expiry
	// and the dependency-change leading edge).
	var lastReload time.Time
	reload := func() {
		lastReload = time.Now()
		// Re-assert the watch filter from the current dependency set
		// before reloading: a grown set must reach the registrar so the
		// watch cache receives the newly in-scope services (no-op when
		// unchanged; the resulting watch events trigger the next reload).
		AssertWatchFilter(r.cache, r.registry)
		if err := r.cache.LoadClustersFromRegistry(ctx, r.clusterName, r.nodeName, r.registry); err != nil {
			r.log.ErrorContext(ctx, "failed to refresh clusters from registry", "error", err)
			if r.refreshErrors != nil {
				r.refreshErrors.Add(ctx, 1)
			}
			return
		}
		r.log.DebugContext(ctx, "refreshed clusters from registry")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			arm()
		case <-depChanges:
			// The node dependency set changed (pod add/remove, changed
			// declared upstreams, or an ODCDS observation). Unlike registry
			// event bursts, this is a single latency-critical event — an
			// ODCDS observation has a PAUSED REQUEST behind it — so it fires
			// the reload immediately on the leading edge; only when reloads
			// are already hot (within one debounce window) does it fall back
			// to the trailing debounce to coalesce storms.
			if time.Since(lastReload) >= r.debounce {
				reload()
			} else {
				arm()
			}
		case <-pruneTicker.C:
			// Each signals a dependency change (handled above) when anything
			// expired: observed (ODCDS) entries past their idle TTL, and
			// retained absent services past the retention grace — the latter
			// receive no watch events in steady state under scoped watches,
			// so expiry must be time-driven, not event-driven.
			r.cache.PruneObservedDependencies()
			r.cache.SignalIfRetentionExpired()
		case <-timer.C:
			reload()
		}
	}
}
