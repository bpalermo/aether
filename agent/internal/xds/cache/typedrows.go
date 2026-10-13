package cache

import (
	"context"
	"maps"
	"slices"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry"
	"aethermesh.dev/registry/endpointmeta"
)

// typedRegistry is the registry a refresh reads through: it hands on only the
// rows that say the protocol of each of their ports (endpointmeta.RowProblem)
// and remembers the ones it dropped.
//
// port_protocols is required (proposal 043). A row without it used to take
// the class of the listing it was read from. Now it contributes NOTHING: no
// endpoint, no port, no class, on any path. Every reader of the registry in
// the agent is a refresh (LoadClustersFromRegistry), on the node proxy and on
// the edge alike, and everything else is derived from what it stores (the
// HTTP clusters and their QUIC twins, the raw-TCP and UDP floors, the
// waypoint's view of remote clusters, the ports the route tables and the
// chains are built from), so the one filter here covers them all.
//
// The registrar filters such rows too, and refuses to store new ones; an agent
// is handed one only by a registrar of an older chart. It is refused here all
// the same: the agent must not depend on who it is talking to for this.
type typedRegistry struct {
	registry.Registry
	// refused: row identity ("<service>|<listing>|<cluster>|<ip>") -> reason,
	// for the rows this refresh dropped.
	refused map[string]string
}

func (t *typedRegistry) keep(service string, protocol registryv1.Service_Protocol, rows []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
	clean := true
	for _, ep := range rows {
		if endpointmeta.RowProblem(ep) != "" {
			clean = false
			break
		}
	}
	if clean {
		return rows
	}
	kept := make([]*registryv1.ServiceEndpoint, 0, len(rows))
	for _, ep := range rows {
		if problem := endpointmeta.RowProblem(ep); problem != "" {
			t.refused[service+"|"+protocol.String()+"|"+ep.GetClusterName()+"|"+ep.GetIp()] = problem
			continue
		}
		kept = append(kept, ep)
	}
	return kept
}

// ListAllEndpoints returns the registry's listing without the untyped rows. A
// service left with no row is left out, as if it were not listed. The
// registry's own map is not written to.
func (t *typedRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	all, err := t.Registry.ListAllEndpoints(ctx, protocol)
	if err != nil {
		return nil, err
	}
	var out map[string][]*registryv1.ServiceEndpoint
	for service, rows := range all {
		kept := t.keep(service, protocol, rows)
		if len(kept) == len(rows) {
			continue
		}
		if out == nil {
			out = maps.Clone(all)
		}
		if len(kept) == 0 {
			delete(out, service)
		} else {
			out[service] = kept
		}
	}
	if out == nil {
		return all, nil
	}
	return out, nil
}

// ListEndpoints is the same for one service.
func (t *typedRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	rows, err := t.Registry.ListEndpoints(ctx, service, protocol)
	if err != nil {
		return nil, err
	}
	return t.keep(service, protocol, rows), nil
}

// typedCatalogRegistry is typedRegistry over a registry that also is a
// registry.ServiceCatalog, the one optional capability a refresh asks its
// registry for. A registry that is none stays none behind the filter.
type typedCatalogRegistry struct {
	*typedRegistry
	catalog registry.ServiceCatalog
}

// HasService forwards registry.ServiceCatalog.
func (t *typedCatalogRegistry) HasService(name string) bool { return t.catalog.HasService(name) }

// typedRowsOf wraps reg in the filter, and returns the filter to read what it
// dropped from.
func typedRowsOf(reg registry.Registry) (registry.Registry, *typedRegistry) {
	typed := &typedRegistry{Registry: reg, refused: map[string]string{}}
	if catalog, ok := reg.(registry.ServiceCatalog); ok {
		return &typedCatalogRegistry{typedRegistry: typed, catalog: catalog}, typed
	}
	return typed, typed
}

// reportRefusedRows says which registry rows a refresh did not serve, once per
// row and reason for as long as the row is listed: a refresh runs on every
// endpoint change, and a row that is still there is not news. A row that left
// and comes back is said again.
func (c *SnapshotCache) reportRefusedRows(ctx context.Context, now map[string]string) {
	c.clusterMu.Lock()
	last := c.refusedRows
	c.refusedRows = now
	c.clusterMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(now)) {
		if last[id] == now[id] {
			continue
		}
		c.metrics.RegistryRowRefused(ctx, now[id])
		c.log.WarnContext(ctx, "a registry row is not served: it does not say the protocol of each of its ports (port_protocols is required). "+
			"It contributes no endpoint, no port and no class; its pod must be registered again by a node agent of this chart",
			"row", id, "reason", now[id])
	}
}
