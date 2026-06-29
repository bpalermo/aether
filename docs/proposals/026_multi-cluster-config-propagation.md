# Proposal: Multi-cluster config propagation

**Status:** Accepted — 2026-06-29. **Decision: Option E (control cluster — centralized config
authority)**, implemented on Option C's export/import channel (the hub is the sole exporter; spokes
import read-only, no cross-cluster K8s credentials); Option D (producer-waypoint) for class-1
*enforcement*; Option A (GitOps) for class-2 consumer-local config; Option B rejected. Run the
control cluster as a pure-management cluster (no mesh workloads). This decides the authority
topology; the export schema + materializer are the implementation follow-up.
**Relates:** proposal 006 (multi-region etcd federation — the registry bus), proposal 019
(multicluster node-waypoint — the producer-side enforcement option), proposal 018 (Gateway
API/GAMMA — the config that needs to propagate), proposal 023 (route-by-Service), proposal 015
(MeshConfig CRD), proposal 017 (VirtualHost CRD), proposal 025 (proxy-extension escape hatch —
the surface that surfaced this gap), the MCS ServiceExport/Import work.
[[project_multicluster_registry]], [[project_registrar_etcd_vs_ddb]], [[project_gateway_api_gamma]].

## Summary

aether federates the **registry** across clusters — `ServiceExport` records a service into the
shared registry, endpoints stay in the registry, and proxies resolve cross-cluster endpoints from
it (origin-partitioned region-scoped etcd, proposal 006; or shared Cloud Map/DDB). It does **not**
federate **config**: HTTPRoute, GRPCRoute, MeshConfig, VirtualHost (and the proposed HTTPFilter)
are Kubernetes CRDs read from *each cluster's own* API server, and GAMMA/edge config is applied
**consumer-side** — at the calling pod's egress proxy (the demand-scoped `cap_http` route table).

The consequence: the same logical service can have **divergent routing/policy in different
clusters**, and a policy "attached to a Service" is actually enforced *per consumer cluster*, not
once near the service. As the mesh goes multi-cluster (proposals 006/019), this is a foundational
gap — config drift across clusters is silent and, for an opaque escape hatch (025), dangerous.
This proposal defines **what config must be consistent mesh-wide, who is authoritative, and how
it propagates** — without violating the standing directive that no cluster's registrar is
authoritative.

## Motivation

- **Registry is federated, config is not.** `registrar/internal/mcs/export_controller.go` is
  explicit: "REGISTRY — not cross-cluster DNS or EndpointSlice import — is aether's model." The
  GAMMA reconciler "watches HTTPRoutes cluster-wide" = the *local* cluster only. No remote config
  read exists.
- **Config is applied consumer-side.** A request from a client in cluster A to service S applies
  **A's** GAMMA config; the same request from cluster B applies **B's**. So behavior depends on
  *where the caller lives*, and a Service-scoped policy cannot be guaranteed for all callers.
- **It's pressing now.** Multi-cluster registry (006) + node-waypoint (019) are landing; route-by-
  Service (023) makes Services first-class routing targets; the escape hatch (025) makes the drift
  opaque and powerful. The registry half is done; the config half is undefined.
- **Don't reinvent GitOps.** Operators already replicate manifests with Argo/Flux. The question is
  not "replace GitOps" but "which config genuinely needs *mesh-native* consistency, and which is
  fine left to GitOps + the existing federation primitives."

## Background: the two planes today

| Plane | Federated? | Mechanism | Applied where |
|---|---|---|---|
| Registry (services, endpoints) | **Yes** | MCS `ServiceExport` → shared registry (etcd 006 / Cloud Map / DDB); `ServiceImport` materializes `ClusterSetIP` | proxy resolves cross-cluster endpoints from registry |
| Config (HTTPRoute, GRPCRoute, MeshConfig, VirtualHost, HTTPFilter) | **No** | per-cluster K8s API; GAMMA/edge reconcilers read local cluster | consumer egress (`cap_http`) / edge listener |

Directives this must respect ([[project_multicluster_registry]]): registrar **per cluster, none
authoritative**; instance scope (a cluster mutates only its own endpoints); **no unilateral GC**;
cross-trust-domain mTLS (SPIFFE federation, trust-domain = mesh-domain per cluster).

## The key distinction: which config even needs to propagate

Not all config does. Split it:

1. **Service-authoritative (producer-owned) config** — routing/policy that should hold for a
   service **regardless of which cluster calls it** (the canonical "attach an HTTPRoute/policy to
   Service S" intent). This is the class that genuinely needs mesh-wide consistency.
2. **Consumer-local config** — a cluster's own client-side preferences (a local routing override,
   caller-side telemetry like `header_to_metadata` for the caller's own pipeline). This **should
   not** propagate; it is correctly cluster-scoped.

Most of the hard problem is class 1. The cleanest sub-question is *whether class-1 config should
be propagated to every consumer, or enforced once near the producer so it never needs to.*

## Options

> The part to review. They are not mutually exclusive — the recommendation combines them.

### Option A — GitOps only (status quo, operator-owned)
Declare config federation out of mesh scope: operators replicate CRDs to all clusters (Argo/Flux).

- **+** Zero mesh machinery; matches "no authoritative cluster"; works today.
- **−** No consistency *guarantee* — drift is invisible to the mesh; a missed/edited replica
  silently changes behavior for that cluster's callers. No producer-authority model. Unacceptable
  *alone* for class-1 policy, but the right baseline for class-2.

### Option B — Config CDC on the registry bus
Piggyback config CRDs onto the cross-cluster registry store (etcd Watch / DDB Streams) alongside
endpoint CDC.

- **+** One bus; reuses 006; eventual-consistent like the registry.
- **−** Couples K8s-native config to a non-K8s store; needs config conflict resolution + ordering +
  cross-trust-domain auth; turns the registrar into a config controller. Heavy; blurs the
  registry/config separation. I'd reject this as the primary mechanism.

### Option C — MCS-coupled config export (producer-authoritative)
Extend the `ServiceExport` model: when a Service is exported, its **attached** route/policy config
is exported *with it* into the registry, scoped to that service and stamped with the exporting
(owning) cluster as authority. Consuming clusters **import** it (read-only) and materialize it
into their consumer-side data-plane config — the way `ServiceImport` already materializes the
`ClusterSetIP`.

- **+** Rides the federation + producer-authority model aether **already has**; bounded to exported
  services (opt-in, like MCS); conflict resolution is trivial (the owning cluster wins); the
  import is read-only so no cross-cluster write authority is created.
- **−** Still consumer-side application (each consumer materializes + applies), so it is propagation,
  not single-point enforcement; only as consistent as the import lag; needs a config schema in the
  registry (or a config-export CRD) and a materializer in the consumer.

### Option D — Producer-side waypoint enforcement (proposal 019)
For class-1 *enforcement*, don't propagate at all: enforce the service's policy at a **producer-side
waypoint** near the service, so it applies to every caller regardless of cluster.

- **+** Service-global policy holds by construction — no propagation, no drift, no per-consumer copy.
  The architecturally correct home for "must hold for all callers."
- **−** Adds a producer hop (latency/cost) and depends on 019 landing; not every policy is
  enforceable producer-side (caller-side routing/telemetry inherently isn't); a shift from the
  pure consumer-side capture model.

### Option E — Control cluster (centralized config authority)
A designated **control cluster** is the single source of truth for class-1 config; everything else
imports it read-only. This is the *centralized* end of the **authority axis** whose *federated*
end is Option C — it is **not a new mechanism**: the hub is simply the sole *exporter* on Option
C's export/import channel, so spokes import read-only with **no cross-cluster Kubernetes
credentials** (avoid the "hub writes into every spoke's API server" model — credential sprawl +
huge blast radius). Best run as a **pure management cluster** (no mesh workloads): owning no
services, it has no producer-authority of its own, so it is purely a config distribution point.

|  | **C — federated producer-authority** | **E — control cluster (centralized)** |
|---|---|---|
| Who writes config | each cluster, for its own services | one hub, for everything |
| Conflict resolution | producer wins (per service) | trivial — one writer |
| Drift | eventual; possible between exports | **none by construction** (single SoT) |
| Availability | no SPOF | hub down → config frozen fleet-wide (spokes serve last-known, AP) |
| Authority vs ownership | aligned (owner = author) | decoupled (mitigated by a pure-mgmt hub) |
| Ops fit | independent / multi-tenant clusters | single central-ops fleet |

- **+** Single source of truth → **no drift by construction**; GitOps to *one* place + mesh-native
  fan-out; trivial single-writer conflict model; manage a service's routing from one place
  regardless of where its pods run.
- **−** Config **SPOF / blast radius** — hub down or partitioned freezes config changes fleet-wide
  (mitigated: spokes serve last-known, AP); a high-value security target (compromise = mesh-wide
  config compromise); centralizes the config RBAC surface. Does **not** centralize the *registry*
  (see Tensions — the "no authoritative cluster" directive is about endpoints, a different plane).

**Decision (2026-06-29): Option E — control cluster (centralized config authority), on C's
mechanism; D for enforcement; A for class-2; B rejected.**
- **Authority topology = E (control cluster).** One cluster is the single source of truth for
  class-1 config — no drift by construction, trivial single-writer conflict model, manage routing
  from one place. Run it as a **pure-management cluster** (no mesh workloads). The federated-producer
  variant (C) is *not* the chosen topology, but its export/import channel IS the mechanism E is
  built on (the hub is simply the sole authorized exporter) — so the channel is built once and the
  topology is a policy over it (who may export), leaving the door open to federated authority later
  without re-plumbing.
- **Class-1 enforcement** (must hold for all callers) → **D** (producer waypoint): no propagation,
  no drift, independent of the authority topology.
- **Class-1 config evaluated consumer-side** (e.g. client routing that should match mesh-wide) →
  the C/E channel.
- **Class-2 consumer-local** → **A** (GitOps / local CRDs; deliberately not propagated).
- **Reject B** as the primary mechanism (don't make the registrar a config bus).

## Tensions / non-goals

- **Authority & conflict.** Config needs an owner. Two topologies (see Options C/E): **federated**
  — the **exporting (producer) cluster** is authoritative for its own exported service's config
  (matches ServiceExport ownership; two clusters exporting the same ClusterSet service is the
  existing MCS conflict case — reuse its resolution); or **centralized** — a **control cluster** is
  the sole authority (one writer, no conflict). Either way a consumer never writes back. The
  topology is a deployment choice over one mechanism, not a fork.
- **This does NOT centralize the registry.** The standing directive "registrar per cluster, none
  authoritative" is about the **registry** plane (a cluster owns its own pods' endpoints) — it
  stays per-cluster under every option here, including the Option E control cluster, which is a
  **config** authority only. A pure-management control cluster owns no services and no endpoints.
- **Control-cluster availability (Option E).** A hub is a config SPOF: while it is down or
  partitioned, no config *changes* propagate. Spokes must serve last-known config (AP) so the data
  plane never hard-fails on a hub outage — the same posture as a partitioned registry consumer.
  The hub is also a high-value target (config-compromise blast radius is fleet-wide); gate it with
  strong RBAC and the cross-trust-domain auth below.
- **Trust.** Config crossing clusters crosses trust domains (SPIFFE federation, trust-domain =
  mesh-domain per cluster). Imported config must be authenticated to its origin cluster, same as
  the registry bus already requires for endpoints.
- **Partition tolerance.** Eventual consistency, AP like the registry: a partitioned consumer
  serves last-known imported config (never hard-fails), consistent with the no-GC / locality-P2
  posture.
- **Don't replace GitOps.** Class-2 and cluster bootstrap stay GitOps. This proposal federates only
  the producer-authoritative slice that the mesh model says should be consistent.
- **K8s-native vs registry-native.** Option C stores a config projection in the registry, not the
  CRD itself; the CRD stays K8s-native in the owning cluster. Avoid turning arbitrary CRDs into
  registry objects.
- **Scope.** This is east-west service config (GAMMA/policy). Edge (north-south) Gateways are
  per-cluster ingress and out of scope.

## Interaction with proposal 025 (escape hatch)

025 is **blocked on this for its Service-`targetRef` (policy-attachment) form**: a service-global
escape-hatch policy cannot be consistent multi-cluster without C or D. Until then, 025 is scoped to
**caller-side route `ExtensionRef`** (consumer-cluster-local, class-2, GitOps-replicated) — which
needs nothing from this proposal. When this lands, the escape hatch's Service-scoped form becomes a
class-1 payload that rides the C/E config channel (export the filter with the service, under either
authority topology) or D (enforce it at the waypoint).

## Verification

- A class-1 HTTPRoute attached to an exported Service in cluster A is observed (C) or enforced (D)
  for a caller in cluster B — same routing decision as a caller in A.
- A class-2 local override in cluster B does **not** leak to cluster A.
- Partition: cluster B keeps serving last-known imported config when cut off from A; no hard fail.
- Conflict: two clusters exporting the same ClusterSet service resolve deterministically (producer
  authority), no flapping.

## Sequencing

1. **Taxonomy + decision:** ratify class-1 vs class-2 and the "one mechanism (C), selectable
   authority topology (C/E), D for enforcement, A for class-2" split. (This doc.)
2. **C — MCS-coupled config export (read path first):** export an exported Service's attached GAMMA
   config into the registry (origin-stamped); consumer materializer applies it read-only. Start
   with HTTPRoute/GRPCRoute; MeshConfig/VirtualHost/HTTPFilter follow the same channel. This is the
   one mechanism both authority topologies use.
3. **E — control-cluster topology (config only):** the same export path with the hub as sole
   exporter; a deployment flag picks federated-producer vs central-hub authority. No new
   data-plane code — purely *who is allowed to export*. (A pure-management hub runs no workloads.)
4. **D — producer waypoint enforcement:** as proposal 019 lands, route class-1 *enforcement* policy
   to the waypoint so it needs no per-consumer copy.
5. **Drift visibility for class-2/GitOps:** a status/metric surfacing per-cluster config divergence
   for a ClusterSet service, so operator-owned replication drift is at least observable.

Independent of the in-flight data-plane work; this is the cross-cluster control-plane story the
registry federation (006) and waypoint (019) imply but never specified for config.
