# Proposal: Gateway API for aether — north-south + GAMMA east-west

**Status:** Design — 2026-06-22
**Relates:** proposal 017 (VirtualHost — the north-south CRD this subsumes),
proposal 003 (edge proxy), proposal 004 (demand-scoped distribution),
proposal 005 (multi-port), proposal 015 (MeshConfig / controller webhook);
[[project_edge_proxy_plan]], [[project_demand_scoped]].

## Summary

Adopt **Kubernetes Gateway API** as aether's routing API, for *both* directions:
`Gateway` + `HTTPRoute` for north-south ingress (the edge), and **GAMMA** —
`HTTPRoute` with a `Service` `parentRef` — for east-west, service-to-service
routing. One standard API, one route→`backendRef`→registry-EDS→SPIRE-mTLS
pipeline, across the whole mesh. This subsumes the `VirtualHost` CRD (proposal
017) and adds aether's first real **east-west traffic-management layer** (canary,
header/method routing, per-route timeout/retry, mirroring, fault injection) — as
the standard API, on top of aether's differentiators (SPIRE identity per hop,
registry-based EDS, demand-scoped push).

## Motivation

The edge already does the hard, differentiated thing: it's a *mesh client* —
single SPIRE SVID, one mTLS hop directly to backend pods (`:15008`), EDS from the
aether registry, identity preserved end-to-end (XFCC `By=`). But its API is a
bespoke `VirtualHost` CRD, and **east-west has no user-facing L7 routing at all**:
the node proxy's outbound path is pure authority demux (`<svc>.<meshDomain>` → the
service's EDS cluster, 1:1, see `ServiceClusterName` in `proxy/egress.go`). There
is no canary, header routing, per-route timeout, mirror, or fault injection.

Gateway API is the standard, and GAMMA makes it cover meshes. Aether already owns
the data plane both ends need; the gap is the API.

### Two design lines (decision)

1. **Gateway API native in aether's edge/proxy** — aether *implements* Gateway
   API; the data plane stays aether's direct-dial mesh mTLS. Mostly a
   reconciler-input swap on a done data plane; full control; unlocks GAMMA
   east-west. Cost: own a (subset-first, conformance-profile-reported) Gateway API
   implementation.
2. **Adopt Envoy Gateway** as the north-south layer + an extension server that
   injects aether's SPIRE-mTLS transport + registry EDS into its Envoy. No
   Gateway-API impl to maintain, but it *replaces* aether's working edge data
   plane with a bespoke, EG-coupled, dual-control-plane integration, is
   north-south only (no GAMMA), and adds an EG controller/fleet to operate.

**Decision: Line 1.** Aether already owns the differentiated data plane, so the
Gateway API surface is additive, not a rewrite — and only Line 1 reaches the
east-west prize.

## End state — one API, both directions

```
NORTH-SOUTH                         EAST-WEST (GAMMA)
GatewayClass(aether)                (no Gateway — the mesh is the gateway)
  Gateway (listeners, TLS)
    HTTPRoute parentRef=Gateway       HTTPRoute parentRef=Service
        │                                 │
   aether EDGE programs its Envoy    aether NODE PROXY programs outbound RDS
        └──────────────┬──────────────────┘
            SAME: rules → backendRef(Service) → registry EDS cluster
                          → SPIRE mTLS → pod :15008  (XFCC identity)
```

The edge and the mesh become **symmetric**: both "route HTTP to mesh Services over
SPIRE mTLS," differing only in parent (`Gateway` vs `Service`) and ingress point
(LB vs pod-local CNI intercept). `VirtualHost` dissolves into `HTTPRoute`.

## GAMMA — the east-west model

GAMMA reuses `HTTPRoute` verbatim; the pivot is `parentRefs → Service` ("route
traffic *destined for this Service*"). Scope follows *where the route lives*:

- **Producer route** — in the **backend Service's namespace**: the service owner's
  default routing for all callers (e.g. a 90/10 canary split). Mesh-wide.
- **Consumer route** — in a **client's namespace**: a consumer's override for its
  own egress to a service. That namespace only.

A client's effective routing = producer ⊕ consumer-in-its-namespace, resolved by
Gateway API precedence (most-specific match; oldest route on tie). Absence of any
`HTTPRoute` = today's behavior (pass through to the Service's endpoints) — GAMMA
is purely additive.

### This rides aether's existing seams
- **Demand-scoped distribution (004) *is* GAMMA scoping.** Producer routes for
  `svc-1` fan out only to proxies whose dependency set includes `svc-1`; consumer
  routes stay node-local. No new distribution mechanism.
- **Per-port clusters (005)** map `parentRef`/`backendRef` ports onto aether's
  per-port EDS clusters.
- **Transport is unchanged** — every `backendRef` resolves to a registry-EDS
  cluster dialed over SPIRE mTLS.

## Phase 2 sizing — `HTTPRoute(parentRef=Service)` → outbound `RouteConfig`

Today (`proxy/egress.go`): the outbound `RouteConfiguration` carries one vhost per
known service, `Domains: [<svc>.<meshDomain>]`, with a **single default route →
cluster `<svc>.<meshDomain>`** (cluster name == authority), plus a catch-all
(`cluster_header: ":authority"` + ODCDS) for cold services.

GAMMA enriches exactly that per-service vhost's **routes**:

```
vhost  Domains: [svc-1.<meshDomain>]
  for each rule in (producer(svc-1) ⊕ consumer(svc-1, client-ns)), in precedence order:
     Route{
       Match:  ← HTTPRoute rule matches (path/header/method/query)
       Action: WeightedClusters{ ← backendRefs
                  { name: <backend>.<meshDomain>, weight: w },  // existing EDS cluster
                  ...
               }
               Timeout/RetryPolicy ← rule timeouts/retries
               RequestMirrorPolicies ← mirror filter
       (RequestHeaderModifier / Redirect / URLRewrite ← filters)
     }
  default (no rule matched / no HTTPRoute): Route → cluster <svc-1>.<meshDomain>  // = today
```

So the data-plane primitives already exist — weighted clusters, retry/timeout,
mirror, header mutation are all stock Envoy route features; each `backendRef`
maps to an existing `NewServiceCluster` EDS cluster. **What's new is the control
plane**: a reconciler that lists `HTTPRoute`s, groups by Service `parentRef`,
classifies producer/consumer by namespace, merges with precedence, and emits the
enriched per-service vhost into each consumer proxy's RDS via the demand-scoped
catalog. A `backendRef` to a service not in the proxy's dependency set extends the
dependency set (ODCDS warm-up) — reusing the cold-path machinery.

### Construct mapping

| Gateway API | aether |
|---|---|
| `GatewayClass` (controllerName) | the aether controller claims its class |
| `Gateway` listener + TLS | edge listener + per-host SDS cert (proposal 017 cert path) |
| `HTTPRoute` parentRef=`Gateway` | edge RDS (north-south) |
| `HTTPRoute` parentRef=`Service` | node-proxy **outbound** RDS for that service (GAMMA) |
| `backendRef` (Service[:port]) | `NewServiceCluster` EDS cluster (`<svc>.<meshDomain>[:port]`) |
| `backendRefs` weights | Envoy `WeightedClusters` |
| matches / filters / timeouts | Envoy route match / header-mod·redirect·rewrite·mirror / timeout·retry |
| cross-ns `backendRef` | `ReferenceGrant` |
| producer vs consumer ns | demand-scoped fan-out vs node-local (proposal 004) |

## Phasing

- **Phase 1 — north-south.** `GatewayClass`/`Gateway`/`HTTPRoute` (parentRef=Gateway)
  on the edge. Small lift: swap the edge reconciler's input from `VirtualHost` to
  `HTTPRoute`, same `cache.SetVirtualHosts`→Envoy gen. Keep `VirtualHost` working
  in parallel during migration.
- **Phase 2 — GAMMA east-west.** `HTTPRoute` parentRef=Service → node-proxy
  outbound RDS, producer/consumer merge + precedence, weighted/mirror/timeout
  backends. The capability jump.
- **Phase 3 — conformance hardening.** Standard Service VIP / `*.svc.cluster.local`
  interception, `GRPCRoute`, `ReferenceGrant`, supported-features + Mesh-profile
  conformance reporting.

## Tensions / non-goals / open questions

- **Authority model vs conformance.** The Mesh conformance suite assumes
  interception of the Service's *standard* identities (ClusterIP,
  `<svc>.<ns>.svc.cluster.local`); aether routes on `<svc>.<meshDomain>`.
  *Supporting* GAMMA routing is easy; *passing* conformance needs the CNI
  capture + demux to honor the standard Service names (Phase 3). Ship support
  first, report supported-features honestly, chase the badge later.
- **CRD coexistence.** `VirtualHost` stays through Phase 1–2 as a deprecation
  path; the dup-FQDN webhook generalizes to a Gateway/HTTPRoute conflict check.
- **Policy attachment.** Timeouts/retries/mTLS-policy beyond HTTPRoute may want
  `BackendTrafficPolicy`-style attachment later; out of scope here.
- **Non-goals:** TCP/UDP routes, Envoy Gateway adoption (Line 2), per-route
  authz (a SecurityPolicy story), and full GAMMA conformance in Phase 1–2.

## Verification (per phase)

- **Phase 1:** an `HTTPRoute` (parentRef=Gateway, path matches, host) routes the
  edge to mesh services exactly as the `VirtualHost` e2e did (XFCC `By=` per path,
  wildcard TLS), from the in-cluster client against the edge LB.
- **Phase 2:** a producer `HTTPRoute` on `svc-1` 90/10-splits to `svc-1`/`svc-1-v2`
  (observed via XFCC `By=` ratios); a consumer `HTTPRoute` in the client's
  namespace overrides only that client; a `/admin` match routes elsewhere; a
  per-route timeout takes effect — all over the existing SPIRE mTLS, with the
  routes pushed only to proxies whose dependency set includes the service.
