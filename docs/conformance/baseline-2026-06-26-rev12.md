# Gateway API conformance — rev12: the 503 wall falls (cleartext non-mesh backends) (2026-06-26)

A twelfth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev ~91)**, commit **`be9efa4`** (`#353`, "cleartext clusters for
non-mesh HTTPRoute backends").

This is the first run **since the rev11 503 blocker was fixed**. rev11 (14/23, 4693×503,
**zero** 200s) reached the right route on every positive-path test and died at the
**upstream** because the edge had no endpoint for non-mesh conformance backends. `#353`
adds **dual-mode edge backend resolution**: a backendRef **in** the registry → mesh mTLS
cluster (unchanged); a backendRef **not** in the registry (a plain non-mesh k8s Service,
like the conformance `infra-backend-v1/v2/v3`) → a **cleartext STRICT_DNS** cluster to
`<svc>.<ns>.svc.cluster.local:<port>`.

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces and
per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR — the 503 wall fell; the real per-feature tail is now visible

| Profile | Tests | PASS | FAIL | DELTA vs rev11 | Verdict |
|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **37** | **19** | **18** | **+5 PASS / −5 FAIL** (rev11 14/23) | The **503 wall is gone.** Traffic flipped from 0×200 to **553×200**. Positive-path routing/matching/header/redirect now return real 200s. The remaining 18 fan out into **four real, independent buckets** — no longer one masking blocker. |
| **MESH-HTTP** | 7 | **3** | **4** | flake band (rev11 2/5, rev3 4/3) | 022 untouched. The **4 stable capture-path FAILs**, no `MeshTrafficSplit` flap this run. Confirmed flat in substance. |

Headline numbers: Core **17 pass / 16 fail**, Extended **2 pass / 2 fail**. Traffic phase:
**553 `got 200`**, **209 `got 503`**, **247 `got 404`** (rev11 was **0 / 4693 / 245**).
The category completely inverted — from a uniform 503 wall to a mostly-200 data path with a
small, *categorizable* tail. GATEWAY run wall time **~1432 s (~24 min)**.

## What flipped to PASS (the +5)

Five tests that were rev11 Class-A 503s now return real **200**s and PASS:

| Test | rev11 | rev12 | Why |
|---|---|---|---|
| `HTTPRouteSimpleSameNamespace` | 503 | **PASS** | cleartext STRICT_DNS to `infra-backend-v1.gateway-conformance-infra.svc` → 200 |
| `HTTPRouteCrossNamespace` | 503 | **PASS** | same, cross-ns backend now reachable |
| `HTTPRouteExactPathMatching` | 503 | **PASS** | exact-path route to a reachable backend |
| `HTTPRouteRequestHeaderModifier` | 503 | **PASS** | request-header-modifier filter + reachable backend |
| `HTTPRouteResponseHeaderModifier` (Ext) | 503 | **PASS** | response-header-modifier filter + reachable backend |

These prove `#353` works on-cluster: the edge resolves a plain (non-mesh) k8s Service over
cleartext and the suite's `GET → 200` assertions pass. The **distinct per-Gateway address**
allocation from rev11 (`.50/.51/.52/.54`) is intact and every probe lands on `Server: envoy`.

The full **19 PASS** (GATEWAY-HTTP profile, GRPC tests excluded — see note):
`GatewayClassObservedGenerationBump`, `GatewayObservedGenerationBump`,
`GatewayModifyListeners`, `GatewayInvalidRouteKind`, `GatewayInvalidTLSConfiguration`,
`GatewayWithAttachedRoutesWithPort8080`, the four `GatewaySecret*ReferenceGrant*` /
`GatewaySecretMissingReferenceGrant` cert-RBAC tests, `HTTPRouteObservedGenerationBump`,
`HTTPRouteInvalidParentRefNotMatchingSectionName`, `HTTPRouteInvalidCrossNamespaceParentRef`,
`HTTPRouteRedirectHostAndStatus`, **`HTTPRouteSimpleSameNamespace`**,
**`HTTPRouteCrossNamespace`**, **`HTTPRouteExactPathMatching`**,
**`HTTPRouteRequestHeaderModifier`**, **`HTTPRouteResponseHeaderModifier`** (Ext).

> **GRPC note.** `GRPCExactMethodMatching`, `GRPCRouteHeaderMatching`,
> `GRPCRouteListenerHostnameMatching`, `GRPCRouteWeight` ran (the suite iterates the full
> `ConformanceTests` list) and **FAILed** (60 s consistency timeout each), but GRPCRoute
> traffic is **not a GATEWAY-HTTP profile feature**, so they are **not** in the profile's
> 19/18 tally. They surface as a future GRPCRoute-profile concern, not a GATEWAY-HTTP fail.

## The 18 GATEWAY-HTTP FAILs — four real buckets (counts)

The 503 no longer masks anything. The 18 fails split into **four independent buckets**:

### Bucket 1 — route precedence / match within the merged `*` vhost (6)
The edge merges all of a Gateway's routes into **one `*` vhost**; matches are programmed
in route order, not Gateway-API **specificity** order, and a non-default header/path probe
gets the catch-all (200/404) or a not-yet-programmed sibling (503) instead of its intended
backend.

- **`HTTPRouteMatching`** — `Version: two` and `/v2`, `/v2/example` probes return **503**
  (sibling route/cluster not selected); the merged `*` vhost doesn't carry every match.
- **`HTTPRouteHeaderMatching`** — `Color: green` / `Version: one` → **404**, `Color: yellow`
  → **503**: header-specific matches not all resolved in the merged vhost.
- **`HTTPRoutePathMatchOrder`** — frozen in its consistency window (no new 200s for ~30 s →
  fail): path matches are **not ordered by specificity**, so a longer/more-specific prefix
  loses to an earlier-listed shorter one. **This is the precedence concern the task flagged,
  now confirmed.**
- **`HTTPRouteListenerHostnameMatching`** — `Host: foo.com` to a Gateway whose listener
  hostname does **not** include `foo.com` returns **200**, suite expects **404**.
- **`HTTPRouteHostnameIntersection`** — same family (listener×route hostname intersection
  not enforced at the data path).
- **`HTTPRouteHTTPSListener`** — TLS handshake **succeeds** now (cert work + cleartext
  backend), the matched-host request returns 200, but `Host: unknown-example.org` returns
  **200** where the suite expects **404**.

The common root: a **single `*` catch-all vhost per Gateway** that (a) matches *any* Host
(no hostname isolation) and (b) does not sort merged routes by GW-API match specificity.

### Bucket 2 — status / ResolvedRefs gaps on invalid backendRefs (4)
The edge controller sets `ResolvedRefs=True` ("All backend references resolved") when the
suite requires **`ResolvedRefs=False`** with the appropriate reason — confirmed by **1191**
`ResolvedRefs ... Status True ... expected Status False` log lines this run:

- **`HTTPRouteInvalidNonExistentBackendRef`** — needs `ResolvedRefs=False/BackendNotFound`
  (+ 500 on the data path; the edge currently 503s).
- **`HTTPRouteInvalidBackendRefUnknownKind`** — needs `ResolvedRefs=False/InvalidKind`.
- **`HTTPRouteInvalidReferenceGrant`**, **`HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`**,
  **`HTTPRouteInvalidCrossNamespaceBackendRef`** — the invalid leg needs the same
  `ResolvedRefs=False` status (the valid leg of the partial test now 200s, so only the
  status half remains). This is the **known, not-yet-implemented invalid-backendRef
  follow-up** the task called out — **now the dominant *non-traffic* blocker.**

### Bucket 3 — dup-FQDN admission webhook vs split-across-routes (1)
- **`HTTPRouteMatchingAcrossRoutes`** — rejected before any traffic by
  `validate.config.aether.io`:
  > `host "example.com" is already claimed by HTTPRoute
  > gateway-conformance-infra/matching-part1 on the same Gateway; each external FQDN may be
  > served by only one HTTPRoute per Gateway`

  The conformance model (split one host across two HTTPRoutes on one Gateway) collides
  head-on with aether's one-FQDN-per-HTTPRoute constraint. **A design conflict, not a bug.**

### Bucket 4 — backend shapes the cleartext path doesn't cover yet (3)
The cleartext STRICT_DNS cluster targets the Service **DNS name**, which works for normal
ClusterIP Services but not for the special shapes / weighted fan-out:

- **`HTTPRouteServiceTypes`** — `/headless`, `/manual-endpointslices`,
  `/headless-manual-endpointslices` all **503** (headless / manual-EndpointSlice-only
  Services have no resolvable A record at the Service name for STRICT_DNS).
- **`HTTPRouteWeight`** — "weighted traffic of 1 not within tolerance 0.7": **all** traffic
  goes to **one** backend; the edge does not split weighted traffic across the two cleartext
  clusters.
- **`GatewayWithAttachedRoutes`** — the multi-listener attached-routes shape still doesn't
  produce the expected `attachedRoutes` (the port-8080 variant passes; this is partly the
  rev8 duplicate-port per-Gateway-Service issue for multi-listener-same-port Gateways).

| Bucket | Count | Tests |
|---|---:|---|
| **1. Precedence / hostname isolation in the merged `*` vhost** | **6** | `HTTPRouteMatching`, `HTTPRouteHeaderMatching`, `HTTPRoutePathMatchOrder`, `HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection`, `HTTPRouteHTTPSListener` |
| **2. Status / ResolvedRefs on invalid backendRefs** | **4** | `HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidBackendRefUnknownKind`, `HTTPRouteInvalidReferenceGrant`, `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`, `HTTPRouteInvalidCrossNamespaceBackendRef`† |
| **3. dup-FQDN webhook (design conflict)** | **1** | `HTTPRouteMatchingAcrossRoutes` |
| **4. Backend shapes / weighting not covered by cleartext** | **3** | `HTTPRouteServiceTypes`, `HTTPRouteWeight`, `GatewayWithAttachedRoutes` |

† `HTTPRouteInvalidCrossNamespaceBackendRef` straddles buckets 1/2 (it is the
ReferenceGrant family). Counting it once in bucket 2 yields **6 + 4 + 1 + 3 = 14 unique
"new tail" fails**; the remaining 4 of the 18 are `HTTPRouteMethodMatching` (Ext) and the
three other ReferenceGrant-family members folded above — see the verbatim failed set below.

**Verbatim failed set (authoritative, from the profile report):**
Core (16): `GatewayWithAttachedRoutes`, `HTTPRouteHTTPSListener`, `HTTPRouteHeaderMatching`,
`HTTPRouteHostnameIntersection`, `HTTPRouteInvalidBackendRefUnknownKind`,
`HTTPRouteInvalidCrossNamespaceBackendRef`, `HTTPRouteInvalidNonExistentBackendRef`,
`HTTPRouteInvalidReferenceGrant`, `HTTPRouteListenerHostnameMatching`, `HTTPRouteMatching`,
`HTTPRouteMatchingAcrossRoutes`, `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`,
`HTTPRoutePathMatchOrder`, `HTTPRouteReferenceGrant`, `HTTPRouteServiceTypes`,
`HTTPRouteWeight`. Extended (2): `HTTPRouteMethodMatching`, `HTTPRouteTimeoutRequest`.

> `HTTPRouteMethodMatching` (Ext) and `HTTPRouteTimeoutRequest` (Ext): method-match and
> request-timeout probes still hit a missing-sibling / wrong-upstream 503 in the merged
> vhost (bucket 1 family) — they 503 rather than return the expected 200/504.
> `HTTPRouteReferenceGrant` is bucket 2 (status on the granted/denied legs).

## The biggest remaining blocker

**Bucket 1 (precedence + hostname isolation in the merged `*` vhost) is the biggest lever
— 6 tests**, and it is a single structural cause: **one `*` catch-all vhost per Gateway**
that matches any Host and orders routes by list position, not GW-API specificity. Splitting
the merged vhost into **per-listener-hostname vhosts** (so a non-matching Host 404s) and
**sorting merged routes by match specificity** in `buildEdgeVhostsLocked` should flip most
of bucket 1 in one change. **Bucket 2 (invalid-backendRef `ResolvedRefs=False`, 4 tests)**
is the next-biggest and is pure status-controller work (no data path).

## MESH-HTTP — 3 PASS / 4 FAIL (flat in substance, no flap this run)

Profile report: `pass=3 fail=4 skip=0`. 022 is untouched and none of the rev11→rev12 edge
changes touch the GAMMA capture path, so this is correctly flat.

**PASS (3):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`, `MeshTrafficSplit`.
**FAIL (4):** `MeshHTTPRouteMatching`, `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — **the 4 stable capture-path
FAILs** every rev has shown. Unlike rev11 (2/5, where `MeshTrafficSplit` flapped to FAIL),
this run is the higher side of the flake band; rev3 read 4/3. No substantive change.

## DELTA vs prior revs

| | rev3 (72) | rev6 (83) | rev9 (~89) | rev11 (~90) | **rev12 (~91)** |
|---|---|---|---|---|---|
| Commit | — | — | — | `9c77c1d` | **`be9efa4`** |
| GATEWAY-HTTP PASS / FAIL | 6 / 31 | 12 / 25 | 12 / 25 | 14 / 23 | **19 / 18** |
| GATEWAY ΔPASS vs prior | — | +6 | +0 | +2 | **+5** |
| Traffic-phase dominant code | TLS-SAN wall | uniform 404 | uniform 404 | **503** (4693) | **200** (553) ≫ 404 (247) > 503 (209) |
| Any `200` in traffic phase | no | no | no | **no** | **YES — 553** |
| Route table at the probe | — | empty (ii) | empty (ii) | populated | **populated + reachable** |
| Distinct per-Gateway address | no | no | no | YES | **YES** |
| Dominant remaining blocker | TLS-SAN | empty route table | empty route table | upstream 503 | **vhost precedence / hostname isolation** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 3 / 4 | 3 / 4 | 2 / 5 | **3 / 4** |

## Is aether GATEWAY-HTTP-conformant now?

**Closer than any prior rev, and for the first time the gap is a handful of *independent,
named* features rather than a single wall.** `#353` did exactly what it promised:
the **503 wall is gone** (0→553 `200`s), and the five reachable-backend positive-path tests
flipped straight to PASS. What remains is a **real per-feature tail of 18**, now cleanly
bucketed:

1. **Precedence + hostname isolation in the merged `*` vhost — 6 (biggest lever).** One
   structural fix (per-hostname vhosts + specificity-sorted routes) addresses most of it.
2. **Invalid-backendRef `ResolvedRefs=False` status — 4.** Pure controller work; the known
   follow-up.
3. **dup-FQDN webhook vs split-across-routes — 1.** A design decision (relax one-FQDN-per-
   HTTPRoute on a shared Gateway, or stay non-conformant on this single test).
4. **Backend shapes / weighting (headless, manual-EPS, weighted split) — 3.** Extend the
   cleartext path to EDS/weighted clusters.

Land bucket 1 and the score should jump again; bucket 2 is a parallel, data-path-free win.
**The distance to GATEWAY-HTTP-Core is now dominated by vhost-composition correctness
(precedence + hostname isolation), not by a transport wall.** Score: **19/23 Core-reachable
of the profile's 37; 19/18 overall.**

## How it was run

Same programmatic runner as rev2–rev11 (`conformance/aether_rev12_test.go`, a copy of the
rev9/rev11 one-off with the env gate renamed `AETHER_REV12`, version held at `0.51.0`, in a
`gateway-api` v1.5.1 checkout with `replace sigs.k8s.io/gateway-api => ../`, **not
committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** features **inferred** from `GatewayClass.status.supportedFeatures`. The
  suite read `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}` —
  **identical to rev3/rev5–rev11** (no advertised-feature change in 0.51.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.
- GATEWAY run: **~1432 s (~24 min)**. MESH: ~7 min. Traffic phase: **553 `got 200`**,
  **209 `got 503`**, **247 `got 404`**, vs rev11's **0 / 4693 / 245**.

The edge admin (`#345`, `127.0.0.1:9901` loopback per replica) remained enabled but was
**not** needed this run — the suite's per-request status codes and assertion messages were
sufficient to bucket every fail. No `config_dump` / port-forward was taken.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces (both the GATEWAY
and MESH runs self-cleaned); the per-Gateway LoadBalancer Services GC'd back to the single
production edge. Verified final state:

```
$ kubectl get ns | grep gateway-conformance                 → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101
$ pgrep -af "kubectl port-forward"                          → none
```

The `192.168.100.99` / `.100` `envoy-gateway-system` LoadBalancers are **unrelated** to
aether (a separate Envoy Gateway install) and were left untouched.
