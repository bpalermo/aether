# Proposal: Demand-Scoped Service Distribution

**Status:** Draft — design decision pending (dependency declaration mechanism)
**Author:** Bruno Palermo
**Date:** 2026-06-12

## Problem Statement

Every node's proxy carries **every service in the mesh**: the agent's registry
refresher loads all clusters from the registrar snapshot, builds CDS/EDS for
all of them, and every client proxy actively health-checks every endpoint of
every cluster. This "every node knows everything" model is invisible at today's
scale (8 services, 5 nodes) and is the single binding constraint at the design
target of **3,000 services / 10,000 nodes / 100,000 pods**:

| Term | Cost at 3k svc / 10k nodes / 100k pods (10 pods/node) |
|---|---|
| Proxy stats, service term | 3,000 × 135 ≈ **405k stats/proxy** (~70–150 MB before per-worker caches) |
| Proxy config memory | 3k clusters (~30–50 KB each) + 100k resident EDS endpoints ≈ **200–300 MB/proxy** |
| Client-side active HC | 10k nodes × 100k endpoints / 5s ≈ **200M probes/sec mesh-wide** |
| Registrar fan-out | every endpoint change → 10k watch streams; at 1 %/min pod churn ≈ 10M stream-messages/min |
| Agent snapshot builds | every change rebuilds and pushes a 3k-cluster snapshot on every node |

Per-pod costs are already solved and stay O(local pods) = O(10): CNI, liveness,
health gateway, SDS, and (after stats rounds 1–2, #146/#147/#155) the per-pod
stats term (~16/pod). No further per-node tuning touches the service term —
the fix is distributing **less**, not compressing more.

The observation that makes this tractable: a node only needs a service if a
pod **on that node** calls it. Real dependency fan-outs are ~5–50 services;
10 pods/node × ~20 dependencies (with overlap) ≈ **≤200 clusters/node**, a
15–40× reduction in every table row above.

## Design Goals

1. A node's proxy carries clusters/endpoints only for the union of its local
   pods' dependencies (plus mesh-internal clusters: xDS, health gateway, OTel).
2. First request to a not-yet-distributed service must work (cold path), then
   become warm — no manifest-update-or-404 cliff.
3. Registrar fan-out per endpoint change scales with the service's consumer
   count, not the node count.
4. Per-endpoint health continues to come from delegated liveness via EDS; the
   O(clients×endpoints) active-HC term is retired, not just reduced.
5. Incremental: each phase deployable and revertible alone; the every-node-
   everything behavior remains available as a fallback flag until Phase 4.

## Proposed Solution

Scope all three layers — agent snapshot, registrar watch, client HC — by the
node's **dependency set**, derived from two sources that cover each other:

### Dependency sources

**A. Declared (warm path):** pods declare their upstreams:

```yaml
metadata:
  annotations:
    config.aether.io/upstreams: "svc-payments,svc-ledger,svc-audit"
```

The CNI plugin already forwards pod annotations in `CNIPod`; the agent unions
the annotations of its local pods into the node dependency set. Declared
dependencies are distributed *before* first use — no cold-start latency — and
double as reviewable, diffable architecture documentation. Operationally this
extends `workload-requirements.md`, exactly like `minReadySeconds`/`preStop`.
The `config.aether.io/*` prefix is distinct from `endpoint.aether.io/*`
(endpoint registration facts) — upstreams describe what the pod *consumes*,
not what it serves.

**B. Observed (cold path + undeclared tail):** when an outbound request names
a service outside the node's current set, the proxy must still route it. Two
candidate mechanisms, decided by spike (see Open Questions):

- **ODCDS** (Envoy on-demand CDS): the outbound HCM requests the unknown
  cluster on first use; the agent's xDS server serves it from the registrar
  snapshot and adds it to the node set (with a TTL'd membership so one-off
  calls don't pin clusters forever). First-request latency ≈ one xDS
  round-trip on the node-local UDS (~ms).
- **Fallback route → node egress cluster**: requests to unknown authorities
  route to a small dynamic-forward-proxy-style path while the agent observes
  the miss (access-log/filter-state tap), adds the service, and the next
  request takes the warm path.

Either way, **observed misses emit a metric** (`aether.agent.upstreams.miss`)
so undeclared dependencies are visible and can be promoted to annotations.

### Layer changes

**1. Agent snapshot (CDS/EDS/RDS):** `LoadClustersFromRegistry` builds clusters
for the node dependency set instead of the full snapshot. The outbound RDS
virtual-host set shrinks identically. Local pods' own services are always
included (a pod's service is trivially "depended on" by inbound).

**2. Registrar watch protocol:** `WatchEndpointsRequest` gains
`repeated string services` (empty = full watch, preserving today's behavior
and the fallback flag). The broadcaster indexes subscribers by service so an
endpoint change fans out to its consumers only. Dependency-set changes
(pod add/remove, annotation change, observed miss) re-issue the watch filter
on the live stream (new `UpdateFilter` message) or reconnect with the new set
— resume-token semantics unchanged. The agent re-asserts its filter on every
reconnect, same as registrations (#149).

**3. Client health model:** per-cluster active HC (`MeshReadyPath` on every
service cluster) is removed in favor of what the registry already provides —
delegated liveness as EDS `HealthStatus`, which every roll since #143 runs on:

- routability = EDS HEALTHY (registrar-vetted, ~1s propagation post-#145)
- drain = two-phase DRAINING→UNHEALTHY (#152), unchanged
- fast local failure detection = **outlier detection** (consecutive 5xx /
  connect failures) on service clusters, which is O(actual traffic), not
  O(clusters×endpoints)
- `IgnoreNewHostsUntilFirstHc` and the HC-warm-up machinery on service
  clusters go with it (EDS health already gates: endpoints register UNHEALTHY
  and are promoted after the destination-side gateway vets them)

The destination-side probe path (health gateway + `health_<pod>` clusters) is
untouched — it is O(local pods) and is the *source* of EDS health.

### What stays exactly as-is

- Registrar write path, write-behind, snapshot-first semantics (#145)
- Delegated-liveness promotion pipeline and the no-traffic HC pin (#146)
- Per-pod xDS resources (inbound/outbound listeners, app/health clusters)
- The registrar still holds the **full** snapshot; scoping is per-watcher

## Scale Model After

At 3k svc / 10k nodes / 100k pods, ~200-cluster node set, ~30 endpoints/svc:

| Term | Before | After |
|---|---|---|
| Stats svc term / proxy | ~405k | ~27k (~10–20 MB) |
| Cluster + EDS config / proxy | 200–300 MB | ~15–30 MB (≈6k resident endpoints) |
| Active HC probes mesh-wide | ~200M/s | 0 (outlier detection rides real traffic) |
| Registrar fan-out per change | 10k streams | consumers of that service (typ. 10–500) |
| Agent snapshot rebuild | 3k clusters | ~200 clusters |

OTel export volume shrinks proportionally with the stats term; if needed the
svc-cluster families move to a 60s flush as a separate knob.

## Failure Modes

- **Undeclared dependency, cold path down** (ODCDS not yet serving / fallback
  misroute): request fails until the miss path lands. Mitigation: declared
  annotations for everything latency- or correctness-critical; miss metric for
  the rest. The fallback flag (full distribution) remains an escape hatch.
- **Dependency-set churn thrash**: TTL'd observed entries + hysteresis (don't
  drop a cluster until idle > TTL) bound CDS churn; declared entries only
  change with pod specs.
- **Filter desync after registrar failover**: filter re-asserted on every
  reconnect (same discipline as endpoint re-assertion, #149); registrar treats
  an empty filter as full-watch, never less-than-asked.
- **Outlier detection vs panic-0 interaction**: ejection percent caps (e.g.
  `max_ejection_percent: 50`) prevent local ejection from emptying a cluster
  that EDS still believes healthy; needs explicit e2e (roll + induced 5xx).

## Implementation Phases

### Phase 1 — Scoped agent snapshot from declared dependencies
Annotation schema; agent unions local pods' dependencies; snapshot builds the
scoped set; fallback flag `--full-service-distribution` (default **on** until
Phase 4). E2e: declared-only cluster, verify scoped CDS + steady traffic.

### Phase 2 — Scoped registrar watch
`services` filter on `WatchEndpoints` + broadcaster consumer index + filter
update/reconnect path. E2e: churn fan-out counted per consumer, not per node.

### Phase 3 — Cold path
Spike ODCDS vs fallback-route+observe on the deployed Envoy; pick one; TTL'd
observed entries + miss metric. E2e: undeclared call succeeds, warms, expires.

### Phase 4 — Retire client-side active HC
Outlier detection on service clusters; remove `MeshReadyPath` HC and
`IgnoreNewHostsUntilFirstHc` from `NewServiceCluster`; flip the fallback flag
default to **off**. E2e: full suite incl. roll-under-churn and induced-5xx
ejection; this phase re-runs the #143/#152 drain validations since the health
composition changes.

### Phase 5 — Export budget (optional)
Per-family OTel flush intervals if collector volume warrants it at real scale.

## Metrics and Observability

- `aether.agent.upstreams.{declared,observed,miss}` (gauge/counter)
- `aether.agent.snapshot.clusters` (gauge — the headline number per node)
- `aether.registrar.watch.filtered_subscribers`, `fanout_messages`
- Existing: snapshot build duration, watch reconnects, promotion delay

## Open Questions

1. **ODCDS vs fallback-route for the cold path** — Phase 3 spike decides;
   ODCDS is the cleaner contract but its interaction with the per-source mTLS
   transport-socket matcher and `connection_pool_per_downstream_connection`
   needs proving on the deployed Envoy build.
2. **Dependency TTL** for observed entries (initial: 1h idle).
3. **Namespace-level declarations** (`config.aether.io/upstreams` on the
   namespace as a default-union) — convenience vs blast radius.
4. **Edge proxy** (proposal 003) — the edge tier likely *wants* full
   distribution (it fronts everything); the fallback flag covers it, but the
   edge chart should pin it explicitly.
5. Whether Phase 4's outlier-detection settings can be validated with the
   existing loadgen or need a fault-injecting workload.
