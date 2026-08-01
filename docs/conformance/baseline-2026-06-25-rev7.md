# Gateway API conformance — the hostname-less routing fix did NOT flip traffic, rev7 (2026-06-26)

A seventh run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.50.0 (rev 86)**, image/commit **`ddd3ee4`**. This run measures the
**3-PR hostname-less routing fix** (#341 / #342 / #343) that was expected to flip
[rev6](./baseline-2026-06-25-rev6.md)'s uniform-404 traffic wall.

In rev6 every traffic test reached the traffic phase and then returned a **uniform
404** (4080/4080), diagnosed as "the edge resolves backends in *status* but does not
program the Envoy route/cluster for them." The fix landed since:

- **#341** — the cache builds a catch-all `*` vhost for hostname-less HTTPRoutes
  (`buildEdgeVhostsLocked`), with their backends scoped into the demand set.
- **#342** — the reconciler stopped filtering hostname-less vhosts before the cache
  (dropped the `len(vh.Hosts)==0` skip).
- **#343** — the new `*` vhost collided with the route table's existing
  `edge_not_found` `*` 404-default (Envoy NACK: "Only a single wildcard domain is
  permitted"); fixed by merging the 404 into the single `*` vhost
  (`appendEdgeCatchAll404`).

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces
and per-Gateway LoadBalancer Services were cleaned up afterward (Cleanup — clean,
no pool leak).

## TL;DR — the headline did NOT move

| Profile | Setup | Tests run | PASS | FAIL | SKIP (in-profile) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** (north-south) | ✅ passed | **37** | **12** | **25** | 0 | **FLAT vs rev6 (12 → 12).** The expected traffic-test flip **did not happen in the suite**: the traffic phase still returns a **uniform 404 — 4080/4080, bit-for-bit identical to rev6** (zero 503s, zero TLS errors, zero other codes). The same 12 pass, the same 25 fail, the same dominant blocker. **However**, the fix is **demonstrably live and correct in isolation**: a controlled repro of the byte-identical `HTTPRouteSimpleSameNamespace` route shape (hostname-less, `infra-backend-v1:8080`, no matches) on rev 86 attaches in **~4 s** and returns **503** (route matched, cluster has no mesh endpoints) — **never 404** — including under 3 concurrent per-Gateway-IP Gateways with PathPrefix + Exact routes. So the routing logic works; the suite just doesn't observe it. The binding constraint is no longer the routing *logic* (rev6's diagnosis) but a **suite-observable lifecycle/convergence gap** under the suite's rapid per-test apply→probe→delete churn. |
| **MESH-HTTP** (east-west / GAMMA) | passed | 7 | **4** | **3** | 0 | **Converged read (good side of the flaky band).** The 3 stable capture-path FAILs (`MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`) — identical to rev3/rev5/rev6/baseline. 022 untouched. |

**Headline delta:** GATEWAY-HTTP **6/31 (rev3)** → **7/30 (rev5)** → **12/25
(rev6)** → **12/25 (rev7)** — **+0 vs rev6.** The fix is deployed and validated in
isolation, but it **does not move the conformance needle**: the run reproduced
rev6's exact 12/25 with an identical 4080×404 traffic wall. MESH-HTTP read 4/3 (the
converged end of the flaky band; substance unchanged).

> **The fix is real, but rev7's central finding is a NEGATIVE result.** On the
> deployed rev-86 build, a conformance-shaped hostname-less HTTPRoute, hit via its
> per-Gateway IP, **does** get a programmed route (503, not 404) within ~4 s — in
> isolation and under 3-Gateway concurrency. Yet the full suite still 404s every
> traffic probe, unchanged from rev6. The binding constraint has moved from a
> static routing-logic bug to a **dynamic lifecycle gap the suite exposes and a
> hand-applied route does not.**

## How it was run

Same programmatic runner as rev2–rev6 (`conformance/aether_rev7_test.go`, a copy of
the rev6 one-off with the env gate renamed `AETHER_REV7` and the version held at
`0.50.0`, in a `gateway-api` v1.5.1 checkout with
`replace sigs.k8s.io/gateway-api => ../`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}`
  — **identical to rev3/rev5/rev6** (no advertised-feature change in 0.50.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification,
  HTTPRouteMethodMatching, HTTPRouteRequestTimeout}`), as in rev2–rev6.
- Budgets (rev6 style): `GatewayMustHaveAddress=180s`,
  `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.

The GATEWAY run finished in **2210 s (~36.8 min)** (≈ rev6's 2209 s — same shape).
MESH finished in 164 s.

## DELTA vs rev3, rev5, rev6, and the original baseline

| | Baseline (0.42.0, rev 68) | rev3 (0.45.0, rev 72) | rev5 (0.49.0, rev 82) | rev6 (0.50.0, rev 83) | **rev7 (0.50.0, rev 86)** | rev6→rev7 |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP Setup** | ❌ blocked | ✅ | ✅ | ✅ | ✅ | = |
| **GATEWAY-HTTP tests run** | 0 | 37 | 37 | 37 | **37** | = |
| **GATEWAY-HTTP PASS** | 0 | 6 | 7 | 12 | **12** | **+0** |
| **GATEWAY-HTTP FAIL** | 0 | 31 | 30 | 25 | **25** | **+0** |
| Gateways get DISTINCT addresses | no | shared `.101` | DISTINCT | DISTINCT | **DISTINCT** (.50/.51/.52…) | = |
| `HTTPRoute.status.parents` written | no | no | no | YES (#339) | **YES** | = |
| Traffic tests reach the traffic phase | no | yes | no | YES | **YES** | = |
| **Traffic-phase result** | n/a | TLS-SAN wall | route-status wall | **uniform 404 (4080/4080)** | **uniform 404 (4080/4080)** | **= (unchanged)** |
| Hostname-less route programmed (in isolation) | — | — | — | **NO (rev6 wall)** | **YES — 503 in ~4 s, repro'd** | ✅ **fix live** |
| Dominant blocker | Gw has no addr | bare-IP TLS-SAN | route status | edge programs no route for backends | **suite-observable lifecycle/convergence gap (route present in isolation, absent during suite churn)** | **moved** |
| Orphan per-Gw LB Service after teardown | n/a | n/a | 5 (manual) | 0 (self) | **0 (self)** | = |
| **MESH-HTTP PASS / FAIL** | 4 / 3 | 4 / 3 | 3 / 4* | 3 / 4* | **4 / 3** (converged) | flat |

## Did the traffic tests flip? (the rev7 question) — NO, but the fix is live

**The suite result did not flip. The fix did, in isolation. Both are directly
observed, and the gap between them is the rev7 finding.**

**1. The suite: still a uniform 404, identical to rev6.** Every traffic test issues
`GET http://192.168.100.5x/…` with an **empty Host header** to the correct,
distinct per-Gateway address, and the edge Envoy answers **404**:

```
expected status code to be one of [200], got 404.
  CRes: &{404 0 HTTP/1.1 map[... Server:[envoy]] ...}
```

This is uniform: **4080/4080 traffic-phase failures are `got 404`** — bit-for-bit
the rev6 tally (zero 503s, zero TLS/x509 errors, zero other codes). A 404 from the
edge means **no route matched** (the request fell through to the
`edge_not_found`/merged-`*` 404 default), i.e. the per-Gateway route table had no
real route at probe time.

**2. Isolation: the fix works — 503, not 404, in ~4 s.** A controlled live repro on
the same rev-86 build, applying the **byte-identical** `HTTPRouteSimpleSameNamespace`
shape (`parentRefs: [{name: same-namespace}]`, no hostnames, single
`backendRef infra-backend-v1:8080`, no matches) against a per-Gateway IP:

- Gateway gets `.50`; route reports `Accepted=True` / `ResolvedRefs=True`.
- Probing `http://.50/` (and `/echo`, and with arbitrary Host) returns a **stable
  503** — *route matched, cluster has no mesh endpoints* (the conformance backend
  is not mesh-registered) — **never 404**.
- The route converges **within ~4 s** of a fresh `Gateway`+`HTTPRoute` apply, then
  serves 503 continuously for 50 s+ (no flap, no 404 window).
- Under **3 concurrent Gateways** (distinct IPs `.50`/`.51`/`.52`), one with a
  `PathPrefix: /` rule and one with an `Exact: /exact` rule, **all three** return
  503 on both `/` and `/exact` — the catch-all `*` vhost, the path-specific match,
  and the merged 404 default all behave correctly side-by-side.

The edge controller debug log confirms the projection path: adding a hostname-less
route moves `projected gateway-api routes virtualHosts:1 → 2` (the `+1` is the
hostname-less repro vhost), i.e. **#342 no longer drops it** and **#341/#343 build
the `*` catch-all + merged 404**. So the rev6 root cause (no data-plane route for a
hostname-less backend) **is fixed and validated**.

**3. The contradiction = the rev7 finding.** The suite 404s where every controlled
repro 503s, on the same binary. The difference is the **lifecycle**: the suite
stands up **5+ Gateways concurrently** and, per test, **creates a route → probes
within the 10–60 s window → deletes it**, repeatedly, while per-Gateway
`LoadBalancer` Services and listener-port allocations churn (the edge log shows
`create per-Gateway Service … already exists` races during the run). A 404 — not a
503 — means the probed Gateway's **listener was serving a route config that did not
yet (or no longer) contain the route** at that instant. A hand-applied route, given
its few seconds to settle, never hits that window. So the binding constraint is now
a **dynamic convergence / per-Gateway-lifecycle gap**, not the static routing bug
rev6 found.

## GATEWAY-HTTP — the 12 PASS (rev7, identical set to rev6)

```
GatewayClassObservedGenerationBump
GatewayInvalidRouteKind
GatewayModifyListeners
GatewayObservedGenerationBump
GatewaySecretInvalidReferenceGrant
GatewaySecretMissingReferenceGrant
GatewaySecretReferenceGrantAllInNamespace
GatewaySecretReferenceGrantSpecific
GatewayWithAttachedRoutesWithPort8080
HTTPRouteInvalidCrossNamespaceParentRef
HTTPRouteInvalidParentRefNotMatchingSectionName
HTTPRouteObservedGenerationBump
```

The 12 are all **status/condition** tests that never depend on a successful traffic
probe. No path-matching or precedence test passed — but **not** because of route
ordering within the `*` vhost: every path-matching traffic test (`ExactPathMatching`,
`PathMatchOrder`, `Matching`, `HeaderMatching`) fails at the **same uniform 404** as
the rest, i.e. it never gets a 200 to even compare path precedence. The known
follow-up (the merged `*` routes are in namespace/name order, not Gateway-API
path-specificity order) is therefore **not yet observable** — it is masked behind
the 404 wall and cannot be assessed until traffic lands.

## GATEWAY-HTTP — the 25 FAIL, categorized (rev7)

CORE (22): `GatewayInvalidTLSConfiguration`, `GatewayWithAttachedRoutes`,
`HTTPRouteCrossNamespace`, `HTTPRouteExactPathMatching`, `HTTPRouteHTTPSListener`,
`HTTPRouteHeaderMatching`, `HTTPRouteHostnameIntersection`,
`HTTPRouteInvalidBackendRefUnknownKind`, `HTTPRouteInvalidCrossNamespaceBackendRef`,
`HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidReferenceGrant`,
`HTTPRouteListenerHostnameMatching`, `HTTPRouteMatching`,
`HTTPRouteMatchingAcrossRoutes`, `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`,
`HTTPRoutePathMatchOrder`, `HTTPRouteRedirectHostAndStatus`, `HTTPRouteReferenceGrant`,
`HTTPRouteRequestHeaderModifier`, `HTTPRouteServiceTypes`,
`HTTPRouteSimpleSameNamespace`, `HTTPRouteWeight`.
EXTENDED (3): `HTTPRouteMethodMatching`, `HTTPRouteResponseHeaderModifier`,
`HTTPRouteTimeoutRequest`.

| # | Blocker | Tests | Category | Notes |
|---|---|---|---|---|
| **1 (DOMINANT)** | **Suite-observable lifecycle/convergence gap** — the per-Gateway route table is empty (404) at the suite's probe instant, although the route is present and serving (503) in any isolated/concurrent hand-applied repro on the same build. | **~21 — every HTTPRoute traffic test** | **NEW shape of the binding constraint.** Not a routing-logic bug (that is fixed, #341/#342/#343, validated). The suspect surface: per-Gateway `LoadBalancer` Service / listener-port-allocation churn under rapid create→probe→delete across 5+ concurrent Gateways (edge log shows `…Service already exists` races during the run), and/or RDS push latency for the just-created per-Gateway route config vs the suite's traffic-retry window. **Top priority — instrument the run (config_dump the probed Gateway's route config at 404 time) to localise.** |
| 2 | `GatewayInvalidTLSConfiguration` malformed/terminate-cert listener still reports `ResolvedRefs=True`. | 1 | status edge-case | Same residual as rev5/rev6 (#331/#337 cover most cert states; this specific malformed/terminate case isn't detected). Independent of #1. |
| 3 | `GatewayWithAttachedRoutes` (non-port-8080) — `attachedRoutes` count. | 1 | status accounting | The port-8080 variant passes; this rides on full route-attachment accounting. Likely independent of #1. |
| 4 | dup-FQDN webhook vs split-across-routes. | `HTTPRouteMatchingAcrossRoutes` (1) | design | Unchanged design conflict. |
| 5 | Invalid-backendRef route-status negatives (`…InvalidBackendRefUnknownKind`, `…InvalidNonExistentBackendRef`, `…InvalidCrossNamespaceBackendRef`, `…InvalidReferenceGrant`, `…PartiallyInvalidViaInvalidReferenceGrant`, `HTTPRouteReferenceGrant`). | up to 6 | mixed status+traffic | Pass the parent gate but assert specific `ResolvedRefs` conditions and/or traffic; fail on the 404 and/or backend-condition specifics. Several flip with #1 + a `ResolvedRefs=False` reason for bad backends. |

## EXTENDED (3 FAIL, all gated by #1)

`HTTPRouteMethodMatching`, `HTTPRouteResponseHeaderModifier`,
`HTTPRouteTimeoutRequest` — all reach the traffic phase and fail at the same uniform
404. The advertised extended features aren't in question; the tests can't get past
the 404 to exercise them. (`GatewayWithAttachedRoutesWithPort8080` passes.) 89
extended tests are correctly **skipped** as unsupported features (BackendTLSPolicy,
client-cert, mirror, host/path-rewrite, alt redirect codes, ListenerSet, …).

## MESH-HTTP — 4/3 (converged; 022 untouched)

Profile report (this run): `pass=4 fail=3`. Failed set:
`MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`,
`MeshHTTPRouteWeight` — the **3 stable** capture-path FAILs, **identical to
rev3/rev5/rev6/baseline**. This run landed on the **good side** of the flaky
boundary (both `MeshHTTPRouteMatching` and `MeshTrafficSplit` passed). HTTPRoute
redirect/header-modifier filters translate on the **edge** path but not on the GAMMA
transparent-capture path, and weighted distribution doesn't converge on capture.
**Proposal 022 is not implemented**, so this is correctly flat; #341/#342/#343 are
edge route-table fixes and don't touch the mesh data path.

## Prioritized remaining blockers

| Priority | Blocker | Profile | Impact |
|---|---|---|---|
| **P0 (GW) — NEW dominant** | **Close the suite-observable lifecycle/convergence gap.** First *measure* it: run a single HTTPRoute traffic test with a config_dump of the probed per-Gateway listener's route config captured at the 404, plus the RDS version timeline, to confirm whether the route config is empty/stale or pointing at the wrong listener under per-Gateway-Service churn. Then fix the race (idempotent per-Gateway Service create — the `already exists` log is a smell — and/or ensure the per-Gateway RDS push lands before the Gateway is `Programmed`/addressed). | GATEWAY-HTTP | **Unblocks ~21 traffic tests** — the whole HTTPRoute traffic surface. The routing logic is already correct (validated 503-in-isolation); this is the one remaining thing between rev7 and a large jump. **Single highest-leverage item.** |
| **P0 (Mesh)** | **Apply HTTPRoute filters on the GAMMA / transparent-capture path (proposal 022).** | MESH-HTTP | `MeshHTTPRouteRedirectHostAndStatus` + `MeshHTTPRouteRequestHeaderModifier` → PASS (~6/7). Unchanged since baseline. |
| **P1** | **After P0(GW): re-assess `*`-vhost route precedence.** Once traffic lands, the known follow-up surfaces — the merged `*` routes are emitted in namespace/name order, not Gateway-API path-specificity order, which will fail `HTTPRoutePathMatchOrder` / `HTTPRouteMatching` even with a programmed route. Sort routes by match specificity in `buildEdgeVhostsLocked`. | GATEWAY-HTTP | The next wall after the convergence gap; cheap and well-scoped. |
| **P2 (quick wins)** | `GatewayInvalidTLSConfiguration` malformed/terminate-cert detection (1); `ResolvedRefs=False`/reason for invalid backendRefs. | GATEWAY-HTTP | small, independent of P0(GW). |
| **P3** | dup-FQDN webhook vs `HTTPRouteMatchingAcrossRoutes` (design); stabilise GAMMA capture. | both | tail. |

## Is aether GATEWAY-HTTP-conformant now? (honest assessment)

**No — and rev7 is the run where the expected jump did NOT arrive.** The
hostname-less routing fix (#341/#342/#343) is **deployed and demonstrably correct**:
on rev 86, a conformance-shaped hostname-less HTTPRoute attaches in ~4 s and serves
its route (503, not 404) in isolation and under 3-Gateway concurrency, and the edge
projects it (`virtualHosts` increments, `*` catch-all + merged 404 built). rev6's
diagnosis ("the edge programs no route for the backend") is genuinely resolved.

**But the conformance suite still scores 12/25 with a bit-for-bit identical
4080×404 traffic wall.** The honest read: the binding constraint **moved but did not
shrink** — from a *static routing-logic* bug (rev6) to a *dynamic
lifecycle/convergence* gap that only the suite's rapid, concurrent,
multi-Gateway create→probe→delete cycling exposes. A 404 (no route) rather than a
503 (route present, no endpoints) at probe time is the tell: the per-Gateway route
config is transiently empty/stale during the suite's churn, even though it is
correct and stable for a hand-applied route. Until that gap is **measured**
(config_dump at the 404) **and closed**, the answer to "conformant?" is **12/37 —
not conformant, and — unlike rev6 — the fix that was expected to unlock the traffic
surface did not, because the wall is now timing/lifecycle, not logic.**

## Cleanup (Phase 2 LB-leak check)

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces, and the
per-Gateway LoadBalancer Services GC **self-resolved** (as in rev6 — no manual
cleanup required). The temporary `rev7-repro` diagnostic namespace was also deleted.
Final state verified:

```
$ kubectl get ns | grep -E 'conformance|rev7-repro'   → none
$ kubectl get gateway -A | grep -E 'conformance|rev7' → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

**No MetalLB pool IPs leaked** (.50–.254 pool clean; only the prod edge `.101`
remains).

## Reproduction

A ~130-LOC standalone test (`conformance/aether_rev7_test.go`, **not committed**) — a
copy of the rev6 one-off with the env gate renamed `AETHER_REV7` (version held at
`0.50.0`) — in a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api-src/conformance`
(its own Go module, `replace sigs.k8s.io/gateway-api => ../`), run with `GOWORK=off`
from inside `conformance/`. Two subtests: `TestAetherRev7Mesh` (explicit mesh
features) and `TestAetherRev7Gateway` (features inferred from
`GatewayClass.status.supportedFeatures`). Same non-obvious requirements as rev2–rev6:
`ManifestFS = conformance.Manifests`, `AllowCRDsMismatch`, empty
`SupportedFeatures`/`ExemptFeatures` with `EnableAllSupportedFeatures=false` for the
inference path. Budgets `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
`RequestTimeout=10s`. The GATEWAY run takes ~37 min.

**Next:** instrument a single HTTPRoute traffic test to config_dump the probed
per-Gateway listener's route config at the 404 and capture the RDS version timeline —
to localise the convergence/lifecycle gap (per-Gateway Service create race vs RDS
push latency) that keeps the suite at 12/25 even though the routing fix is live.
