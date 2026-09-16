package cache

import (
	"slices"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
)

// Determinism is protocol-visible (incident #135, and its RDS redux #772/A4).
//
// Go randomises map iteration, so any xDS resource assembled by ranging a map
// comes out in a different order on every rebuild even when nothing changed.
// Two mechanisms then punish it:
//
//   - go-control-plane versions each resource for delta-xDS by hashing its
//     marshalled bytes (Snapshot.ConstructVersionMap → MarshalResource →
//     HashResource). proto.MarshalOptions{Deterministic:true} canonicalises
//     *map* fields only — the order of a repeated field is preserved exactly as
//     built. A reshuffled repeated field is therefore a different resource.
//   - Envoy skips an update only when the new resource's MessageUtil::hash is
//     unchanged, so a reshuffle makes it rebuild the resource for real: a full
//     route-table rebuild per RDS push, a listener replacement per LDS push.
//
// These helpers give every map-derived resource slice a stable, total order.
// Resource names are unique within a type (go-control-plane indexes resources
// by name), and virtual-host names are unique within a route configuration, so
// name ordering is total — no ties to break.

// sortResourcesByName orders an xDS resource slice by resource name.
func sortResourcesByName(resources []types.Resource) {
	slices.SortStableFunc(resources, func(a, b types.Resource) int {
		an, bn := cachev3.GetResourceName(a), cachev3.GetResourceName(b)
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	})
}

// sortVirtualHostsByName orders a virtual-host slice by name. Envoy matches a
// virtual host by domain specificity, never by position in the list, so the
// order carries no routing semantics — only bytes.
func sortVirtualHostsByName(vhosts []*routev3.VirtualHost) {
	slices.SortStableFunc(vhosts, func(a, b *routev3.VirtualHost) int {
		an, bn := a.GetName(), b.GetName()
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	})
}
