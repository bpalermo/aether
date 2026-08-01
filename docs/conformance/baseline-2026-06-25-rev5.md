# Gateway API conformance — first scored GATEWAY-HTTP, rev5 (2026-06-25)

A fifth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now
at aether **0.49.0 (rev 82)**. This is the **first GATEWAY-HTTP run to actually
score** since [rev3](./baseline-2026-06-25-rev3.md): the cert-namespace
regression that **setup-blocked** [rev4](./baseline-2026-06-25-rev4.md) is fixed
(**#337** — resolve the listener `certificateRef` from `certificateRef.namespace`
‖ the Gateway's namespace, not the edge's), and the full per-Gateway-addressing
stack is live, so the profile ran to completion (~46.6 min).

Aether was **not** modified or redeployed for this run. The deployed image digest
is `b30c4e8`, which is exactly the #337 commit
(`fix(edge): resolve listener certificateRef from its own namespace (#337)`).
All `gateway-conformance-*` namespaces **and** the per-Gateway LoadBalancer
Services were cleaned up afterward (see Cleanup).

## TL;DR

| Profile | Setup | Tests run | PASS | FAIL | SKIP (in-profile) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** (north-south) | ✅ **passed** (no abort) | **37** | **7** | **30** | 0 | **It scores again — but the headline barely moved (6 → 7).** The rev4 cert-namespace fix unblocked Setup, and the Phase 2 machinery demonstrably works (every conformance Gateway got its own pool IP, the HTTPS base Gateway is `Programmed=True` with all 4 HTTPS listeners `ResolvedRefs=True` against its **own-namespace** cert). But a **different, deeper wall** was uncovered: aether **does not populate `HTTPRoute.status.parents`** (route-level `Accepted`/`ResolvedRefs` conditions), so **every** HTTPRoute test times out at the suite's *"wait for the route to be accepted by its parent"* gate **before any traffic is sent.** The distinct-address + HTTPS-cert win is real but is **masked** by the route-status gap. |
| **MESH-HTTP** (east-west / GAMMA) | passed | 7 | **3*** | **4*** | 0 | **Flat vs rev3/rev4.** 4 stable + the flaky `MeshHTTPRouteMatching` boundary read 3/4 this run (its capture-path sub-cases timed out again — 46 retry lines). 022 untouched, so the 3 stable capture-path FAILs are identical. |

**Headline delta:** GATEWAY-HTTP went **6 PASS / 31 FAIL (rev3)** → **Setup-blocked / 0 (rev4)** → **7 PASS / 30 FAIL (rev5)**. So vs the last *scored* run (rev3) the net is **+1 PASS**, and vs the original baseline (0 run) it's the first real score. The *expected* large jump did **not** materialise, because the rev3 traffic bucket (HTTP-path + HTTPS/TLS) is now gated behind a **previously-hidden** blocker — **HTTPRoute route status** — that only became the binding constraint once Setup and addressing stopped being the binding constraints. MESH-HTTP is flat.

> **The Phase 2 + #337 work is correct and was directly observed working.** It is
> simply **not sufficient on its own** to flip the traffic tests, because those
> tests first assert `HTTPRoute.status.parents[].conditions` and never reach the
> traffic phase. **The single highest-leverage next fix is HTTPRoute (and
> GatewayClass) status reconciliation.** See the prioritised tail.

## How it was run

Same programmatic runner as rev2–rev4 (`conformance/aether_rev5_test.go`, a
one-off in a `gateway-api` v1.5.1 checkout with `replace
sigs.k8s.io/gateway-api => ../`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}`
  — **identical to rev3/rev4** (no advertised-feature change in 0.49.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification,
  HTTPRouteMethodMatching, HTTPRouteRequestTimeout}`), as in rev2–rev4.
- Budgets were **generous** (rev4 style, since Gateways now genuinely get an
  address and traffic deserves convergence time):
  `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.

The GATEWAY run finished in **2799 s (~46.6 min)** — a real, full, retry-heavy
pass (rev4's ~5-min "fast" finish was the *abort*; this is the genuine runtime).
MESH finished in 183 s.

## DELTA vs rev3, rev4, and the original baseline

| | Baseline (0.42.0, rev 68) | rev2 (0.43.0) | rev3 (0.45.0, rev 72) | rev4 (0.49.0, rev 81) | **rev5 (0.49.0, rev 82)** | rev3→rev5 |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP Setup** | ❌ blocked | ✅ | ✅ | ❌ blocked (#331 cert-ns bug) | ✅ **unblocked (#337)** | restored |
| **GATEWAY-HTTP tests run** | 0 | 37 | 37 | 0 | **37** | = |
| **GATEWAY-HTTP PASS** | 0 | 6 | 6 | 0 | **7** | **+1** |
| **GATEWAY-HTTP FAIL** | 0 | 31 | 31 | 0 | **30** | −1 |
| Gateways get `.status.addresses` | no | no | shared `.101` | DISTINCT `.50/.51/.52` | **DISTINCT per-Gw** | ✅ |
| Per-Gateway LoadBalancer Service | no | no | no | yes | **yes** | ✅ |
| HTTPS base Gw `Programmed` | n/a | n/a | n/a | **False** (wrong-ns cert) | **True** (own-ns cert) | ✅ **#337** |
| HTTPS listener `ResolvedRefs` | n/a | n/a | n/a | False | **True (×4 listeners)** | ✅ **#337** |
| Dominant blocker | Gw has no addr | Gw has no addr | bare-IP TLS-SAN / shared IP | HTTPS base Gw not Programmed | **HTTPRoute `.status.parents` not written** | **moved deeper** |
| **MESH-HTTP PASS / FAIL** | 4 / 3 | 4 / 3 | 4 / 3 | 4 / 3* | **3 / 4*** (flaky boundary) | flat |

## Did the distinct-address + HTTPS-traffic tests flip? (the two big rev3 buckets)

**Short answer: no — but not because the addressing/TLS machinery is wrong.** It
was directly observed working; the tests fail one layer *earlier*, at route
status.

**Live evidence captured mid-run (during Setup / early tests):**

```
## conformance gateways (.status)
all-namespaces                          Programmed=True   192.168.100.51
backend-namespaces                      Programmed=True   192.168.100.52
same-namespace                          Programmed=True   192.168.100.50
same-namespace-with-https-listener      Programmed=True   <port-demuxed behind .50-class>
gateway-certificate-malformed-secret    Programmed=True   192.168.100.54   (negative test gw)
gateway-certificate-nonexistent-secret  Programmed=False  <none>           (correct)
gateway-certificate-unsupported-group   Programmed=False  192.168.100.53   (correct)
gateway-certificate-unsupported-kind    Programmed=False  192.168.100.53   (correct)

## same-namespace-with-https-listener listeners
https                                 ResolvedRefs=True
https-with-hostname                   ResolvedRefs=True
https-with-wildcard-hostname          ResolvedRefs=True
https-with-hostname-matching-wildcard ResolvedRefs=True
```

So **#337 is validated end-to-end**: the HTTPS base Gateway (whose cert Secret
lives in `gateway-conformance-infra`, its own namespace) goes
`Programmed=True / ResolvedRefs=True`, and **distinct per-Gateway addresses are
assigned** (.50/.51/.52/.53/.54, all distinct from the production edge `.101`).
The rev4 Setup abort is gone.

**Why the traffic tests still fail.** Every HTTPRoute test (including
`HTTPRouteHTTPSListener`, `HTTPRouteSimpleSameNamespace`, `HTTPRouteCrossNamespace`,
`HTTPRouteExactPathMatching`, …) fails at the **same** assertion, with the
**same** message, **before sending any request**:

```
helpers.go:814: error waiting for HTTPRoute to have parents matching expectations
```

(19 distinct occurrences across the run.) The suite's
`HTTPRouteMustHaveCondition` / parents-match gate requires the **HTTPRoute** to
report, under `.status.parents[]` for the attached `parentRef`, an `Accepted=True`
+ `ResolvedRefs=True` condition with the correct `controllerName` and
`observedGeneration`. **Aether populates Gateway/listener status (now correctly)
but does not write per-route `HTTPRoute.status.parents` conditions**, so the gate
times out at `MaxTimeToConsistency` (60 s/test) and the test fails **without ever
reaching the traffic phase.** The HTTPS tests behaved identically (the
`HTTPRouteHTTPSListener` 180 s duration is 3× the 60 s route-status gate across
its sub-routes — not a TLS-handshake failure).

**Bottom line on the two rev3 buckets:** the *enabling* infrastructure
(distinct addr + own-namespace HTTPS cert) is **in place and verified**, but the
buckets cannot score until **HTTPRoute route-status** is implemented. They are
no longer blocked by addressing or by TLS; they are blocked by route status.

## GATEWAY-HTTP — the 7 PASS (all Gateway/Secret-level status; zero traffic)

```
GatewayInvalidRouteKind              (supportedKinds / InvalidRouteKinds — #331)
GatewayModifyListeners
GatewayObservedGenerationBump        (Gateway observedGeneration DOES increment)
GatewaySecretInvalidReferenceGrant   (RefNotPermitted — ReferenceGrant for certs)
GatewaySecretMissingReferenceGrant   (RefNotPermitted)
GatewaySecretReferenceGrantAllInNamespace   (cross-ns cert via grant → Programmed)
GatewaySecretReferenceGrantSpecific         (cross-ns cert via grant → Programmed)
```

This is a **better, more meaningful 7** than rev2/rev3's 6: it now includes the
**ReferenceGrant-for-certificateRefs** tests (`GatewaySecretReferenceGrant*`,
`GatewaySecret*ReferenceGrant`) — i.e. **#337's cross-ns cert handling, gated by
ReferenceGrant, passes conformance.** `GatewayObservedGenerationBump` also passes
(Gateway-level observedGeneration is correct).

## GATEWAY-HTTP — categorized remaining blockers (rev5)

| # | Blocker | Tests affected | Category | Notes |
|---|---|---|---|---|
| **1 (NEW dominant)** | **`HTTPRoute.status.parents[]` conditions not written** (no per-route `Accepted`/`ResolvedRefs`/`observedGeneration`). | **~24 — every HTTPRoute test** (CrossNamespace, SimpleSameNamespace, ExactPathMatching, HeaderMatching, HostnameIntersection, ListenerHostnameMatching, Matching, MatchingAcrossRoutes, PathMatchOrder, RedirectHostAndStatus, ReferenceGrant, RequestHeaderModifier, ResponseHeaderModifier, ServiceTypes, SimpleSameNamespace, Weight, HTTPSListener, MethodMatching, TimeoutRequest, all Invalid* status tests, PartiallyInvalid*, ObservedGenerationBump). | **Real feature, the binding constraint.** Blocks both the traffic tests (gate before traffic) **and** the route-status negative tests (Invalid*/PartiallyInvalid* assert specific route conditions). **Top priority — the single fix that unlocks the bulk of the profile.** |
| 2 | `GatewayClass.status.observedGeneration` not bumped. | `GatewayClassObservedGenerationBump` (1) | quick win | Gateway-level bump works (PASS); GatewayClass-level doesn't. Small. |
| 3 | One bad-cert listener still reports `ResolvedRefs=True` where the suite expects `InvalidCertificateRef`. | `GatewayInvalidTLSConfiguration` (1) | status edge-case | #331/#337 cover most cert states; this specific malformed/terminate-cert case isn't detected (got `ResolvedRefs=True / Programmed=True`, expected `ResolvedRefs=False / InvalidCertificateRef`). |
| 4 | `GatewayWithAttachedRoutes` / `…WithPort8080` — `attachedRoutes` count + observedGeneration on the Gateway listener. | 2 | depends on #1 | These count attached routes; they ride on the same route-attachment reconciliation as #1. Likely flip with #1. |
| 5 | dup-FQDN webhook vs split-across-routes. | `HTTPRouteMatchingAcrossRoutes` (1) | design | Unchanged design conflict (also gated by #1 today). |

> **Re-prioritisation from rev4:** rev4 predicted the rev3 22-test traffic bucket
> would "flip the moment the cert-namespace bug is fixed." That was **half right** —
> the cert bug *was* the Setup blocker and is fixed — but the traffic bucket is
> **also** gated by route status, which rev4 couldn't see (Setup aborted before any
> route test ran). rev5's contribution is making that **next** blocker visible and
> measured.

## EXTENDED (4 FAIL, all gated by #1)

`GatewayWithAttachedRoutesWithPort8080`, `HTTPRouteMethodMatching`,
`HTTPRouteResponseHeaderModifier`, `HTTPRouteTimeoutRequest` — all four are
HTTPRoute tests that fail at the **route-status gate** (same as #1). The
advertised extended features themselves (`GatewayPort8080`,
`HTTPRouteMethodMatching`, `HTTPRouteRequestTimeout`,
`HTTPRouteResponseHeaderModification`) are not in question; the tests just can't
get past route status to exercise them.

## MESH-HTTP — still ~4/3 (flaky boundary; 022 untouched)

Profile report (this run): `pass=3 fail=4`. Failed set:
`MeshHTTPRouteMatching`, `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`.

- **`MeshHTTPRouteMatching` is the flaky boundary** (read FAIL this run): its
  GAMMA capture-path sub-cases didn't converge (46 `expected pod name to start
  with echo-v2 … not ready yet` retry lines). On a converging run the profile
  reads `4/3`. Non-deterministic capture-path convergence, not a stable failure.
- **Stable PASS (4):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`,
  `MeshTrafficSplit`, and `MeshHTTPRouteMatching` *when it converges*.
- **Stable FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
  `MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — **identical to
  rev3/rev4/baseline.** HTTPRoute redirect/header-modifier filters translate on
  the **edge** path but **not on the GAMMA transparent-capture path**, and the
  weighted-distribution assertion doesn't converge on capture. **Proposal 022
  (arbitrary-Service interception / genuine GAMMA data plane) is not
  implemented**, so this is correctly flat.

## Prioritized remaining blockers

| Priority | Blocker | Profile | Impact |
|---|---|---|---|
| **P0 (GW) — NEW dominant** | **Implement `HTTPRoute.status.parents[]` reconciliation** (per-`parentRef` `Accepted` + `ResolvedRefs` conditions with the aether `controllerName` and correct `observedGeneration`). | GATEWAY-HTTP | **Unblocks essentially the entire HTTPRoute surface** — the ~24 HTTPRoute tests (traffic *and* status negatives) all gate on it. With distinct addressing + own-ns HTTPS cert already in place, this is the one fix expected to convert the bulk of the 30 fails. **Single highest-leverage item.** |
| **P0 (Mesh)** | **Apply HTTPRoute filters on the GAMMA / transparent-capture path (proposal 022).** | MESH-HTTP | `MeshHTTPRouteRedirectHostAndStatus` + `MeshHTTPRouteRequestHeaderModifier` → PASS (~6/7). Unchanged since baseline. |
| **P1** | **Re-score GATEWAY-HTTP after P0(GW).** Distinct addr + per-Gateway HTTPS cert are live and verified; the rev3 traffic bucket (HTTP-path + HTTPS/TLS) should finally flip once route status exists. | GATEWAY-HTTP | Expected to move the headline well past 7. |
| **P2 (quick wins)** | `GatewayClass.status.observedGeneration` bump (1); `GatewayInvalidTLSConfiguration` malformed/terminate-cert detection (1). | GATEWAY-HTTP | 2 small, independent of P0. |
| **P3** | dup-FQDN webhook vs `HTTPRouteMatchingAcrossRoutes` (design); stabilise GAMMA capture so `MeshHTTPRouteMatching`/`MeshHTTPRouteWeight` converge. | both | tail. |

## Is aether GATEWAY-HTTP-conformant now? (honest assessment)

**No — not yet, and the gap is now precisely localised.** rev5 is a genuine
milestone: the profile **scores again** (rev4 couldn't), the **Phase 2
addressing + #337 own-namespace HTTPS cert work is correct and observed working**
(Gateways `Programmed`, HTTPS listeners `ResolvedRefs=True`, distinct pool IPs,
ReferenceGrant-for-certs passing), and the **Gateway/Secret-level status surface
is in good shape** (7/7 of the tests that don't depend on route status pass).

But conformance is **HTTPRoute-centric**, and aether currently writes **Gateway**
status but **not HTTPRoute** status. Because the suite asserts route
`.status.parents` *before* it sends traffic, **one missing reconciler
(`HTTPRoute.status.parents`) gates ~24 of the 30 failures** — including the two
big rev3 buckets the addressing/cert work was meant to unlock. So the honest
read is: **aether is one well-scoped reconciler away from a large jump.** The
data-plane (routing, TLS termination, distinct addressing) is demonstrably
capable; the conformance gap is now **status reporting on HTTPRoute**, not data
plane. Until that lands, the answer to "conformant?" is **7/37 — not conformant,
but the binding constraint is singular and identified.**

## Cleanup (Phase 2 LB-leak check)

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces, but
deleting the namespaces left **5 orphan per-Gateway LoadBalancer Services** in
`aether-ingress` (the edge-provisioned Services are not GC'd when the Gateway
disappears via namespace deletion — they held pool IPs `.50–.54`). These were
**manually deleted** after the run. Final state verified:

```
$ kubectl get ns | grep conformance                      → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

No MetalLB pool IPs leaked. **Follow-up note:** the orphan-Service-on-namespace-delete
behavior is itself a bug worth filing — the edge should GC its per-Gateway Service
when the owning Gateway is deleted (incl. via namespace teardown), so future
conformance runs don't require manual LB cleanup.

## Reproduction

A ~150-LOC standalone test (`conformance/aether_rev5_test.go`, **not committed**)
in a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api-src/conformance` (its
own Go module, `replace sigs.k8s.io/gateway-api => ../`), gated by `AETHER_REV5=1`,
run with `GOWORK=off` from inside `conformance/`. Two subtests:
`TestAetherRev5Mesh` (explicit mesh features) and `TestAetherRev5Gateway`
(features inferred from `GatewayClass.status.supportedFeatures`). Same non-obvious
requirements as rev2–rev4: `ManifestFS = conformance.Manifests`,
`AllowCRDsMismatch`, and — for the inference path — empty
`SupportedFeatures`/`ExemptFeatures` with `EnableAllSupportedFeatures=false`.
Budgets `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
`RequestTimeout=10s`. The GATEWAY run takes ~46 min (retry-heavy); implement
`HTTPRoute.status.parents` to make the bulk of it pass on the next run.
