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
}

// Importer polls the registrar for cross-cluster config projections and materializes
// them into the cache. Implements controller-runtime's Runnable (Start).
type Importer struct {
	importer   registry.ConfigImporter
	sink       Sink
	ownCluster string
	interval   time.Duration
	log        *slog.Logger
}

// NewImporter builds the config-import poller. ownCluster is this cluster's name; the
// importer SKIPS projections that originate here (a cluster's own export is redundant
// with its local routes). interval defaults to 10s when non-positive.
func NewImporter(imp registry.ConfigImporter, sink Sink, ownCluster string, interval time.Duration, log *slog.Logger) *Importer {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Importer{
		importer:   imp,
		sink:       sink,
		ownCluster: ownCluster,
		interval:   interval,
		log:        commonlog.Named(log, "configimport"),
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
		i.log.WarnContext(ctx, "config import fetch failed; keeping last-known imported routes", "error", err)
		return
	}
	i.sink.SetImportedServiceRoutes(i.materialize(projections))
}

// materialize converts peer-origin projections into a service→routes map, skipping
// this cluster's own exports. On a same-service conflict across peers the
// higher-version projection wins (deterministic, last-writer-by-version).
func (i *Importer) materialize(projections []*registryv1.ServiceConfigProjection) map[string][]proxy.GammaRoute {
	type chosen struct {
		version string
		routes  []proxy.GammaRoute
	}
	picked := make(map[string]chosen, len(projections))
	for _, p := range projections {
		if p.GetOriginCluster() == i.ownCluster {
			continue // own export — local routes already cover it
		}
		svc := p.GetService()
		if svc == "" {
			continue
		}
		if cur, ok := picked[svc]; ok && cur.version >= p.GetVersion() {
			continue
		}
		picked[svc] = chosen{version: p.GetVersion(), routes: proxy.FromConfigProjection(p)}
	}
	if len(picked) == 0 {
		return nil
	}
	out := make(map[string][]proxy.GammaRoute, len(picked))
	for svc, c := range picked {
		out[svc] = c.routes
	}
	return out
}
