# Proposal: multi-region etcd federation (region-scoped registry ownership)

**Status:** Design — 2026-06-20
**Relates:** [`docs/registry-backend-evolution.md`](../registry-backend-evolution.md)
(the multi-region directive this implements), proposal 004 (demand-scoped
distribution), proposal 000 (in-cluster registrar); [[project_registrar_etcd_vs_ddb]],
[[project_multicluster_registry]].

## Summary

Make the etcd registry **region-scoped and origin-partitioned** so the mesh can
run a per-region etcd cluster with asynchronous, conflict-free cross-region
replication (the 2026-06-13 directive). The core is a **key-schema change**: the
origin (region, then cluster) moves to the **front** of the key, giving each
region one contiguous authoritative subtree that a replicator mirrors verbatim
into peer regions.

Ship the schema + region/cluster config **first** (single-region default, no
behavior change), so the replicator and region-failover work land later as a
clean add instead of a live re-keying migration.

## Context

### Today

The etcd backend (`registry/internal/etcd/etcd.go`) keys endpoints by IP under a
flat, service-first prefix; the cluster lives only in the serialized value:

```
/aether/services/<service>/protocols/<proto>/endpoints/<ip>      # value carries clusterName, region, zone
```

This is fine for a single shared etcd, but it has a latent multi-region hazard:
pod CIDRs overlap across clusters, so replicating region A's
`…/endpoints/10.244.1.5` into region B's flat tree would **clobber** B's own pod
at the same IP. Origin is not in the key, so the partitions aren't disjoint.

### The directive

Per-region etcd cluster + asynchronous cross-region replication, eventual
consistency between regions accepted (see `registry-backend-evolution.md` §
"Multi-region"). Replication must stay conflict-free by construction, reusing the
mesh's existing discipline: *registrar per cluster, none authoritative,
scoped reconciliation, no unilateral GC.*

## Decision: region-scoped ownership, origin-first key

**Each region owns its authoritative subtree and is the sole cross-region writer
of it.** Encode the origin in the key, region first:

```
/aether/v1/regions/<region>/clusters/<cluster>/services/<service>/protocols/<proto>/endpoints/<ip>
```

- `<region>` — the etcd boundary and the replication / failover unit. A region is
  the sole authoritative writer of `/aether/v1/regions/<region>/` in **every**
  etcd; peers only ever see that subtree mirrored (read-only locally).
- `<cluster>` — nested under region so the existing **cluster-scoped**
  reconciliation (ghost sweep, no-unilateral-GC) is unchanged. Multiple clusters
  per region share the region partition but write disjoint cluster sub-keys.
- `<service>/<proto>/<ip>` — unchanged from today.
- `v1` — a schema-version segment so any future layout change is itself
  migratable.

Disjoint origin partitions ⇒ one writer per partition ⇒ last-write-wins is a
no-op ⇒ eventual consistency is trivial, regardless of pod-CIDR overlap.

### Why origin-first (not service-first)

The conflict-free property only needs the origin *somewhere* in the key. Putting
it **first** is what makes the **replicator's base path deterministic** — the
property that turns replication into a verbatim subtree mirror:

| Layout | A region's authoritative data is… | Replicator source |
|---|---|---|
| service-first `…/services/<svc>/clusters/<self>/…` | **scattered** under every service subtree | watch `/aether/…/services/` (everything) and filter `/clusters/<self>/` mid-key |
| **origin-first** `…/regions/<self>/…` (this proposal) | **one contiguous subtree** | watch/range `/aether/v1/regions/<self>/` — mirror verbatim |

Origin-first also keeps every consumer's access pattern a clean prefix range:

- **Replicator** ranges/watches `/aether/v1/regions/<self>/` — its own authoritative subtree.
- **Ghost sweep** (per cluster) ranges `/aether/v1/regions/<self>/clusters/<cluster>/` — no read-all-and-filter.
- **Registrar** watches `/aether/v1/regions/` — the whole local etcd (local-authoritative *plus* mirrored-in), and groups by service in memory exactly as it does today, one level up. It stays region-local and never knows about other regions.

## Ownership & conflict-freedom

```
Region A: agents → registrar(A) → etcd(A) ──/regions/A mirror──▶ etcd(B), etcd(C)
Region B: agents → registrar(B) → etcd(B) ──/regions/B mirror──▶ etcd(A), etcd(C)
```

- **Single writer per origin subtree.** A region authoritatively writes only
  `/regions/<self>/`; foreign endpoints arrive mirrored into `/regions/<peer>/`
  and are read-only in the local etcd.
- **Region-granular lease / self-healing failover.** Mirrored keys carry the
  **origin region's** heartbeat lease, refreshed by that region's replicator. If
  region A (or the inter-region link) drops, all of A's foreign replicas
  everywhere expire together — the origin's heartbeat lapsing, *not* a peer
  judging it dead — which honors the no-unilateral-GC directive while giving
  automatic cross-region cleanup. A single cluster failing inside a healthy
  region is still handled by that region's normal intra-region sweep, not the
  lease.
- **Locality keeps staleness off the hot path.** Foreign-region endpoints are
  EDS priority 2 (failover only, proposal 004 locality), so accepted
  cross-region staleness rides the non-critical path; the steady path is always
  local.

**Trade accepted:** the replication/failover unit is the **region**, not the
individual cluster — the right grain when a region is the etcd boundary and the
blast-radius unit, but it means clusters within a region aren't independently
replicated (they share the region's mirror stream).

## The replication mechanism (deferred — Phase 2)

A per-region **replicator** (a thin purpose-built component, or a registrar mode)
watches its own `/aether/v1/regions/<self>/` prefix and replays changes into peer
regions' etcd. This is etcd↔etcd replication, **not** registrar-to-registrar
federation — deliberately, to avoid cross-trust-domain mTLS (SPIFFE federation)
and a WAN gossip mesh. `etcdctl make-mirror` fits the one-directional-per-origin
shape as a reference but lacks lease management and origin-filtering, so it's
purpose-built. Full spec — lease/tombstone semantics, resume after disconnect,
compaction-resync, HA/leader-election, and a region-failover e2e — is Phase 2 and
out of scope for this document beyond the shape above.

## Phasing

1. **Schema + topology config (this proposal, buildable now).**
   - Add `region` and `cluster` to the etcd key builder; default `region` from
     the node/registrar topology (`topology.kubernetes.io/region`) or a
     `--region` flag, `cluster` from the existing `--cluster-name`.
   - Single region in play ⇒ everything lands under one `/regions/<region>/`
     subtree; **no behavior change**, no replication, no cross-region reads.
   - Registrar watch root moves to `/aether/v1/regions/`. Ghost sweep ranges its
     `/regions/<self>/clusters/<cluster>/` slice.
2. **Replicator** — per-region mirror with leases/tombstones/resume/HA.
3. **Region-failover e2e** — validate priority-2 failover + lease-expiry cleanup
   when a region is cut off.

## Migration

The key path is centralized in `endpointKey()` /
`endpointsPrefix()` / `protocolsPrefix()` (`registry/internal/etcd/etcd.go`), so
the change is contained. Because endpoints are short-lived (re-registered by the
ghost sweep / CNI ADD) and the value already carries `region`/`zone`/`clusterName`,
the simplest cutover is **re-register under the new prefix**: deploy the new key
builder, let the next sweep/registration cycle populate `/aether/v1/…`, and
range-delete the legacy `/aether/services/…` prefix once drained. No value
re-marshaling is needed (origin data already exists in the value). Doing this in
Phase 1, while single-region, means no live multi-region data is ever re-keyed.

## Alternatives considered

- **Service-first with cluster-in-key** (the prior sketch in
  `registry-backend-evolution.md`): conflict-free but scatters each origin's data
  under every service subtree, so the replicator has no single base prefix — it
  must watch the whole tree and filter. Rejected: defeats the deterministic-mirror
  goal. (That sketch is updated to this layout.)
- **Cluster as the replication unit** (per-cluster leases/mirrors): finer grain,
  but multiplies replicator streams and leases without benefit when the etcd and
  blast-radius boundary is the region. Region-scoped ownership with cluster nested
  keeps per-cluster reconciliation locally while making the cross-region unit the
  region.
- **DynamoDB global tables:** a managed multi-region store isn't worth adopting
  *just* for global tables; per-region etcd dissolves the original objections (see
  `registry-backend-evolution.md`).

## Open questions

- **Region identity source:** topology label vs explicit `--region` flag vs
  derived from the etcd endpoint set. Leaning explicit `--region` (deterministic,
  decoupled from node labels), defaulting to the topology label.
- **Tombstones vs lease-only expiry** for replicated deletes (Phase 2): leases
  cover whole-region loss; individual endpoint deletes still want a mirrored
  tombstone so peers converge faster than lease TTL.
- Whether the **registrar** hosts the replicator as a mode or it's a separate
  Deployment (Phase 2 HA discussion).
