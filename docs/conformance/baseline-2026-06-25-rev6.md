# Gateway API conformance — the cross-namespace route-status unlock, rev6 (2026-06-25)

A sixth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.50.0 (rev 83)**. This run measures **one fix**: **#339 — grant the edge
`namespaces` read RBAC**, which was the binding constraint that capped
[rev5](./baseline-2026-06-25-rev5.md).

In rev5 the profile *scored again* but the headline barely moved (6 → 7) because a
**previously-hidden** blocker became binding: aether wrote Gateway/listener status
but **not `HTTPRoute.status.parents`**, so every HTTPRoute test timed out at the
suite's *"wait for the route to be accepted by its parent"* gate before any
traffic was sent. The root cause was diagnosed as **#339**: the edge ClusterRole
lacked `namespaces` `list`/`watch`, so the Namespace informer never synced and the
`allowedRoutes.namespaces.from: Selector` attachment check (a cached `Get` on the
route's Namespace) stalled — **cross-namespace** route reconciliation never
completed → those routes never got `status.parents`. With **#339** the informer
syncs and **cross-ns HTTPRoutes now attach and report status.**

Aether was **not** modified or redeployed for this run. The deployed Helm release
is `aether-0.50.0` rev **83**, image/commit **`cfa8c65`** — exactly the #339 commit
(`fix(edge): grant the edge namespaces read RBAC (unblocks cross-ns route status) (#339)`).
All `gateway-conformance-*` namespaces **and** the per-Gateway LoadBalancer
Services were cleaned up afterward (see Cleanup — **the GC self-resolved this run**).

## TL;DR

| Profile | Setup | Tests run | PASS | FAIL | SKIP (in-profile) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** (north-south) | ✅ passed | **37** | **12** | **25** | 0 | **Large jump: 7 → 12 (+5).** #339 lands exactly as predicted: **every HTTPRoute test now passes the route-status gate** ("Route … Parents matched expectations") and **reaches the traffic phase** — including cross-namespace routes, which were the specific casualties of the namespaces-informer stall. The rev5 route-status wall is gone, and three route-status **negatives** now pass (`HTTPRouteInvalidCrossNamespaceParentRef`, `HTTPRouteInvalidParentRefNotMatchingSectionName`, `GatewayClassObservedGenerationBump` quick win too). But a **new, deeper wall** is now binding: the edge **does not program Envoy routes/clusters for conformance HTTPRoute backends**, so every traffic probe returns a **404 from envoy** on plain HTTP to the (correct, distinct) per-Gateway address. **4080/4080 traffic failures are `got 404` — zero TLS errors, zero 503s.** |
| **MESH-HTTP** (east-west / GAMMA) | passed | 7 | **3*** | **4*** | 0 | **Flat / within the flaky band.** This run read `3/4`: `MeshTrafficSplit` flaked instead of `MeshHTTPRouteMatching` (which read PASS this run). The 3 stable capture-path FAILs (`MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`) are unchanged. 022 untouched. |

**Headline delta:** GATEWAY-HTTP **6/31 (rev3)** → **7/30 (rev5)** → **12/25
(rev6)** — **+5 vs rev5, +6 vs rev3.** The expected "large jump" materialised: the
single-reconciler blocker rev5 identified (`HTTPRoute.status.parents`, gated on the
namespaces informer) was fixed by **#339**, the route tests flipped past the
status gate **and reached traffic**, and three route-status negative tests flipped
to PASS outright. **The traffic tests did NOT flip to PASS** — but for a brand-new,
clearly-localised reason (uniform backend 404), not the old route-status or
TLS-SAN walls. MESH-HTTP is flat.

> **#339 is validated end-to-end.** A cross-namespace HTTPRoute now gets
> `status.parents` with `controllerName=gateway.aether.io/edge`, `Accepted=True`,
> `ResolvedRefs=True`; the cross-ns Gateways are `Programmed=True` with their own
> distinct pool IPs; **0 namespace-forbidden errors** in the edge logs; and the
> orphan-Service GC **self-resolved** (no manual LB cleanup needed). The binding
> constraint has moved one layer deeper — from **route status** to **data-plane
> route/cluster generation for the resolved backend.**

## How it was run

Same programmatic runner as rev2–rev5 (`conformance/aether_rev6_test.go`, a copy
of the rev5 one-off in a `gateway-api` v1.5.1 checkout with
`replace sigs.k8s.io/gateway-api => ../`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}`
  — **identical to rev3/rev5** (no advertised-feature change in 0.50.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification,
  HTTPRouteMethodMatching, HTTPRouteRequestTimeout}`), as in rev2–rev5.
- Budgets (rev5 style): `GatewayMustHaveAddress=180s`,
  `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.

The GATEWAY run finished in **2209 s (~36.8 min)** — faster than rev5's ~46.6 min
because routes now *pass* the status gate quickly instead of each chewing the full
60 s `MaxTimeToConsistency` waiting for `status.parents` (the traffic-phase retry
windows are shorter than the status-gate timeout). MESH finished in 195 s.

## DELTA vs rev3, rev5, and the original baseline

| | Baseline (0.42.0, rev 68) | rev3 (0.45.0, rev 72) | rev5 (0.49.0, rev 82) | **rev6 (0.50.0, rev 83)** | rev5→rev6 |
|---|---|---|---|---|---|
| **GATEWAY-HTTP Setup** | ❌ blocked | ✅ | ✅ | ✅ | = |
| **GATEWAY-HTTP tests run** | 0 | 37 | 37 | **37** | = |
| **GATEWAY-HTTP PASS** | 0 | 6 | 7 | **12** | **+5** |
| **GATEWAY-HTTP FAIL** | 0 | 31 | 30 | **25** | **−5** |
| Gateways get DISTINCT `.status.addresses` | no | shared `.101` | DISTINCT per-Gw | **DISTINCT per-Gw** (.50/.51/.52…) | = |
| HTTPS base Gw `Programmed` / listener `ResolvedRefs` | n/a | n/a | **True** (#337) | **True** | = |
| **`HTTPRoute.status.parents` written** | no | no | **no (rev5 wall)** | **YES (#339)** | ✅ **flipped** |
| Cross-ns HTTPRoute attaches (`Accepted`/`ResolvedRefs`) | no | no | **no** | **YES** | ✅ **#339** |
| Route-status negative tests pass | no | no | no | **YES (×2)** | ✅ |
| Traffic tests reach the traffic phase | no | yes (then TLS-SAN wall) | **no (route-status wall)** | **YES** | ✅ |
| Dominant blocker | Gw has no addr | bare-IP TLS-SAN | **`HTTPRoute.status.parents` not written** | **edge does not program routes/clusters for backends → uniform 404** | **moved deeper** |
| Orphan per-Gw LB Service after teardown | n/a | n/a | **5 (manual delete)** | **0 (GC self-resolved)** | ✅ |
| **MESH-HTTP PASS / FAIL** | 4 / 3 | 4 / 3 | 3 / 4* | **3 / 4*** (flaky) | flat |

## Did the route/traffic tests flip? (the rev5 question)

**The route-status tests flipped to PASS; the traffic tests flipped past the
status gate and into the traffic phase, but do not yet pass.** Both halves of the
expected effect of #339 are directly observed.

**1. Route status: flipped (the #339 win).** Live evidence captured mid-run — a
cross-namespace HTTPRoute (`gateway-conformance-web-backend/cross-namespace`
attaching to `gateway-conformance-infra/backend-namespaces`) now reports:

```
ctrl=gateway.aether.io/edge  parent=gateway-conformance-infra/backend-namespaces
  Accepted=True       reason=Accepted      observedGeneration=1
  ResolvedRefs=True   reason=ResolvedRefs  observedGeneration=1
```

and **0 namespace-forbidden errors** in the edge logs (rev5's stall signature is
gone). In every traffic test's log the suite now prints
`Route … Parents matched expectations` **before** the first request — the rev5
gate (`error waiting for HTTPRoute to have parents matching expectations`, 19
occurrences) is **entirely absent**. Three route-status **negative** tests now
PASS as a direct consequence: `HTTPRouteInvalidCrossNamespaceParentRef` and
`HTTPRouteInvalidParentRefNotMatchingSectionName` (route `Accepted=False` with the
right reason) plus `GatewayClassObservedGenerationBump`.

**2. Traffic: reaches the phase, then 404s uniformly (the NEW wall).** Past the
status gate, every traffic test issues a real `GET http://192.168.100.50/…`
(plain HTTP, to the **correct, distinct** per-Gateway address — **no** redirect,
**no** TLS, **no** bare-IP SAN failure: the rev3 TLS-SAN wall is gone), and the
edge Envoy answers **404**:

```
GET http://192.168.100.50/  → expected 200, got 404
  CRes: {404 … Server:[envoy]}
```

**This is uniform: 4080/4080 traffic-phase failures across the whole run are
`got 404`. Zero `x509`/`tls:`/IP-SAN errors, zero 503s, zero other status codes.**
Even `HTTPRouteHTTPSListener` and `HTTPRouteRedirectHostAndStatus` 404 (the
expected 301/302 redirect responses never arrive — they 404 first). So the
diagnosis is crisp: the **route attaches and resolves its backendRef in *status*,
but the edge does not generate the corresponding Envoy route/cluster in the *data
plane*** for the per-Gateway listener, so every backend is a 404. The
distinct-address + own-ns HTTPS-cert machinery (validated in rev5) is in place and
no longer the constraint; the constraint is now **data-plane route/cluster
generation for resolved HTTPRoute backends.**

## GATEWAY-HTTP — the 12 PASS (rev6)

```
GatewayClassObservedGenerationBump            (NEW — quick win)
GatewayInvalidRouteKind
GatewayModifyListeners
GatewayObservedGenerationBump
GatewaySecretInvalidReferenceGrant
GatewaySecretMissingReferenceGrant
GatewaySecretReferenceGrantAllInNamespace
GatewaySecretReferenceGrantSpecific
GatewayWithAttachedRoutesWithPort8080
HTTPRouteInvalidCrossNamespaceParentRef       (NEW — route-status negative, #339)
HTTPRouteInvalidParentRefNotMatchingSectionName (NEW — route-status negative, #339)
HTTPRouteObservedGenerationBump
```

The **+5 vs rev5** = 3 route-status tests unblocked by #339
(`HTTPRouteInvalidCrossNamespaceParentRef`,
`HTTPRouteInvalidParentRefNotMatchingSectionName`,
`HTTPRouteObservedGenerationBump`) + the `GatewayClassObservedGenerationBump`
quick win + the net of `GatewayInvalidRouteKind` now stably passing. The
Gateway/Secret-level status surface remains in good shape (all the
`GatewaySecret*` / ReferenceGrant-for-cert tests still pass).

## GATEWAY-HTTP — categorized remaining blockers (rev6)

| # | Blocker | Tests affected | Category | Notes |
|---|---|---|---|---|
| **1 (NEW dominant)** | **Edge does not program Envoy routes/clusters for resolved HTTPRoute backends** → uniform **404** on the per-Gateway listener. | **~20 — every HTTPRoute traffic test** (SimpleSameNamespace, CrossNamespace, ExactPathMatching, Matching, HeaderMatching, PathMatchOrder, HostnameIntersection, ListenerHostnameMatching, ServiceTypes, Weight, RedirectHostAndStatus, RequestHeaderModifier, ResponseHeaderModifier, MethodMatching, TimeoutRequest, HTTPSListener, and the Invalid*BackendRef/ReferenceGrant tests that also probe traffic). | **Real data-plane feature, the new binding constraint.** Route *status* resolves the backend; the *data plane* doesn't get a matching route/cluster, so the edge 404s. **Top priority — the single fix that unlocks the bulk of the remaining profile.** |
| 2 | `GatewayInvalidTLSConfiguration` malformed/terminate-cert listener still reports `ResolvedRefs=True`. | 1 | status edge-case | Same residual as rev5 (#331/#337 cover most cert states; this specific malformed/terminate case isn't detected). Independent of #1. |
| 3 | `GatewayWithAttachedRoutes` (non-port-8080 variant) — `attachedRoutes` count/shape. | 1 | depends on #1-adjacent | The port-8080 variant passes; this one rides on full route-attachment accounting. |
| 4 | dup-FQDN webhook vs split-across-routes. | `HTTPRouteMatchingAcrossRoutes` (1) | design | Unchanged design conflict (one-FQDN-per-HTTPRoute vs the conformance split model). |
| 5 | Invalid-backendRef route-status negatives (`HTTPRouteInvalidBackendRefUnknownKind`, `…InvalidNonExistentBackendRef`, `…InvalidCrossNamespaceBackendRef`, `…InvalidReferenceGrant`, `…PartiallyInvalidViaInvalidReferenceGrant`, `HTTPRouteReferenceGrant`). | up to 6 | mixed status+traffic | These now pass the parent gate but assert specific `ResolvedRefs`/backend conditions **and** (for the positive ones) traffic; they fail on the 404 and/or on backend-condition specifics. Several likely flip with #1 + a `ResolvedRefs=False` reason for bad backends. |

> **Re-prioritisation from rev5:** rev5 predicted that implementing
> `HTTPRoute.status.parents` would "unlock essentially the entire HTTPRoute
> surface." That was **half right** — #339 unblocked route *status* and the *gate*
> (the ~24-test status wall is gone; +5 PASS), but it also **revealed the next
> wall**: the edge resolves backends in status without programming the
> corresponding Envoy route/cluster, so the traffic tests now fail on a uniform
> backend 404 rather than on route status. rev6's contribution is making that
> **next** blocker visible and measured.

## EXTENDED (3 FAIL, all gated by #1)

`HTTPRouteMethodMatching`, `HTTPRouteResponseHeaderModifier`,
`HTTPRouteTimeoutRequest` — all three pass the route-status gate and fail at the
**backend 404** (same as #1). The advertised extended features
(`GatewayPort8080`, `HTTPRouteMethodMatching`, `HTTPRouteRequestTimeout`,
`HTTPRouteResponseHeaderModification`) aren't in question; the tests can't get
past the 404 to exercise them. (`GatewayWithAttachedRoutesWithPort8080` — the
extended port-8080 variant — passes.)

## MESH-HTTP — still ~3-4/3-4 (flaky boundary; 022 untouched)

Profile report (this run): `pass=3 fail=4`. Failed set: `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`, `MeshTrafficSplit`.

- **The boundary is flaky.** This run `MeshHTTPRouteMatching` read **PASS** while
  `MeshTrafficSplit` read **FAIL** — the inverse of some prior runs. On a fully
  converging run the profile reads `4/3`. Both are capture-path
  convergence-sensitive (GAMMA transparent capture), not stable failures.
- **Stable FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
  `MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — **identical to
  rev3/rev5/baseline.** HTTPRoute redirect/header-modifier filters translate on
  the **edge** path but **not on the GAMMA transparent-capture path**, and
  weighted distribution doesn't converge on capture. **Proposal 022
  (arbitrary-Service interception / genuine GAMMA data plane) is not
  implemented**, so this is correctly flat. #339 is an edge RBAC fix and does not
  touch the mesh data path.

## Prioritized remaining blockers

| Priority | Blocker | Profile | Impact |
|---|---|---|---|
| **P0 (GW) — NEW dominant** | **Program the Envoy route + cluster for each resolved HTTPRoute backend on the per-Gateway listener** (the edge resolves `backendRef` in *status* but emits no matching data-plane route/cluster → uniform 404). | GATEWAY-HTTP | **Unblocks ~20 traffic tests** — the entire HTTPRoute traffic surface (routing, matching, redirect, header-modifier, weight, HTTPS-listener) plus several backendRef-status negatives. With route status (#339), distinct addressing, and own-ns HTTPS cert all in place, this is the one fix expected to convert the bulk of the 25 fails. **Single highest-leverage item.** |
| **P0 (Mesh)** | **Apply HTTPRoute filters on the GAMMA / transparent-capture path (proposal 022).** | MESH-HTTP | `MeshHTTPRouteRedirectHostAndStatus` + `MeshHTTPRouteRequestHeaderModifier` → PASS (~6/7). Unchanged since baseline. |
| **P1** | **Re-score GATEWAY-HTTP after P0(GW).** | GATEWAY-HTTP | Expected to move the headline well past 12 — most route tests already reach traffic and 404 only for lack of a programmed route. |
| **P2 (quick wins)** | `GatewayInvalidTLSConfiguration` malformed/terminate-cert detection (1); `ResolvedRefs=False`/reason for invalid backendRefs (helps the backend-negative set). | GATEWAY-HTTP | small, independent of P0(GW). |
| **P3** | dup-FQDN webhook vs `HTTPRouteMatchingAcrossRoutes` (design); stabilise GAMMA capture so `MeshHTTPRouteMatching`/`MeshTrafficSplit`/`MeshHTTPRouteWeight` converge. | both | tail. |

## Is aether GATEWAY-HTTP-conformant now? (honest assessment)

**No — but it cleared the rev5 wall and the gap is again singular and precisely
localised.** rev6 is a real milestone: **#339 landed exactly as rev5 predicted** —
cross-ns HTTPRoutes attach, `status.parents` is written (Accepted/ResolvedRefs with
the aether controllerName and correct observedGeneration), the route-status wall is
gone, three route-status negatives flip to PASS, and **every traffic test now
reaches the traffic phase**. The headline jumped **7 → 12 (+5)**, the GC
self-resolved (no LB leak), and the namespaces-informer stall is gone (0
forbidden errors).

But conformance is **traffic-centric for the HTTPRoute surface**, and aether now
resolves backends in *status* without programming them in the *data plane*: **one
missing data-plane reconciler (per-Gateway listener route/cluster generation for
resolved backends) gates ~20 of the 25 failures** with a **uniform 404** — not a
TLS, addressing, or status problem. So the honest read is unchanged in shape but
advanced one full layer: **aether is again one well-scoped reconciler away from a
large jump — this time on the data plane rather than on status.** Until that lands,
the answer to "conformant?" is **12/37 — not conformant, but the binding
constraint is singular, identified, and (unlike rev5) past the entire status
surface.**

## Cleanup (Phase 2 LB-leak check)

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces, and —
**unlike rev5** — the per-Gateway LoadBalancer Service GC **self-resolved** (it was
a symptom of the same namespaces-informer stall #339 fixed). **No manual cleanup
was required.** Final state verified:

```
$ kubectl get ns | grep conformance                      → none
$ kubectl get gateway -A | grep conformance              → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

**No MetalLB pool IPs leaked** (.50–.254 pool clean; only the prod edge `.101`
remains). The rev5 follow-up (orphan-Service-on-namespace-delete) is resolved.

## Reproduction

A ~130-LOC standalone test (`conformance/aether_rev6_test.go`, **not committed**) —
a copy of the rev5 one-off with the env gate renamed `AETHER_REV6` and the impl
version bumped to `0.50.0` — in a `gateway-api` v1.5.1 checkout under
`/tmp/gateway-api-src/conformance` (its own Go module, `replace
sigs.k8s.io/gateway-api => ../`), run with `GOWORK=off` from inside `conformance/`.
Two subtests: `TestAetherRev6Mesh` (explicit mesh features) and
`TestAetherRev6Gateway` (features inferred from
`GatewayClass.status.supportedFeatures`). Same non-obvious requirements as
rev2–rev5: `ManifestFS = conformance.Manifests`, `AllowCRDsMismatch`, and — for the
inference path — empty `SupportedFeatures`/`ExemptFeatures` with
`EnableAllSupportedFeatures=false`. Budgets `GatewayMustHaveAddress=180s`,
`MaxTimeToConsistency=60s`, `RequestTimeout=10s`. The GATEWAY run takes ~37 min;
implement per-Gateway-listener data-plane route/cluster generation for resolved
HTTPRoute backends to make the bulk of it pass on the next run.
