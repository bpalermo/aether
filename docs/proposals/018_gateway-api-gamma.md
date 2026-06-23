# Proposal: Gateway API for aether — north-south + GAMMA east-west

**Status:** Design — 2026-06-22
**Relates:** proposal 017 (VirtualHost — the north-south CRD this subsumes),
proposal 003 (edge proxy), proposal 004 (demand-scoped distribution),
proposal 005 (multi-port), proposal 006 (origin-partitioned registry — the
cross-cluster plane), proposal 015 (MeshConfig / controller webhook);
[[project_edge_proxy_plan]], [[project_demand_scoped]],
[[project_registrar_etcd_vs_ddb]].

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
- **Phase 3 — transparent capture + conformance.** Today's call model is
  **explicit egress**: the app dials `127.0.0.1:18081` with `Host:
  <svc>.<meshDomain>` (the proxy binds that loopback listener into the pod netns;
  no iptables redirect). That is HTTP-only *by construction* — the proxy only ever
  sees what the app explicitly sends. Conformance (and unmodified apps, and the
  multi-cluster `ServiceImport` VIP) need **transparent capture** of the Service
  VIP / `*.svc.cluster.local:port` (and the clusterset VIP), recovering the intended
  service via **original-dst / SNI** instead of an explicit `Host`.
  - **Keep EDS metadata under original-dst.** Use original-dst as a *recover +
    demux* signal (captured VIP → service → the existing **EDS cluster**), NOT an
    `ORIGINAL_DST` *cluster*. The latter has no endpoint set, so it loses locality,
    subsets, registry health, and — fatally for zero-trust — the per-endpoint
    **SPIFFE SAN** for upstream mTLS. The agent already owns the VIP↔service map (it
    allocates the Service/ServiceImport VIP); the cold path stays ODCDS. Reserve the
    `ORIGINAL_DST` cluster for genuinely unknown destinations only.
  - **Transparent capture forces L4+L7.** Capturing the VIP captures *everything*
    the app sends there (Postgres, Redis, app-TLS, raw TCP), so the proxy must
    become a general-protocol data plane: protocol-detect
    (`tls_inspector`+`http_inspector`) → **HCM** for HTTP/2·HTTP/1.1
    (HTTPRoute/GRPCRoute, full L7) and a **`tcp_proxy`-over-mTLS floor** for
    everything else (non-HTTP services keep working *and* keep SPIRE mTLS + the EDS
    SPIFFE SAN, just without L7 rules). Both the outbound capture path **and** the
    inbound `:15008` (today an HCM) need the inspector-selected `tcp_proxy` chain.
  - **Phase 3a (floor):** HTTP/gRPC transparent capture + the TCP-over-mTLS
    passthrough — the minimum that makes capture safe; still HTTP-Mesh-profile
    conformant. **Phase 3b (opt-in):** `TCPRoute`/`TLSRoute`/`UDPRoute` L4 routing as
    additional profiles. Throughout, keep the **explicit `localhost:18081` HTTP
    lane** as an HTTP-only fast path for mesh-aware clients.
  - Plus `ReferenceGrant`, supported-features + Mesh-profile conformance reporting.

## Multi-cluster — the registry is the cross-cluster plane

Gateway API and GAMMA are single-cluster specs; the standard multi-cluster answer
is the **MCS API** (`ServiceExport`/`ServiceImport`, `*.svc.clusterset.local`),
which normally pairs with **EndpointSlice import** + a **Lighthouse-style CoreDNS**
that resolves `clusterset.local` by querying a cross-cluster broker. Aether avoids
both: it already has a cross-cluster endpoint plane that **isn't DNS** — the
registrar + origin-partitioned per-region etcd (proposal 006). A `backendRef`
resolves to endpoints in any cluster/region via registry EDS, at **dial time in
the proxy**, never via DNS.

Design — MCS objects for *conformance*, registry for *wiring*:

- **Registry carries endpoints AND producer routes** (proposal 006 extended),
  origin-partitioned + replicated. Consumer clusters pull a remote service's
  producer `HTTPRoute` from the registry (demand-scoped); consumer overrides stay
  local. No HTTPRoute-object replication, no config-replication controller.
- **MCS objects are materialized locally from the registry.** An aether MCS
  controller reconciles `ServiceExport` → registry, and in each cluster materializes
  a **local** `ServiceImport` + a **local** clusterset VIP (local IPAM). Gateway
  API/GAMMA accept `backendRef`/`parentRef` → `ServiceImport` (group
  `multicluster.x-k8s.io`), so multi-cluster routes are expressed in-spec.
- **DNS stays strictly local.** Each cluster's CoreDNS serves `clusterset.local`
  from its *own* `ServiceImport` objects → its *own* VIP — standard MCS DNS over
  local objects, never forwarding/replicating across clusters. The proxy intercepts
  the VIP and resolves endpoints from the registry. The MCS spec doesn't mandate how
  endpoints are aggregated behind the VIP; aether uses its registry instead of
  EndpointSlice import.

This is conformant (Gateway API `ServiceImport` backends; MCS `ServiceExport`/
`ServiceImport` + local `clusterset.local`; users only ever touch standard objects
in their own cluster) while the cross-cluster wiring is aether's registry.

### Connectivity modes

Only the endpoint's *dial target* differs; the registry/MCS/DNS design above is
identical for both.

- **Flat network (default).** Pod IPs are routable across clusters, so a remote
  endpoint in the registry is just `pod_ip:15008` + SPIFFE ID — identical to a
  local one. The proxy dials it **directly** over SPIRE mTLS (unchanged code; one
  identity-preserving hop). No east-west gateway. **Requires:**
  1. **Non-overlapping, globally-unique pod CIDRs** across all clusters (IPAM
     discipline; watch CIDR exhaustion as cluster count grows).
  2. **A routable underlay** that forwards cross-cluster pod-to-pod traffic — flat
     L3 (VPC peering, BGP, Cilium native routing / cluster-mesh, non-encapsulated;
     mind MTU on tunneled fabrics). Connectivity is pushed to the network, not a
     gateway.
  3. **SPIRE trust across clusters** — one shared trust domain, or SPIRE federation
     (per-cluster trust domains + bundle exchange) so the dialing proxy already
     trusts the remote SVID issuer named in the registry endpoint.
  Trade-off: any pod is L3-reachable cross-cluster (no gateway chokepoint). Defused
  by zero-trust — every hop is SPIRE mTLS, so a dial without a valid trusted SVID is
  rejected at inbound `:15008` regardless of L3 reachability; identity, not the
  network, is the boundary. What you give up is defense-in-depth / a single egress
  audit point.
- **East-west gateway (fallback).** No flat-network requirement: the registry
  endpoint for a remote pod carries the **remote cluster's east-west gateway address
  + target SPIFFE ID** (the edge generalized), and the proxy dials that gateway,
  which forwards to the local pod. Adds a component to build/run/scale; one extra
  hop (identity still preserved via XFCC).

Connectivity mode is a deploy-time choice; conformance is unaffected (it's a
data-plane detail below the API).

## Tensions / non-goals / open questions

- **Authority model vs conformance.** The Mesh conformance suite assumes
  interception of the Service's *standard* identities (ClusterIP,
  `<svc>.<ns>.svc.cluster.local`); aether routes on `<svc>.<meshDomain>`.
  *Supporting* GAMMA routing is easy; *passing* conformance needs the CNI
  capture + demux to honor the standard Service names (Phase 3). Ship support
  first, report supported-features honestly, chase the badge later.
- **The real conformance cost is a protocol-agnostic data plane, not route types.**
  aether is HTTP-only today *by construction* (explicit `localhost:18081` egress —
  the proxy only sees what the app sends). Transparent capture of the Service VIP
  promotes the proxy to a general **L4+L7** data plane (inspectors → HCM for HTTP,
  `tcp_proxy`-over-mTLS for the rest), because capturing the VIP captures every
  protocol the app uses. This is the genuinely new data-plane work the
  Gateway-API direction pulls in (Phase 3a) — driven by the *interception model*,
  not the HTTP route-type checklist.
- **CRD coexistence.** `VirtualHost` stays through Phase 1–2 as a deprecation
  path; the dup-FQDN webhook generalizes to a Gateway/HTTPRoute conflict check.
- **Policy attachment.** Timeouts/retries/mTLS-policy beyond HTTPRoute may want
  `BackendTrafficPolicy`-style attachment later; out of scope here.
- **Non-goals:** TCP/UDP **L4 routing** (`TCPRoute`/`UDPRoute`/`TLSRoute`, deferred
  to Phase 3b — note the TCP-over-mTLS *passthrough floor* is NOT a non-goal; Phase
  3a requires it), Envoy Gateway adoption (Line 2), per-route authz (a
  SecurityPolicy story), and full GAMMA conformance in Phase 1–2.

## Verification (per phase)

- **Phase 1:** an `HTTPRoute` (parentRef=Gateway, path matches, host) routes the
  edge to mesh services exactly as the `VirtualHost` e2e did (XFCC `By=` per path,
  wildcard TLS), from the in-cluster client against the edge LB.
- **Phase 2:** a producer `HTTPRoute` on `svc-1` 90/10-splits to `svc-1`/`svc-1-v2`
  (observed via XFCC `By=` ratios); a consumer `HTTPRoute` in the client's
  namespace overrides only that client; a `/admin` match routes elsewhere; a
  per-route timeout takes effect — all over the existing SPIRE mTLS, with the
  routes pushed only to proxies whose dependency set includes the service.
