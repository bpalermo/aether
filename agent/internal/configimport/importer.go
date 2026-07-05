// Package configimport materializes cross-cluster GAMMA config on a spoke (proposal
// 026, multi-cluster config propagation — the consumer side). It periodically pulls
// the clusterset-wide config projections from the registrar (registry.ConfigImporter
// — agents never read the store directly) and feeds the imported routes into the xDS
// cache, where they merge with local routes (local wins). Eventually-consistent / AP:
// a fetch failure keeps the last-known imported set in place.
package configimport

import (
	"context"
	"log/slog"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
)

// Sink receives the materialized imported routes (the xDS SnapshotCache).
type Sink interface {
	SetImportedServiceRoutes(routes map[string][]proxy.GammaRoute)
	// SetImportedServiceChainFilters receives the imported service-wide always-on
	// extension filters (025 M4 CHAIN scope over the 026 channel); local wins.
	SetImportedServiceChainFilters(filters map[string]proxy.ExtensionFilter)
}

// Importer polls the registrar for cross-cluster config projections and materializes
// them into the cache. Implements controller-runtime's Runnable (Start).
type Importer struct {
	importer   registry.ConfigImporter
	sink       Sink
	ownCluster string
	// controlCluster, when non-empty, is the single authorized config exporter
	// (proposal 026 EM3, Option E control-cluster authority): the importer then trusts
	// ONLY projections originating there and ignores all other origins. Empty =
	// federated (any peer origin is trusted, highest-version wins).
	controlCluster string
	interval       time.Duration
	log            *slog.Logger
	metrics        *metrics
}

// NewImporter builds the config-import poller. ownCluster is this cluster's name; the
// importer SKIPS projections that originate here (a cluster's own export is redundant
// with its local routes). controlCluster (optional) restricts trust to a single hub
// origin (EM3). interval defaults to 10s when non-positive.
func NewImporter(imp registry.ConfigImporter, sink Sink, ownCluster, controlCluster string, interval time.Duration, log *slog.Logger) *Importer {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Importer{
		importer:       imp,
		sink:           sink,
		ownCluster:     ownCluster,
		controlCluster: controlCluster,
		interval:       interval,
		log:            commonlog.Named(log, "configimport"),
		metrics:        newMetrics(),
	}
}

// Start runs the poll loop until ctx is cancelled (controller-runtime Runnable).
func (i *Importer) Start(ctx context.Context) error {
	i.log.InfoContext(ctx, "starting cross-cluster config import", "interval", i.interval, "own_cluster", i.ownCluster)
	t := time.NewTicker(i.interval)
	defer t.Stop()
	i.poll(ctx) // initial pull so imported config is present before the first tick
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			i.poll(ctx)
		}
	}
}

// poll fetches the projections and materializes them. A failure is logged and the
// last-known imported set is retained (AP).
func (i *Importer) poll(ctx context.Context) {
	projections, err := i.importer.ListConfig(ctx)
	if err != nil {
		i.metrics.fetchError(ctx)
		i.log.WarnContext(ctx, "config import fetch failed; keeping last-known imported routes", "error", err)
		return
	}
	routes, chainFilters, oldest := i.materialize(projections)
	i.sink.SetImportedServiceRoutes(routes)
	i.sink.SetImportedServiceChainFilters(chainFilters)
	// EM4: surface propagation lag — the age of the OLDEST imported projection. Negative
	// when nothing carried a parseable export timestamp.
	age := -1.0
	if !oldest.IsZero() {
		age = time.Since(oldest).Seconds()
	}
	i.metrics.observe(ctx, len(routes), age)
}

// materialize converts peer-origin projections into a service→routes map, skipping
// this cluster's own exports. On a same-service conflict across peers the
// higher-version projection wins (deterministic, last-writer-by-version). It also
// returns the oldest materialized projection's export time (zero when none parseable)
// for the EM4 propagation-lag metric.
func (i *Importer) materialize(projections []*registryv1.ServiceConfigProjection) (map[string][]proxy.GammaRoute, map[string]proxy.ExtensionFilter, time.Time) {
	type chosen struct {
		version string
		routes  []proxy.GammaRoute
		filter  *registryv1.ExtensionFilter
	}
	picked := make(map[string]chosen, len(projections))
	for _, p := range projections {
		if p.GetOriginCluster() == i.ownCluster {
			continue // own export — local routes already cover it
		}
		// EM3 control-cluster authority: when a hub is designated, trust only its config.
		if i.controlCluster != "" && p.GetOriginCluster() != i.controlCluster {
			continue
		}
		svc := p.GetService()
		if svc == "" {
			continue
		}
		if cur, ok := picked[svc]; ok && cur.version >= p.GetVersion() {
			continue
		}
		picked[svc] = chosen{version: p.GetVersion(), routes: proxy.FromConfigProjection(p), filter: p.GetServiceFilter()}
	}
	if len(picked) == 0 {
		return nil, nil, time.Time{}
	}
	out := make(map[string][]proxy.GammaRoute, len(picked))
	var chainFilters map[string]proxy.ExtensionFilter
	var oldest time.Time
	for svc, c := range picked {
		out[svc] = c.routes
		if c.filter != nil {
			if chainFilters == nil {
				chainFilters = map[string]proxy.ExtensionFilter{}
			}
			chainFilters[svc] = proxy.ExtensionFilter{Name: c.filter.GetName(), Config: c.filter.GetConfig()}
		}
		// version is the exporter's RFC3339Nano timestamp (proposal 026 EM1c); track the
		// oldest for the propagation-lag metric. Unparseable (e.g. a test stamp) → ignored.
		if ts, err := time.Parse(time.RFC3339Nano, c.version); err == nil {
			if oldest.IsZero() || ts.Before(oldest) {
				oldest = ts
			}
		}
	}
	return out, chainFilters, oldest
}
