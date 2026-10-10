package cache

import (
	"context"
	"fmt"
	"slices"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
)

// A resource name is the key of everything downstream of a snapshot build
// (issue #1584). go-control-plane indexes each type's resources by name and
// keeps the LAST one it is handed under a name, without an error; the proxy
// holds a listener or a cluster under its name; the agent's acknowledgement
// tracker and its health gateway paths are keyed by it. Two resources of one
// type under one name therefore do not reach the proxy as two: one is dropped,
// and since the slices handed over are sorted by name with a STABLE sort over
// a map walk, which one is dropped can change from build to build.
//
// The generators are meant to make that unrepresentable (proxy.PodResourceKey
// puts the namespace into every per-pod name). This is the check that says so
// when they do not: it changes nothing about what is published, it makes the
// condition visible, as an ERROR naming the type and the names and as
// aether.agent.snapshot.duplicate_resource_names.

// duplicateNamesLogged caps the names one ERROR line carries per type.
const duplicateNamesLogged = 20

// duplicateResourceNames returns, per xDS type, the sorted names under which
// that type carries two resources that DIFFER, and how many such names there
// are in all. Types without one are absent from the map.
//
// Two resources of one name that are equal are not reported: whichever of the
// two go-control-plane keeps, the proxy is sent the same thing.
//
// The check is about names, not about pods, and per-pod names are not its
// only input. Known today: two sandboxes of one pod (same namespace and name,
// two network namespaces). A service's bare load assignment, which its HTTP,
// TCP and UDP entries all name, was a second one until #1635 (a retained HTTP
// entry's empty one beside a live TCP entry's; a service listed under TCP and
// UDP): exactly one entry holds it now (ownsBareCLALocked,
// retainAbsentClustersLocked), so the check has nothing to say about it.
func duplicateResourceNames(resources map[resourcev3.Type][]types.Resource) (map[resourcev3.Type][]string, int) {
	var (
		out   map[resourcev3.Type][]string
		total int
	)
	for typ, list := range resources {
		first := make(map[string]types.Resource, len(list))
		differ := map[string]struct{}{}
		for _, r := range list {
			name := cachev3.GetResourceName(r)
			prev, seen := first[name]
			if !seen {
				first[name] = r
				continue
			}
			if !proto.Equal(prev, r) {
				differ[name] = struct{}{}
			}
		}
		if len(differ) == 0 {
			continue
		}
		names := make([]string, 0, len(differ))
		for name := range differ {
			names = append(names, name)
		}
		slices.Sort(names)
		if out == nil {
			out = make(map[resourcev3.Type][]string)
		}
		out[typ] = names
		total += len(names)
	}
	return out, total
}

// reportDuplicateResourceNames logs and counts the names that more than one
// resource of a type carries in the snapshot about to be built from resources.
//
// The count is added on every build the condition lasts for. The ERROR is
// written when the set of duplicated names CHANGES (and an INFO when it
// empties), not on every build: a snapshot is rebuilt on every pod and
// registry event, and one line per build would bury the line that says what
// is wrong. The caller holds snapshotMu.
func (c *SnapshotCache) reportDuplicateResourceNames(ctx context.Context, resources map[resourcev3.Type][]types.Resource) {
	dups, total := duplicateResourceNames(resources)
	c.metrics.DuplicateResourceNames(ctx, int64(total))

	// fmt prints a map in key order, and each value is sorted: one set of
	// duplicated names always has one signature.
	signature := ""
	if total > 0 {
		signature = fmt.Sprint(dups)
	}
	if signature == c.duplicateNamesSeen {
		return
	}
	c.duplicateNamesSeen = signature
	if total == 0 {
		c.log.InfoContext(ctx, "no resource name is carried by more than one resource any more")
		return
	}
	typs := make([]resourcev3.Type, 0, len(dups))
	for typ := range dups {
		typs = append(typs, typ)
	}
	slices.Sort(typs)
	for _, typ := range typs {
		names := dups[typ]
		c.log.ErrorContext(ctx, "more than one resource of a type carries the same name: the proxy is sent only one of them, and which one can change from build to build",
			"type", typ,
			"count", len(names),
			"names", names[:min(len(names), duplicateNamesLogged)])
	}
}
