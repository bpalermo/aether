package cache

import (
	"bytes"
	"fmt"
	"maps"
	"slices"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Issue #1115: a registry refresh reuses the cluster-cache entries of every
// service whose INPUTS did not change.
//
// #1105 memoizes a resource's xDS version on "the same proto object under the
// same name as the previous build", so the version of a resource that is not
// rebuilt costs nothing. LoadClustersFromRegistry used to rebuild every entry
// -- cluster, load assignment, mTLS clone -- as fresh protos on every refresh,
// so no registry-derived resource was ever a memo hit and every refresh
// re-marshalled and re-hashed all of them, although a refresh almost always
// changes one or two services.
//
// Now each service's entries are rebuilt only when the inputs its builder reads
// changed. The comparison is on the INPUTS, never on the outputs: the reuse key
// is the exact byte string below, compared with bytes.Equal (no digest, so no
// collision argument), and anything a builder reads that is not in it would be
// a stale resource. The key of one (pass, service) is
//
//   - the pass (HTTP, TCP or UDP): each runs a different builder;
//   - the node's locality (region, zone): EDS priority banding;
//   - the waypoint rewrite (enabled, tunnel port, local cluster): endpoint dial
//     address, priority, waypoint tag;
//   - the mesh domain and the edge flag: cluster names, FQDNs, aliases;
//   - for the TCP and UDP passes, whether the entry owns the service's bare-name
//     load assignment (it does unless an HTTP entry built in the same refresh
//     owns it, buildTCPEndpointsLocked);
//   - every registry row of the service, in listing order, as its
//     deterministic protobuf encoding. A row is the WHOLE ServiceEndpoint: ip,
//     port, ports, port_protocols, health (so a DRAINING or UNHEALTHY mark is a
//     key change), health-check mode, locality, cluster, Kubernetes metadata
//     (namespace -> SAN pin, pod, node IP -> waypoint), endpoint metadata
//     (subset keys), and any field added later, unknown fields included. An
//     added or removed endpoint changes the row list. Order is kept because
//     the builder reads it (the default port is the first row's).
//
// Inputs deliberately NOT in the key, and why that is sound:
//
//   - GAMMA rules and service chain filters feed only the outbound vhosts,
//     which are not xDS resources (they are embedded in out_http, rebuilt on
//     every snapshot). A reused HTTP entry gets its vhosts rebuilt from the
//     current rules every refresh (refreshReusedVhostsLocked), so they are
//     never stale and need no key.
//   - The local mTLS state (node SVID, trust domain): the mTLS-injected cluster
//     is not built here but by refreshEntryMTLSLocked, which has its own
//     render key (mtlsRenderKey) covering it.
//   - The QUIC fan-out: twins and selection arms are derived per snapshot from
//     the entries, never stored on them.
//   - The dependency set: it decides WHICH services are built (a service that
//     leaves it is not built at all), not what a built entry contains.
//
// A record is reused only if every entry it produced is still in the cluster
// map exactly as built (same cluster and load-assignment objects, not retained
// as absent). The other writers of the map -- RemoveEndpoint (replaces the load
// assignment), RemoveCluster (deletes), the absent-service retention (replaces
// the load assignment and sets absentSince) -- therefore invalidate it.
//
// Reuse never mutates a published proto: a reused entry is the previous
// refresh's entry value, copied as is; a changed key builds NEW objects, as
// before. In this package's tests every reuse is audited against a fresh build
// and a difference panics (registryReuseAudit), so the whole suite is the
// stale-entry detector, as it is for the version memo.
type registryReuse struct {
	// records is the previous refresh's record per (pass, service).
	records map[string]reuseRecord
	// buf and row are the key and row-marshal buffers, reused across
	// services.
	buf, row []byte
	// reused and built count services in the last refresh (tests, logs).
	reused, built int
}

// reuseRecord is what one (pass, service) build produced.
type reuseRecord struct {
	key []byte
	// names are the cluster-map keys the build wrote, sorted.
	names []string
	// fps fingerprint each name's entry as built (parallel to names).
	fps []entryFingerprint
	// subsetKeys are the service's sorted subset keys, folded into the
	// node-wide union on reuse exactly as the builder folds them.
	subsetKeys []string
}

type entryFingerprint struct {
	cluster        *clusterv3.Cluster
	loadAssignment *endpointv3.ClusterLoadAssignment
}

func fingerprint(e clusterEntry) entryFingerprint {
	return entryFingerprint{cluster: e.cluster, loadAssignment: e.loadAssignment}
}

// reusePass names the builder a record belongs to.
type reusePass byte

const (
	reusePassHTTP reusePass = 'h'
	reusePassTCP  reusePass = 't'
	reusePassUDP  reusePass = 'u'
)

// reuseInputs are the refresh-wide builder inputs that enter every key.
type reuseInputs struct {
	localRegion, localZone string
	waypoint               proxy.WaypointRewrite
}

// registryReuseAudit makes every reuse rebuild the entries fresh and panic on
// any difference. Tests only (versionmemo_strict_test.go); production never
// writes it.
var registryReuseAudit = false

// beginRegistryReuseLocked starts a refresh: the records it fills replace the
// previous refresh's at commit. Caller holds clusterMu for writing.
func (c *SnapshotCache) beginRegistryReuseLocked() map[string]reuseRecord {
	c.registryReuse.reused, c.registryReuse.built = 0, 0
	return make(map[string]reuseRecord, len(c.registryReuse.records))
}

// commitRegistryReuseLocked keeps only this refresh's records, so a record is
// always from the immediately preceding refresh and never outlives its
// service. Caller holds clusterMu for writing.
func (c *SnapshotCache) commitRegistryReuseLocked(next map[string]reuseRecord) {
	c.registryReuse.records = next
}

// reuseKey appends the exact key of one (pass, service) to buf[:0].
func (r *registryReuse) reuseKey(pass reusePass, meshDomain string, edge, ownsBareCLA bool, in reuseInputs, endpoints []*registryv1.ServiceEndpoint) ([]byte, error) {
	b := r.buf[:0]
	b = append(b, byte(pass))
	b = protowire.AppendString(b, in.localRegion)
	b = protowire.AppendString(b, in.localZone)
	b = protowire.AppendVarint(b, protowire.EncodeBool(in.waypoint.Enabled))
	b = protowire.AppendVarint(b, uint64(in.waypoint.TunnelPort))
	b = protowire.AppendString(b, in.waypoint.LocalCluster)
	b = protowire.AppendString(b, meshDomain)
	b = protowire.AppendVarint(b, protowire.EncodeBool(edge))
	b = protowire.AppendVarint(b, protowire.EncodeBool(ownsBareCLA))
	b = protowire.AppendVarint(b, uint64(len(endpoints)))
	for _, ep := range endpoints {
		// Length-prefix each row so two different row lists can never
		// concatenate to the same bytes.
		row, err := deterministic.MarshalAppend(r.row[:0], ep)
		if err != nil {
			return nil, err
		}
		r.row = row
		b = protowire.AppendBytes(b, row)
	}
	r.buf = b
	return b, nil
}

// reuseOrBuildLocked publishes one (pass, service)'s entries into c.clusters:
// the previous refresh's entries when its key is unchanged and they are still
// in prev exactly as built, otherwise a fresh build. build writes the entries
// into dst and the service's subset keys into subsetKeys; refreshReused, when
// set, re-derives the parts of reused entries that are deliberately not in the
// key (the HTTP vhosts). It returns the subset keys to fold into the
// node-wide union and whether the entries were reused. Caller holds clusterMu
// for writing.
func (c *SnapshotCache) reuseOrBuildLocked(
	pass reusePass, serviceName string, endpoints []*registryv1.ServiceEndpoint, ownsBareCLA bool, in reuseInputs,
	prev map[string]clusterEntry, next map[string]reuseRecord,
	build func(dst map[string]clusterEntry, subsetKeys map[string]struct{}),
	refreshReused func(names []string),
) (subsetKeys []string, reused bool) {
	recKey := string(pass) + "\x00" + serviceName
	key, err := c.registryReuse.reuseKey(pass, c.meshDomain, c.edge, ownsBareCLA, in, endpoints)
	if err != nil {
		// Unreachable for a valid proto; never reuse on doubt.
		key = nil
	}
	if rec, ok := c.registryReuse.records[recKey]; ok && key != nil && bytes.Equal(rec.key, key) && rec.intactIn(prev) {
		for _, name := range rec.names {
			c.clusters[name] = prev[name]
		}
		if refreshReused != nil {
			refreshReused(rec.names)
		}
		if registryReuseAudit {
			c.auditReusedLocked(serviceName, rec, build)
		}
		next[recKey] = rec
		c.registryReuse.reused++
		return rec.subsetKeys, true
	}

	dst := make(map[string]clusterEntry, 4)
	keys := map[string]struct{}{}
	build(dst, keys)
	rec := reuseRecord{
		names:      slices.Sorted(maps.Keys(dst)),
		subsetKeys: proxy.SortSubsetKeys(keys),
	}
	if key != nil {
		rec.key = bytes.Clone(key)
	}
	rec.fps = make([]entryFingerprint, len(rec.names))
	for i, name := range rec.names {
		c.clusters[name] = dst[name]
		rec.fps[i] = fingerprint(dst[name])
	}
	if key != nil {
		next[recKey] = rec
	}
	c.registryReuse.built++
	return rec.subsetKeys, false
}

// intactIn reports whether every entry the record produced is still in m
// exactly as built: present, the same cluster and load-assignment objects, and
// not retained as absent.
func (r reuseRecord) intactIn(m map[string]clusterEntry) bool {
	for i, name := range r.names {
		e, ok := m[name]
		if !ok || !e.absentSince.IsZero() || fingerprint(e) != r.fps[i] {
			return false
		}
	}
	return true
}

// auditReusedLocked rebuilds a reused (pass, service) fresh and panics if
// anything published from it differs (tests only, registryReuseAudit).
// Called after the reused entries' vhosts were refreshed. Caller holds
// clusterMu for writing.
func (c *SnapshotCache) auditReusedLocked(serviceName string, rec reuseRecord, build func(dst map[string]clusterEntry, subsetKeys map[string]struct{})) {
	dst := map[string]clusterEntry{}
	keys := map[string]struct{}{}
	build(dst, keys)
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("registry reuse: service %q reused a STALE entry (#1115): ", serviceName) + fmt.Sprintf(format, args...))
	}
	if got := proxy.SortSubsetKeys(keys); !slices.Equal(got, rec.subsetKeys) {
		fail("subset keys %v, fresh build %v", rec.subsetKeys, got)
	}
	if got := slices.Sorted(maps.Keys(dst)); !slices.Equal(got, rec.names) {
		fail("entries %v, fresh build %v", rec.names, got)
	}
	for name, fresh := range dst {
		cur, ok := c.clusters[name]
		if !ok {
			fail("fresh build has %q, reuse does not", name)
		}
		if d := entryDiff(cur, fresh); d != "" {
			fail("%q %s", name, d)
		}
	}
}

// entryDiff names the first published difference between a reused entry and
// a fresh build of it, or "" when they agree.
func entryDiff(cur, fresh clusterEntry) string {
	switch {
	case !proto.Equal(cur.cluster, fresh.cluster):
		return "cluster differs"
	case !proto.Equal(cur.loadAssignment, fresh.loadAssignment):
		return "load assignment differs"
	case !proto.Equal(cur.vhost, fresh.vhost):
		return "vhost differs"
	case len(cur.endpoints) != len(fresh.endpoints):
		return fmt.Sprintf("endpoint map size %d, fresh %d", len(cur.endpoints), len(fresh.endpoints))
	case !slices.Equal(cur.sanNamespaces, fresh.sanNamespaces) || cur.service != fresh.service || cur.sni != fresh.sni ||
		cur.l4Floor != fresh.l4Floor || cur.bareEDSAlias != fresh.bareEDSAlias:
		return "entry facts differ"
	}
	for ip, ep := range fresh.endpoints {
		if !proto.Equal(cur.endpoints[ip], ep) {
			return "endpoint " + ip + " differs"
		}
	}
	return ""
}
