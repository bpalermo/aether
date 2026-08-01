# Proposal: Namespace-aware services + opt-in non-mesh endpoint projection

**Status:** Design — 2026-06-25
**Relates:** proposal 006 (origin-partitioned registry key schema — the migration
pattern this follows), proposal 018 (Gateway API/GAMMA + MCS — the alignment driver),
proposal 019 (multi-cluster waypoint — bounds cross-cluster non-mesh reachability),
proposal 004 (demand-scoped distribution), proposal 005 (multi-port);
[[project_multicluster_registry]], [[project_gateway_api_gamma]], [[project_demand_scoped]].

## Summary

Two alignments with the Kubernetes Service / MCS model, kept strictly separate from
endpoint **management** (the CNI + registry stay the source of truth):

1. **Namespace-aware services.** Key the registry `<ns>/<svc>`, name clusters
   `<svc>.<ns>.<meshDomain>`, and honor `<svc>.<ns>.svc.cluster.local` on the capture
   path. This is a **correctness fix** (same-named ServiceAccounts across namespaces
   collide today), and the foundation for MCS and Mesh-profile conformance.
2. **Opt-in EndpointSlice projection.** For services that opt in, project the
   registry's *local* endpoints into EndpointSlices so **non-mesh** consumers reach
   them over standard Kubernetes networking — the registry stays authoritative; the
   slices are a read-only *output*, never the input.

Explicit non-goal: **do not** adopt EndpointSlices as the endpoint-management plane —
that would forfeit cross-cluster endpoints, mesh health, two-phase drain,
demand-scoping, and CNI registration. Slices remain a projection.

## Motivation

- **Namespace collision (latent bug).** The service identity is the *ServiceAccount*
  and the registry key + cluster name are `<svc>.<meshDomain>` — **namespace-free**. Two
  workloads with the same SA name in different namespaces (e.g. the ubiquitous
  `default` SA) collapse into one `<sa>.<meshDomain>` cluster with merged endpoints and
  merged SAN namespaces. This is wrong, not cosmetic.
- **MCS.** `ServiceExport`/`ServiceImport` are name+namespace; aether's namespace-free
  model has no clean answer for which namespace an import lands in or how two same-named
  services across namespaces export distinctly.
- **Conformance.** The Mesh profile intercepts the Service's *standard* identities
  (`<svc>.<ns>.svc.cluster.local`, ClusterIP); aether routes on `<svc>.<meshDomain>`.
- **Mixed clusters.** A non-mesh consumer (an un-meshed pod, kube-proxy, an external
  client) cannot reach a mesh service today — the generated mesh Service is selectorless
  with no endpoints. Incremental adoption requires non-mesh → mesh to work.
- **SPIFFE is already namespace-scoped** (`spiffe://<td>/ns/<ns>/sa/<sa>`). The security
  identity is fine; only the registry key and cluster name drop the namespace. The fix
  is additive to identity, breaking only to the key schema.

## Design

### Part 1 — Namespace-aware services (foundational)

- **Registry key:** `<ns>/<svc>`, origin-partitioned per proposal 006
  (`/aether/v1/regions/<r>/clusters/<c>/ns/<ns>/services/<svc>/endpoints/...`).
- **Cluster name / mesh FQDN:** `<svc>.<ns>.<meshDomain>`; the capture path additionally
  honors `<svc>.<ns>.svc.cluster.local` (the standard name) and the existing
  `cluster.local` authority, so both the mesh-global and standard FQDNs route.
- **SAN pinning:** unchanged — already `ns/<ns>/sa/<svc>`. The namespace just stops
  being derived from a union and becomes the key.
- **mesh-DNS + capture authorities + MCS:** namespace-qualified throughout; MCS export
  of `<ns>/<svc>` materializes a `ServiceImport` + clusterset VIP **in `<ns>`**.
- **Migration:** breaking key-schema change. Follow proposal 006's pattern — versioned
  keys + dual-read during rollout; the CNI re-registers each pod under its
  namespace-qualified key on the next ADD/restart. The mesh Service objects (generated)
  re-key to `<svc>.<ns>`.

### Part 2 — Opt-in EndpointSlice projection (non-mesh reachability)

- **Opt-in, default OFF** (per-service annotation, e.g. `aether.io/expose-endpoints:
  "true"`) — preserves zero-trust by default.

**The registry is the single source of truth — the Service is selectorless, and the
slices are a projection, NOT a selector.** This is the crux. aether membership is
*identity-based* (the CNI registers a pod by `aether.io/managed=true` + its
ServiceAccount → the service key `<ns>/<svc>`); a Kubernetes Service `selector` is
*label-based*. Those are two different definitions of "which pods are endpoints," and if
both exist they **drift**: a pod that matches the selector but isn't registered yet, or a
registered pod missing the selector label, lands in one set and not the other — so
non-mesh consumers (kube-proxy → slices) and mesh consumers (capture → registry EDS) would
see **different endpoint sets** for the same Service. Avoid that split-brain by having
**one** source:

- aether **projects** the registry's *local* endpoints into `EndpointSlice`s owned by the
  generated (selectorless) mesh Service — a read-only output the registrar/agent writes
  from the registry. There is nothing to "align," because the slices *are* the registry's
  local view by construction.
- **Project mesh health into the slice conditions.** The slice `Ready`/`Terminating`
  conditions carry the registry's health (CNI liveness + the two-phase drain), so a pod
  the mesh is draining stops receiving non-mesh traffic too. This is *more* correct than a
  raw selector, which only sees k8s readiness and would keep steering non-mesh traffic at
  a draining pod.

- **Coexistence on one ClusterIP** — no collision, because the capture redirect only
  exists in *meshed* pod netns:
  - mesh pod → CNI capture-redirect → proxy → registry EDS (mTLS, cross-cluster,
    demand-scoped);
  - non-mesh pod / kube-proxy → projected `EndpointSlice` → local pod.

- **Caveats (why opt-in, not a blanket must):**
  - **Local endpoints only.** Cross-cluster non-mesh reachability is bounded by the
    proposal 019 connectivity mode (waypoint / flat network); projection covers
    in-cluster pods.
  - **The app must bind the pod IP**, not loopback — meshed apps that listen only on
    `127.0.0.1` (inbound proxy forwards) won't accept a direct connection.
  - It is a **deliberate mTLS/identity bypass** for that path (the non-mesh client skips
    the inbound `:15008` handshake). Per-service opt-in, never default.

**Discouraged alternative — a generated selector.** If standard-k8s ergonomics (letting
the EndpointSlice controller build the slices, `kubectl get endpoints`, third-party
controllers) are wanted, a selector is acceptable *only if aether generates it from the
exact same membership* — never user-authored. The registrar/injecting webhook stamps a
per-service label mirroring the registration (e.g. `aether.io/service: <svc>`) on
registered pods, and the mesh Service selector is `{aether.io/managed: "true",
aether.io/service: "<svc>"}`, so the two align *because the same membership produces
both*. The trade-offs vs the projection: it adds a per-service label that must stay in
sync (a new drift surface), and it hands local-endpoint health back to k8s readiness,
losing the mesh's drain/liveness signal on that path. **A user-authored selector that is
"supposed to match" the registry is the trap — don't allow it.**

### Part 3 — Service-from-ServiceAccount decoupling (deferred)

aether conflates **Service == ServiceAccount**. In Kubernetes a Service selects pods by
label and may front pods of *different* SAs; MCS exports *Services*, not SAs. Full
alignment makes the **routing target** the k8s Service (name+namespace) and endpoints the
pods carrying their **own** SA identity. This is only needed for multi-SA Services / full
MCS Service semantics; the common 1-workload-1-SA case is served by Part 1 alone. Deferred
behind Parts 1–2.

## Sequencing — after the in-flight conformance/feature work

Part 1 touches the **registry key schema, cluster naming, capture authorities, and
mesh-DNS** — the same surfaces the current in-flight work sits near (the gRPC
RegularExpression + header-modifier route-builder changes, and the conformance baseline
run). To avoid merge conflicts, this proposal's implementation is **sequenced after those
land and merge**. Then:

1. **Part 1** (ns-key + FQDN) — foundational, breaking; migrate like proposal 006.
2. **Part 2** (EndpointSlice projection) — opt-in, layered on Part 1.
3. **Part 3** (Service-from-SA) — only if multi-SA Services are required.

## Non-goals / tensions

- **EndpointSlices are never the management source** — only a projected output.
- **Cross-cluster non-mesh reachability** is out of scope (bounded by proposal 019).
- The **mTLS bypass** for projected endpoints is a deliberate, opt-in trade-off, not a
  default — the mesh stays zero-trust unless a service opts in.
- The **Service-from-SA** decoupling is deferred; most workloads are 1-SA.
- Breaking the registry key schema is disruptive; gate it behind the 006-style dual-read
  migration and a cluster-wide CNI re-registration window.

## Verification

- **ns-collision:** deploy two services with SA `default` (or any shared name) in
  different namespaces → distinct `<svc>.<ns>.<meshDomain>` clusters + separate EDS
  (today: one merged cluster).
- **Standard FQDN:** a mesh client dialing `<svc>.<ns>.svc.cluster.local` is captured and
  routed identically to the mesh-global name.
- **MCS:** `ServiceExport` of `<ns>/<svc>` → `ServiceImport` + clusterset VIP **in
  `<ns>`**; two same-named services in different namespaces export distinctly.
- **Projection (opt-in):** annotate a service → an `EndpointSlice` appears for the mesh
  Service; a **non-mesh** pod curls the ClusterIP and reaches a local pod (no mesh hop);
  a **mesh** pod still traverses capture + mTLS; flipping the annotation off removes the
  slice and the non-mesh path.
