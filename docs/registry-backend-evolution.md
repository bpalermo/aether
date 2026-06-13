# Registry Backend Evolution: from Kubernetes to Cloud Map to DynamoDB to etcd Watch

**Status:** Current backend on talos-main = **etcd with watch-driven sync**.
Supported backends: `kubernetes`, `dynamodb`, `etcd` (Cloud Map removed in #150).
**Date:** 2026-06-13

This document records *why* the registry backend evolved the way it did. The
short version: every backend choice was really a choice about **how endpoint
changes propagate**, and the recurring antagonist was a single failure mode —
the **cross-replica "last-old-exit" skew**. Each backend handled it
differently; etcd's native `Watch` is the first to close it cleanly without
bolting on a second mechanism.

## The constant: what the registry must do

The registrar is the mesh's discovery plane. Two registrar replicas run
active-active (no leader — `WriteBehindQueue.NeedLeaderElection()` is false).
Each replica keeps its own in-memory snapshot, fans out changes to the agents
watching it, and reconciles against a durable backend. The backend's job is
narrow but load-bearing: **be the durable source of truth and the channel by
which one replica (and, later, one cluster) learns another's writes.**

Everything below is a story about that channel.

## The recurring problem: cross-replica last-old-exit skew

Agents reach the registrar through a ClusterIP Service, so an agent's *watch*
lands on one replica while its *write* RPCs may land on the other. The write
path is **snapshot-first**: replica A applies a change and broadcasts to its
own watchers in ~ms, then persists to the backend (write-behind). Replica B
learns of A's write **only through the backend** — so B's watchers lag by
however long backend propagation takes.

During a rolling restart this is masked for most of the roll (surge invariant +
two-phase drain + retries keep a live endpoint visible). It surfaces at the
**last old pod's exit**: for a beat, a B-watching client's visible set can be
all-stale — the just-removed endpoints A already dropped, with the replacements
not yet propagated — so even a retry lands on a dead host. The window lasts
exactly one backend-propagation cycle. **This skew is the throughline that
every backend decision below was really about.**

## 1. Kubernetes backend — the simple default

The original/default backend stores endpoints as Kubernetes objects. It's
fine for development and small clusters and needs no external dependency. It
was never the target for the design scale (3k services / 10k nodes / 100k
pods): per-endpoint churn against the API server, and the propagation channel
is the API server's own watch — adequate, but it couples mesh discovery to
control-plane capacity. It remains supported as the zero-dependency option.

## 2. AWS Cloud Map — managed DNS-SD, then dropped (#150)

Cloud Map was attractive as a *managed* service-discovery store: AWS-native
health status, no datastore to operate. We invested in it — node_ip plumbing
(#89), native DRAINING health (#119), propagating health-update failures and
cluster-scoping the sweep (#121).

It was **dropped (#150, a breaking change)** for a decisive reason: the
**async-register health race**. Cloud Map registers instances asynchronously
and applies its own health evaluation, so a freshly registered endpoint spent
an indeterminate window in an ambiguous health state before becoming routable —
the mesh couldn't get crisp, immediate "this endpoint is serving / draining"
semantics that the two-phase drain and delegated-liveness pipeline depend on.
The control we needed over *when* an endpoint flips state lived inside a
managed service's eventual-consistency model, not in our hands. Managed-ness
wasn't worth surrendering that control. Post-removal cleanup landed in #151.

**Lesson carried forward:** the backend must give the registrar **deterministic,
immediate control over endpoint state transitions** — discovery freshness is a
correctness property here, not a convenience.

## 3. DynamoDB — managed KV with snapshot-first resilience

DynamoDB replaced Cloud Map: a managed key-value store where *we* own the
health/state semantics (the value is our `ServiceEndpoint` proto, written and
interpreted by us). It paired naturally with the registrar's **snapshot-first +
write-behind** design — a failing external write never makes a serving pod
invisible (the rev-68 regression fix) — and its **global tables** are a strong
multi-region, multi-active substrate for the multi-cluster directive (registrar
per cluster, none authoritative, cluster-scoped reconciliation).

talos-main ran on DynamoDB with full e2e green. But the cross-replica skew
remained, because the **propagation channel was a 5-second poll**
(`ListAllEndpoints` on a ticker): replica B learned A's writes up to ~5–6 s
late, producing the last-old-exit residue (~3 dropped requests per service roll
in the e2e).

The natural fix — event-driven CDC — runs into DynamoDB's weak change story:

- **DynamoDB Streams** is poll-based per shard, **per-item ordered only**,
  ~sub-second-to-1 s, and capped at **~2 readers per shard** (AWS-documented
  throttle limit) — which pins a native-Streams registrar to 2 replicas;
  more requires Kinesis Enhanced Fan-Out (≤20 consumers).
- Worse for *this* problem: Streams only carries a change **after** the
  snapshot-first write-behind has flushed it to DynamoDB. So cross-replica via
  Streams stays gated on the very async write the snapshot-first design exists
  to hide — it shrinks the window (~5 s → ~1–2 s) but never closes it.

So DynamoDB is an excellent *durable, multi-region, managed* substrate, but its
CDC is the wrong tool to close the skew. That pointed two ways: a
backend-agnostic fix (**peer-watch**), or a backend with a better native CDC
(**etcd**).

## 4. etcd — native Watch closes the skew

etcd's `Watch` is a materially better change channel than DynamoDB Streams on
every axis that matters here:

| | etcd `Watch` | DynamoDB Streams |
|---|---|---|
| model | push, native | poll shards |
| latency | ~ms | ~sub-s–1 s |
| ordering | **global** (revision) | per-item only |
| concurrent readers | thousands (no cap) | **2/shard** (→ EFO for ≤20) |
| resume | from revision (compaction-bounded) | 24 h trim |
| endpoint | same client | separate Streams endpoint |

We switched talos-main to the etcd backend (validating backend *parity* first —
it still polled), then implemented the watch: the etcd backend now satisfies
`registry.ChangeNotifier` via `clientv3.Watch` over the key prefix, and the
registrar `Syncer` reacts to change signals (200 ms debounce to coalesce roll
bursts) **with the 5 s poll retained as a backstop** for the rare gap during a
watch re-establish (compaction, leader change). DynamoDB is unchanged
(poll-only).

Result: cross-replica propagation drops from ~5 s to sub-second, and the
last-old-exit window collapses.

## Why agents still go through the registrar (not direct to etcd)

A tempting simplification with etcd: let agents read/write etcd directly and
delete the registrar. **No — at scale this is the kubelet→etcd anti-pattern.**
The registrar is the apiserver-equivalent (watch cache + write choke + authz +
resilience). Direct etcd loses:

1. **Watch fan-out coalescing** — 10k nodes × ≤200 dependency prefix-watches ≈
   ~2M watch streams against one etcd. The registrar Watches *once* and fans
   out demand-scoped filtered streams.
2. **Write authz** — every node holding etcd write credentials means registry
   poisoning from any node, which would undo the upstream-SAN-pinning threat
   model.
3. Snapshot-first/write-behind resilience, backend pluggability, and
   server-side demand-scoping + the service catalog.

So the etcd `Watch` win is captured **at the registrar↔etcd link** (registrar
as a watch cache over etcd), not at an agent↔etcd link.

## Multi-region: etcd, not DynamoDB global tables

Intra-region/intra-cluster, the skew fix is local (the Watch above). The
multi-region question used to point at DynamoDB global tables — but
**a managed multi-region store is not worth adopting *just* for global tables**,
and a per-region-etcd topology dissolves the reasons etcd looked unfit for
multi-region in the first place.

**Directive (2026-06-13): keep etcd for multi-region.** The model is **a
per-region etcd cluster plus asynchronous cross-region replication, with
eventual consistency between regions accepted.** Per-region quorums remove the
three objections to a single shared etcd:

- **Region locality** — each region writes to its local etcd at local RTT; no
  cross-region Raft penalty.
- **Blast radius** — per-region quorums; one region's etcd failure is isolated.
- **CP partition behavior** — a region cut off from the others keeps its local
  quorum, so it keeps registering and serving its own endpoints; only
  cross-region propagation pauses, which is the accepted eventual-consistency
  trade.

### How the cross-region replication stays conflict-free

The mesh's existing discipline — *registrar per cluster, none authoritative,
cluster-scoped reconciliation, no unilateral GC* — is exactly what makes
multi-master replication conflict-free: **each region is authoritative only for
its own `cluster_name` endpoints.** Encode that in the key so the partitions
are truly disjoint regardless of pod-CIDR overlap across regions:

```
# prerequisite schema change — origin (cluster) goes IN the key, not just the value
/aether/services/<service>/clusters/<cluster>/endpoints/<ip>
```

Each region authoritatively writes only `clusters/<self>`; peers' endpoints
arrive mirrored into `clusters/<peer>`. Disjoint partitions ⇒ one writer per
partition ⇒ last-write-wins is a no-op ⇒ eventual consistency is trivial.

### The replication mechanism

```
Region A: agents → registrar(A) → etcd(A) ──own-prefix mirror──▶ etcd(B), etcd(C)
Region B: agents → registrar(B) → etcd(B) ──own-prefix mirror──▶ etcd(A), etcd(C)
```

- The registrar stays **region-local** and store-shaped: it Watches only its
  local etcd, which now contains local-authoritative *plus* mirrored-foreign
  endpoints. It never knows about other regions.
- A per-region **replicator** watches its own `clusters/<self>` prefix and
  replays changes into peer regions' etcd. This is etcd↔etcd replication, *not*
  registrar-to-registrar federation — deliberately, to avoid cross-trust-domain
  mTLS (SPIFFE federation) and a WAN gossip mesh.
- **Self-healing without unilateral GC:** mirrored keys carry an etcd **lease**
  the replicator refreshes. If a region or the inter-region link dies, its
  foreign keys elsewhere expire on their own — the *origin's* heartbeat
  lapsing, not a peer judging it dead — giving automatic cross-region failover
  cleanup that honors the no-GC directive.
- **Locality keeps it safe:** foreign-region endpoints are priority 2 (failover
  only), so the accepted cross-region staleness rides the non-critical path; the
  steady path is always local.

`etcdctl make-mirror` fits the one-directional-per-origin-prefix shape as a
starting point but lacks lease management and origin-filtering, so the
replicator is a thin purpose-built component (or a registrar mode). This is
scoped as **proposal 006 (multi-region etcd federation)**: the key-schema
change, the replicator spec (lease/tombstone/resume/compaction-resync, HA), and
a region-failover e2e — not yet built.

**Net:** etcd is the substrate single- *and* multi-region. DynamoDB global
tables are an alternative, not a requirement.

## Empirical validation

Identical disruption battery on each — baseline → svc-1/4/5 rolls → proxy
hot-restart → agent roll → simultaneous svc-2+svc-3 churn → settle — over five
data paths including a multi-port service (h1 on 8080, h2c on 3001), ~100k+
requests per run:

| Backend / mode | Drops (in-window) |
|---|---|
| DynamoDB (poll) | 3 — all in the svc-5 roll window |
| etcd (poll) | 1 — svc-5 roll window (parity) |
| **etcd (watch)** | **0 — last-old-exit skew closed** |

Proxy hot-restart and agent roll were **hitless on all three**. The only drops
the poll backends produced were the last-old-exit residue, and watch-speed
cross-replica propagation eliminated even those.

## Where this leaves us

- **Backend = a propagation-channel decision.** Cloud Map failed on *control*
  (async health race), DynamoDB succeeds on *durability/managed/multi-region*
  but its CDC can't close the skew, etcd's native `Watch` closes it directly.
- **Current:** talos-main on etcd + watch (poll backstop); DynamoDB remains a
  fully-supported, e2e-validated managed alternative; Kubernetes is the
  zero-dependency option.
- **Guidance:** **etcd is the chosen substrate single- and multi-region** —
  single-region/intra-cluster via `Watch`; multi-region via per-region etcd +
  asynchronous, origin-partitioned, lease-managed cross-region replication
  (eventual consistency cross-region, accepted). DynamoDB is supported but is
  *not* required by the multi-region case — we do not adopt a managed store
  solely for global tables. Agents always go through the registrar, never the
  store directly.
- **Open items:** proposal 006 (multi-region etcd federation — key-schema
  change + replicator); and `peer-watch` (registrar↔registrar) as the
  backend-agnostic intra-cluster skew close — lower priority for etcd (Watch
  already gets it to zero), relevant only if DynamoDB is the production
  substrate.
