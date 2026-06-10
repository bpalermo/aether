package server

import (
	"context"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// defaultRefreshDebounce is how long the refresher waits for change signals to
// settle before rebuilding the cluster snapshot, collapsing bursts (e.g. a full
// snapshot replay) into a single reload.
const defaultRefreshDebounce = 250 * time.Millisecond

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
	log      logr.Logger
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

// NewRegistryRefresher creates a RegistryRefresher.
func NewRegistryRefresher(clusterName, nodeName string, snapshotCache *cache.SnapshotCache, reg registry.Registry, log logr.Logger) *RegistryRefresher {
	r := &RegistryRefresher{
		log:         log.WithName("registry-refresher"),
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
		r.log.Error(err, "failed to create coalesced counter; continuing without instrumentation")
	}
	if r.refreshErrors, err = meter.Int64Counter("aether.agent.refresher.errors",
		metric.WithDescription("Failed cluster reloads from the registry (snapshot left stale until the next change signal)")); err != nil {
		r.log.Error(err, "failed to create refresh errors counter; continuing without instrumentation")
	}

	return r
}

// Start blocks until ctx is cancelled, rebuilding the cluster snapshot from the
// registry on each (debounced) change notification. It implements
// controller-runtime's manager.Runnable.
func (r *RegistryRefresher) Start(ctx context.Context) error {
	notifier, ok := r.registry.(registry.ChangeNotifier)
	if !ok {
		r.log.Info("registry does not support change notifications; clusters refresh only at startup")
		<-ctx.Done()
		return nil
	}

	changes := notifier.Changes()

	// Debounce timer, created stopped: it is (re)armed on each change signal and
	// fires once the signals settle, triggering a single reload.
	timer := time.NewTimer(r.debounce)
	if !timer.Stop() {
		<-timer.C
	}

	r.log.V(1).Info("watching registry for endpoint changes")

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			// (Re)arm the debounce window. Stop+drain before Reset so a prior
			// expiry doesn't leave a stale tick queued. Stop reporting true
			// means the timer was already armed — this signal coalesced.
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
		case <-timer.C:
			if err := r.cache.LoadClustersFromRegistry(ctx, r.clusterName, r.nodeName, r.registry); err != nil {
				r.log.Error(err, "failed to refresh clusters from registry")
				if r.refreshErrors != nil {
					r.refreshErrors.Add(ctx, 1)
				}
				continue
			}
			r.log.V(1).Info("refreshed clusters from registry")
		}
	}
}
