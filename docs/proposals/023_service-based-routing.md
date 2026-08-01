# Proposal: Service-based routing — decoupling the GAMMA route target from ServiceAccount identity

**Status:** Design — 2026-06-28
**Relates:** proposal 018 (Gateway API/GAMMA), proposal 020 (namespace-aware services
— the `<ns>/<svc>` registry key + SA-identity model this refines), proposal 022
(arbitrary-Service interception — captures the *real* Service ClusterIP:port this
builds the routing layer on top of), proposal 004 (demand-scoped distribution);
the MESH-HTTP conformance investigation (2026-06-28);
[[project_m3_mesh_http_conformance]], [[project_gateway_api_gamma]],
[[project_020_m1_progress]], [[project_m2a_gate_result]].

## Summary

aether identifies a service by its **ServiceAccount**: the registry key is `<ns>/<sa>`,
the mTLS SVID is `spiffe://<td>/ns/<ns>/sa/<sa>`, and a routable "service" exists only
where SA-backed pods exist. Gateway API GAMMA, by contrast, attaches an HTTPRoute to a
**k8s Service** (the *route target*) and forwards to **backend Services** — and those
need not coincide with a single ServiceAccount. The two models collide at one point: a
route-target Service that fronts *versioned* backends (the canonical GAMMA shape: an
`echo` Service routed by path/weight to `echo-v1`/`echo-v2`) cannot be represented,
because distinguishing the backends requires distinct SAs (`echo-v1`, `echo-v2`) and the
route-target `echo` then maps to *two* SAs at once — but a pod has exactly one.

This proposes **decoupling the route target from identity**: a k8s Service is a
*routing handle* — captured by its real `ClusterIP:port` (proposal 022), carrying its
attached GAMMA route table — while **identity stays the ServiceAccount** of the
**backend** workloads it forwards to (mTLS/SAN unchanged). The route target needs no
SA-backed pods of its own. This is the missing layer that makes east-west GAMMA — and
the upstream MESH-HTTP conformance profile — actually representable.

## Motivation

- **The MESH-HTTP conformance blocker (2026-06-28).** Re-running the suite after the 020
  cutover + 022 M2 (redirect-all) + namespace auto-injection (#401) confirmed two things:
  (1) the suite never exercised aether until injection (unlabeled pods → CNI ignores them);
  (2) even meshed, the route target `echo` (`selector: app=echo`, fronting `echo-v1`/`echo-v2`)
  has no SA of its own. With SA-identity, `<ns>/echo` has no backing pods → no registry
  endpoints → no mesh Service → no `cap_http` vhost, so a client dialing `echo` finds no
  GAMMA route. The route precedence + segment-prefix bugs found alongside this (#400,
  shipped) are real and fixed, but they fire only once the route target is reachable.
- **GAMMA's data model is Service-centric, aether's is SA-centric.** This is fine for
  identity (SAN must be the *workload* identity = SA) but wrong for *routing*: the thing a
  client addresses (`echo`) and the thing it ends up talking to (`echo-v1` pods) are
  deliberately different objects in Gateway API. aether already half-knows this — the GAMMA
  reconciler keys routes by the parentRef Service `<ns>/<name>` and resolves backendRefs to
  `<ns>/<backend>` clusters (proposal 020 + the namespace-qualified GAMMA work). What is
  missing is a route target that exists **without** being an SA-backed service.
- **022 captures the real Service; 023 routes it.** Proposal 022 makes aether intercept the
  real `echo` `ClusterIP:port`. This proposal defines what happens *after* capture for a
  route-target Service: which route table it hits, how its GAMMA rules apply, and how its
  backendRefs resolve to SA-backed clusters for mTLS.

## The shift this represents (state it plainly)

Today, "service" is one concept in aether: an SA, with an identity, endpoints, a cluster,
and a name. This proposal splits it into two:

| Concept | Keyed by | Has | Used for |
|---|---|---|---|
| **Route target** (NEW) | k8s Service `<ns>/<name>` | ClusterIP(s)+ports, an attached GAMMA route table, backendRefs | the destination a client addresses; capture + L7 routing |
| **Backend service** (today) | ServiceAccount `<ns>/<sa>` | endpoints, an EDS cluster, a SPIFFE SVID | identity, mTLS, load balancing |

A route target *may* also be a backend service (the common case: an `echo` Service whose
pods share SA `echo` is both — it routes to itself). The versioned-fanout case is where they
diverge: `echo` is a pure route target; `echo-v1`/`echo-v2` are backend services.

## Design

### Route-target catalog
Introduce a **route-target** entry, distinct from the registry's SA-keyed endpoints. Sources,
in precedence:
1. A GAMMA HTTPRoute/GRPCRoute whose parentRef is a k8s Service → that Service is a route
   target keyed `<ns>/<name>`, carrying the route's rules.
2. (Optional, demand-scoped) any in-mesh k8s Service captured per 022, with an identity route
   (pass-through to its own SA-backed cluster) when no GAMMA route is attached.

A route target carries: its real `ClusterIP`(s) + ports (for capture demux, proposal 022),
and the ordered GAMMA rule set (already built by the reconciler — specificity-sorted,
segment-boundary prefixes, header/redirect/rewrite/weight, per #400).

### Capture + route table
- **Capture** (proposal 022): the per-pod nftables/redirect set includes the route target's
  real `ClusterIP:port`, so a client dialing `echo:80` lands on the capture listener with the
  original dst preserved.
- **`cap_http` route table**: gains a vhost per route target, keyed by the route target's
  authorities (`<name>.<ns>.svc.cluster.local[:port]` and the `<name>.<ns>.<meshDomain>` mesh
  name), carrying its GAMMA rules. This is the SAME `BuildOutboundServiceVirtualHost` builder
  used today — the only change is that the vhost is sourced from the **route-target catalog**
  (which does not require SA-backed pods) rather than only from generated SA mesh Services.
- **Backend resolution**: each rule's backendRef resolves to the SA-backed cluster
  `<ns>/<backend>` (existing path). mTLS to that cluster pins the backend's SAN
  `spiffe://<td>/ns/<ns>/sa/<backend>` — **identity is unchanged and stays SA-based**.

### Identity is unaffected
The route target is a routing handle with **no SVID** — nothing presents it, nothing
validates it. mTLS happens on the hop into the *backend* cluster, exactly as today. This is
the key invariant: decoupling routing from identity does **not** weaken the per-source mTLS
model (proposal 020's SAN work, the `sa/<bare-name>` fix) — it only adds a routing layer in
front of it.

### Demand scope
A route target joins the node dependency set when a local pod declares it (or via ODCDS cold
path), same as a service today; its backendRefs are unioned in (the existing
`routeBackendsLocked` path, now namespace-qualified). No new scope mechanism.

## Tensions / non-goals

- **Selectorless route targets.** A pure route target (`echo` fronting only versioned
  backends) may still have a selector that matches the backend pods; aether ignores the
  selector and routes by the attached GAMMA rules. A route target with no GAMMA route and no
  SA pods is a no-op (nothing to route to) — not an error.
- **Not multi-SA identity.** This does **not** give a route target an identity or let one
  workload hold multiple SAs. The fanout is expressed as routing (target → backend Services),
  never as identity.
- **Overlap with a same-named backend.** When a Service is both a route target and an
  SA-backed service (`echo` with SA `echo` pods), the GAMMA route table wins for its
  authority; absent a route, it falls through to its own identity cluster. Deterministic, no
  collision.
- **kube-proxy coexistence.** Until 022 redirect-all/real-port capture is the default, a route
  target is only reached on the captured path; uncaptured port traffic still rides kube-proxy
  (the conformance "illusory passes" caveat from 022 applies).

## Verification

- Unit: route-target catalog assembles from GAMMA parentRefs; `cap_http` vhost built for a
  route target with no SA pods; backendRef resolves to the SA-backed cluster + correct SAN.
  Offline `envoy --mode validate` (proposal 011 / `//test/envoy_validate`) over the generated
  capture config.
- e2e on talos: the **MESH-HTTP conformance** profile run with namespace injection (#401) +
  per-version SAs on the backends — `MeshHTTPRouteMatching`, `…Weight`,
  `…RequestHeaderModifier`, `…RedirectHostAndStatus` exercise the route-target → backend path
  for real (no kube-proxy coincidence). Target: a *meaningful* MESH-HTTP score.

## Sequencing

1. **Prerequisites (shipped):** 020 Part 1 (namespace-aware keys), 022 M2 (redirect-all +
   `original_dst` passthrough), namespace auto-injection (#401), the GAMMA precedence +
   segment-prefix fixes (#400).
2. **M1 — route-target catalog + `cap_http` sourcing.** Build the catalog from GAMMA
   parentRefs; source capture vhosts from it. (No identity change.)
3. **M2 — real-`ClusterIP:port` capture for route targets** (folds in proposal 022 Option B):
   demux the route target's real ClusterIP/ports into the capture listener.
4. **M3 — conformance.** Patch the (uncommitted) MESH runner for per-version SAs + the
   `aether.io/managed` namespace label; re-run to a meaningful score; close the gaps.

## Alternative considered (and rejected)

**Make every route target an SA-backed service** (synthesize an `echo` SA + pods, or relabel).
Rejected: it distorts the workload topology to satisfy a routing concern, can't express the
versioned fanout without colliding (echo-v1/echo-v2 under one SA), and conflates identity with
routing — the exact coupling this proposal removes.
