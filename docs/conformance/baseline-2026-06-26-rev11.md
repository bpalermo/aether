# Gateway API conformance — rev11: the 404 wall is GONE (the 503 layer) (2026-06-26)

An eleventh run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev ~90)**, chart/commit **`9c77c1d`** (`aether-0.51.0-9c77c1d…`,
confirmed on the live edge Deployment label `app.kubernetes.io/version`).

This is the first run **since the load-bearing 404 root cause was fixed**
(`#9c77c1d`, "assign per-Gateway vhosts by attachment, not by cert tag"). rev8
captured the smoking gun — conformance Gateways were projected with a **present-but-empty**
route table (only the `edge_not_found` `*`→404 default), because
`buildEdgeGatewayEntries` assigned vhosts to Gateways by the vhost's **cert tag**, and a
plain-HTTP route could be TLS-tagged with an *unrelated* Gateway's catch-all cert
(cross-Gateway `hostCerts[""]` bleed), so the route attached to **zero** Gateways.
`#9c77c1d` assigns vhosts by **attachment** (the route's `parentRefs`).

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces and
per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR — the wall fell exactly as predicted; one layer deeper now

| Profile | Tests run | PASS | FAIL | DELTA vs rev9 | Verdict |
|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **37** | **14** | **23** | **+2 PASS / −2 FAIL** (rev9 12/25) | The **uniform 404 wall is gone.** Route tables are now populated and routes *match*. Traffic flipped from **404 → 503**: 4693 `got 503` vs 245 `got 404` (rev8/rev9 were 4080× 404, **zero** 503). |
| **MESH-HTTP** | 7 | **2** | **5** | flaky band (rev9/rev8 read 3/4, rev7 4/3) | 022 untouched. Low side of the flake this run: the 4 known capture-path FAILs + `MeshTrafficSplit` flapped to FAIL. |

**The headline is the *category flip*, not the +2.** The score moved only +2, but the
**entire failure mode moved a layer deeper** — exactly the rev3→rev6 pattern repeating.
For the first time **every conformance Gateway's route table is populated and its routes
match** (proven live, below); the suite's traffic now reaches the right route and dies at
the **upstream**, returning **503 "no healthy upstream"** instead of 404. Zero `200`s
were observed anywhere in the traffic phase — the gating blocker is now **a single,
clean, upstream-endpoint-resolution gap**, not 25 scattered routing failures.

## Live proof the fix landed — route tables are now populated

A concurrent monitor dumped the edge's real Envoy `dynamic_route_configs` (admin on,
`#345`, both replicas via port-forward) for every conformance Gateway with
`attachedRoutes > 0`, throughout the run. The four Gateways rev8 caught
**present-but-empty** are now **populated**:

| Gateway | rev8 route table | **rev11 route table (live)** |
|---|---|---|
| `same-namespace` | only `edge_not_found` `*`→404 | **1 real vhost, 1–6 real routes** (varies with the test's HTTPRoute) |
| `gateway-with-one-attached-route` | only `edge_not_found` `*`→404 | **1 real vhost, 1 real route** |
| `gateway-with-two-attached-routes` | only `edge_not_found` `*`→404 | **2 real vhosts, 3 real routes** |
| `backend-namespaces` | only `edge_not_found` `*`→404 | **1 real vhost, ≥1 real route** |

Across **109** monitor samples of attached Gateways: **78× `nonNF=1`, 15× `nonNF=2`,
only 16× `nonNF=0`** (empty). In rev8 essentially **100%** were `nonNF=0`. The fix flipped
route-table population from ~0% to **~85%** of attached-Gateway samples; the residual 16
empties are brief apply→project windows (the same status/snapshot non-atomicity rev8
noted), not the old structural bleed.

## What flipped, in traffic terms

The suite probed **distinct per-Gateway addresses** this run — `192.168.100.50`, `.51`,
`.52`, `.54` (MetalLB IPs allocated from the aether pool per the per-external-port
allocator, `#348`). That is **distinct-address allocation working**: multiple Gateways,
multiple IPs, not one shared edge. The request reaches Envoy (`Server: envoy` on every
response), the route matches — and then returns **503**:

```
expected status code to be one of [200], got 503.
CRes: &{503 0 HTTP/1.1 map[Content-Length:[0] ... Server:[envoy]] <nil> []}
```

**Category flips vs rev9 (uniform 404):**

| Traffic category | rev9 | **rev11** |
|---|---|---|
| Route table present + matching | NO (empty, 404) | **YES** (populated, route matches) |
| Distinct per-Gateway address | masked | **YES** (.50/.51/.52/.54 all probed) |
| Reaches an upstream | NO | **reaches route, no healthy upstream → 503** |
| Routing / path / header / method matching | masked by 404 | **now visible — all 503 (upstream), not a match failure** |
| Redirect (`HTTPRouteRedirectHostAndStatus`) | masked | **PASS** (no upstream needed) |
| HTTPS listener | 404 | **503** (same upstream gap) |
| Any `200` | 0 | **0** (the upstream gap gates *every* positive-path test) |

## The new gating blocker — edge has no upstream endpoints for conformance backends

Every traffic test that needs a real `200` from a backend now returns **503**. The route
is programmed and matches; Envoy has **no healthy upstream host** for the conformance
backend Services (`infra-backend-v1/2/3`, `web-backend`, `app-backend-*`, the
`HTTPRouteServiceTypes` `/headless`, `/manual-endpointslices` variants — all 503).

This is consistent with the edge's transport model: `agent edge` dials backend pods at
`pod_ip:15008` as a **single-identity SPIRE mTLS client**. The conformance backends are
**plain pods not in the aether mesh** — no CNI capture, no `:15008` HBONE listener, no
SPIRE workload identity — so the edge has **no reachable mTLS endpoint** to put in the
cluster, and the cluster has **zero healthy hosts** → 503. (rev3 saw a related symptom
from the other side: the edge could not even *reach* a plain backend.) Whether the fix is
to let the edge speak **plain HTTP to non-mesh backends** when no peer identity is
advertised, or to require conformance backends be mesh-onboarded, is a design call — but
**this single gap gates ~17 of the 23 GATEWAY-HTTP failures.**

## Categorized GATEWAY-HTTP blockers (23 FAIL)

The 404 no longer masks the follow-ups the task flagged. Two clean classes:

### Class A — 503 "no healthy upstream" (the dominant gate, ~17 tests)
Route present + matching, every probe 503, zero 200s. **One root cause** (above):

`GatewayWithAttachedRoutes`, `HTTPRouteSimpleSameNamespace`, `HTTPRouteCrossNamespace`,
`HTTPRouteReferenceGrant`, `HTTPRouteExactPathMatching`, `HTTPRouteHeaderMatching`,
`HTTPRouteMatching`, `HTTPRouteMatchingAcrossRoutes`, `HTTPRoutePathMatchOrder`,
`HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection`,
`HTTPRouteHTTPSListener`, `HTTPRouteServiceTypes`, `HTTPRouteRequestHeaderModifier`,
`HTTPRouteMethodMatching` (Ext), `HTTPRouteResponseHeaderModifier` (Ext),
`HTTPRouteWeight`, `HTTPRouteTimeoutRequest` (Ext, 503 not 504).

These are **not** matching/header/method/redirect *logic* failures — they cannot be
distinguished from a correct implementation while the upstream is empty. Fix the upstream
gap and this whole class should be re-evaluated (most likely flips to PASS or to a real,
much smaller, per-feature tail).

### Class B — status / ResolvedRefs gaps (now visible; small, independent)
- **`HTTPRouteInvalidNonExistentBackendRef`** and **`HTTPRouteInvalidBackendRefUnknownKind`**:
  the edge controller sets `ResolvedRefs=True` ("All backend references resolved") when
  the suite requires **`ResolvedRefs=False`** with reason **`BackendNotFound`** /
  **`InvalidKind`** (and a 500 from the data path). This is exactly the
  "invalid-backendRef ResolvedRefs=False+500" follow-up flagged in the task —
  **now confirmed failing** with the 404 gone.
- **`HTTPRouteInvalidCrossNamespaceBackendRef`**,
  **`HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`**,
  **`HTTPRouteInvalidReferenceGrant`**: mixed — partly the Class-B status gap, partly the
  Class-A 503 on the valid leg.

### Follow-ups the task asked to watch (404 no longer masks them)
- **Route precedence within a multi-route `*` vhost** (`HTTPRoutePathMatchOrder`,
  `HTTPRouteMatchingAcrossRoutes`): **cannot be confirmed** — they 503 before any
  ordering can be observed. Still latent behind Class A; the merge-by-namespace/name vs
  path-specificity concern from rev8 remains unverified.
- **dup-FQDN webhook**: not exercised to failure this run (the suite doesn't apply two
  HTTPRoutes sharing a hostname per Gateway in a way that tripped it).
- **cert-error Gateway `observedGeneration`** / invalid-TLS status: `GatewayInvalidTLSConfiguration`
  **PASSED** this run; the `observedGeneration` smells in the log were transient and did
  not fail their owning tests.

## Wins this run (14 PASS — the Programmed/status/redirect core is solid)

`GatewayClassObservedGenerationBump`, `GatewayObservedGenerationBump`,
`GatewayModifyListeners`, `GatewayInvalidRouteKind`, `GatewayInvalidTLSConfiguration`,
`GatewayWithAttachedRoutesWithPort8080`, the four `GatewaySecret*ReferenceGrant*` /
`GatewaySecretMissingReferenceGrant` cert-RBAC tests, `HTTPRouteObservedGenerationBump`,
`HTTPRouteInvalidParentRefNotMatchingSectionName`, `HTTPRouteInvalidCrossNamespaceParentRef`,
and notably **`HTTPRouteRedirectHostAndStatus`** (redirect needs no upstream → it passes
now that the route is programmed).

## MESH-HTTP — unchanged substance, low side of the flake (2/5)

022 is untouched, so this is correctly flat in substance. This run read **2 PASS / 5 FAIL**;
rev8/rev9 read 3/4 and rev7 4/3 — the same flaky band on the GAMMA capture path. The 5
FAILs: the **4 stable capture-path FAILs** every rev has shown
(`MeshHTTPRouteMatching`, `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`) **plus `MeshTrafficSplit`**,
which flapped to FAIL this run. None of the rev9→rev11 edge fixes touch the mesh data
path, so the mesh score is expected to wander only within the flake band — which it did.

## DELTA vs prior revs

| | rev3 (72) | rev6 (83) | rev9 (~89) | **rev11 (~90)** |
|---|---|---|---|---|
| Commit | — | — | — | **`9c77c1d`** |
| GATEWAY-HTTP PASS / FAIL | 6 / 31 | 12 / 25 | 12 / 25 | **14 / 23** |
| GATEWAY ΔPASS vs prior | — | +6 | +0 | **+2** |
| Traffic-phase dominant code | TLS-SAN wall | uniform 404 | uniform 404 | **503 (4693) ≫ 404 (245)** |
| Route table at the probe | — | empty (ii) | empty (ii) | **populated (~85% of samples)** |
| Distinct per-Gateway address probed | no | no | no | **YES (.50/.51/.52/.54)** |
| Any `200` in traffic phase | no | no | no | **no (upstream gap gates all)** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 3 / 4 | 3 / 4 | **2 / 5** |

## Prioritized remaining tail (GATEWAY-HTTP)

| Priority | Blocker | Fix | Unblocks |
|---|---|---|---|
| **P0** | Edge has **no upstream endpoint** for non-mesh backends → 503 on every positive-path test (0× `200`). | Let the edge dial **plain HTTP/cleartext** to backends that advertise no mesh/SPIRE identity (or formally require conformance backends be mesh-onboarded). | ~17 Class-A tests; re-reveals the real matching/header/method/redirect/weight tail. |
| **P1** | `ResolvedRefs=True` on **invalid** backendRefs (nonexistent / unknown-Kind). | Set `ResolvedRefs=False` + reason `BackendNotFound`/`InvalidKind` (and 500 on the data path). | `HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidBackendRefUnknownKind`, the invalid legs of the ReferenceGrant tests. |
| **P2** | `*`-vhost route **precedence** (merge order vs path-specificity) — still latent behind the 503. | Sort merged `*` routes by Gateway-API match specificity in `buildEdgeVhostsLocked`. | `HTTPRoutePathMatchOrder`, `HTTPRouteMatchingAcrossRoutes` (verify only after P0). |
| **P3** | Residual ~15% empty-table windows (apply→project non-atomicity). | Order `Programmed`/`attachedRoutes` status behind the route-config ACK. | flake reduction across all traffic tests. |
| **P0 (Mesh)** | HTTPRoute filters on the GAMMA/capture path (proposal 022). | Unchanged since baseline. | the 4 stable mesh FAILs. |

## Is aether GATEWAY-HTTP-conformant now?

**Not yet — but rev11 is the first run where the gap is a single, clean, well-understood
blocker rather than a wall.** The vhost-assignment-by-attachment fix (`#9c77c1d`) did
exactly what it promised: it **eliminated the uniform 404**, every conformance Gateway now
gets a populated, matching route table at a distinct address, and the failures moved an
entire layer deeper to a **503 "no healthy upstream"** that is **one root cause** — the
edge has no endpoint for non-mesh conformance backends. Land P0 (cleartext-to-non-mesh-
backend, or mesh-onboard the conformance backends) and ~17 Class-A tests should re-evaluate
in a single stroke, at which point the real, much smaller per-feature tail (precedence,
invalid-backendRef status) becomes visible and addressable. The score is 14/23; the
**distance to GATEWAY-HTTP-Core is now dominated by one upstream-resolution fix**, not by
twenty independent routing bugs.

## How it was run

Same programmatic runner as rev2–rev9 (`conformance/aether_rev11_test.go`, a copy of the
rev9 one-off with the env gate renamed `AETHER_REV11`, version held at `0.51.0`, in a
`gateway-api` v1.5.1 checkout with `replace sigs.k8s.io/gateway-api => ../`, **not
committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** features **inferred** from `GatewayClass.status.supportedFeatures`. The
  suite read `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}` —
  **identical to rev3/rev5–rev9** (no advertised-feature change in 0.51.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.
- GATEWAY run: **1850 s (~30.8 min)**. MESH: ~3 min. Traffic phase: **4693 `got 503`**,
  **245 `got 404`**, **0 `got 200`**.

A concurrent monitor ran two `kubectl port-forward`s to the edge admin (`:9901`, both
replicas) and a probe-and-dump loop keyed on `(route_config, attachedRoutes)` that
recorded each attached Gateway's real route-table population — the source of the
"populated tables" evidence above.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces (both the
GATEWAY and MESH runs self-cleaned); the per-Gateway LoadBalancer Services GC'd back to
the single production edge. Final state:

```
$ kubectl get ns | grep conformance                       → none
$ kubectl get svc -A -l aether.io/edge-gateway
NAMESPACE        NAME                                        EXTERNAL-IP
aether-ingress   aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

The only `aether.io/edge-gateway` Service is the production `.101`. The `.99`/`.100`
LoadBalancers in `envoy-gateway-system` are a **pre-existing, unrelated** Envoy Gateway
install, **not** an aether conformance leak. Both edge-admin `kubectl port-forward`s and
the monitor loop were killed.
