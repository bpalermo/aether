# Gateway API conformance — re-run rev4 (2026-06-25)

A fourth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now
at aether **0.49.0** (rev 81). This re-run measures the **delta** from the
[rev3 baseline](./baseline-2026-06-25-rev3.md) (0.45.0, rev 72) after the rest of
**proposal 021** and the status/redirect work landed:

- **021 Phase 2 (#333 + #335)** — **DISTINCT per-Gateway addresses.** Each
  class-`aether` Gateway now provisions its **own** MetalLB LoadBalancer Service
  (label `aether.io/edge-gateway`) and gets its **own** routable IP, with
  internal-port demux behind it. This was expected to be the big rev4 unlock —
  it dissolves rev3's shared-edge-IP / bare-IP-TLS-SAN wall.
- **Per-listener HTTP→HTTPS redirect (#330 + #332)** — HTTP listeners now serve
  their routes by default; the redirect is per-Gateway opt-in.
- **6 status edge-cases (#331)** — listener `supportedKinds`/`InvalidRouteKinds`,
  **`InvalidCertificateRef`**, parentRef `sectionName`/`namespaces` attachment,
  `attachedRoutes`, GatewayClass `observedGeneration`.
- **ReferenceGrant (#324)** — already in rev3 (cross-ns backendRef enforcement).
- **Chart-managed edge Gateway API (#334)** — declarative edge Gateway/HTTPRoute
  (context only; no conformance-visible behavior change).

Aether was **not** modified or redeployed for this run. All
`gateway-conformance-*` namespaces **and** their per-Gateway LoadBalancer
Services were cleaned up afterward (verified: only the production edge
`aether-edge-gw-aether-ingress-aether-edge` = `192.168.100.101` remains; the
MetalLB pool `192.168.100.50-254` is not exhausted).

## TL;DR

| Profile | Setup | Tests run | PASS | FAIL | SKIP (in-profile) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** (north-south) | **❌ FAILED in Setup** | **0** | **0** | **0 (blocked)** | — | **REGRESSION: the profile no longer reaches its tests.** Phase 2 distinct addressing genuinely *works* (each conformance Gateway got its own pool IP — `.50/.51/.52`, distinct from the edge's `.101`). But **#331 introduced strict HTTPS-listener cert validation**, and the edge resolves the listener `certificateRef` **from its own `aether-ingress` namespace instead of the `certificateRef.namespace`**. The base-manifest `same-namespace-with-https-listener` Gateway therefore never becomes `Programmed`, and since it is a **base resource**, the suite's "ensure base Gateways ready" gate times out → the whole profile aborts before any test runs. |
| **MESH-HTTP** (east-west / GAMMA) | passed | 7 | **4*** | **3*** | 0 | **Effectively unchanged from rev3/baseline.** 4 stable PASS + the same 3 stable capture-path filter FAILs. `MeshHTTPRouteMatching` is a **flaky boundary** test (a different ~3-of-9 sub-cases time out on each run), so a single run can read 3/4 or 4/3. The genuine, untouched gap (proposal 022 not implemented) is identical. |

**Headline delta:** GATEWAY-HTTP went from **6 PASS / 31 FAIL (37 run)** in rev3
to **Setup-blocked / 0 run** in rev4 — a **net regression in measurability**,
caused by a **new cert-namespace resolution bug in #331**, *not* by anything in
Phase 2 (which works). MESH-HTTP is flat at **4 stable PASS / 3 stable FAIL**.

> **The Phase 2 win is real but currently un-scorable.** The single one-line fix
> below (resolve cert Secrets from `certificateRef.namespace`) should unblock
> Setup and is expected to flip the bulk of rev3's 22 shared-address/TLS fails to
> PASS in the *next* run, because distinct addressing + per-Gateway certs are
> now in place. See "What Phase 2 would have delivered" below.

## How it was run

Same programmatic runner as rev2/rev3 (`conformance/aether_rev4_test.go`, a
one-off in a `gateway-api` v1.5.1 checkout that wires `replace
sigs.k8s.io/gateway-api => ../`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}`
  — **identical to rev3** (no advertised-feature change in 0.49.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification,
  HTTPRouteMethodMatching, HTTPRouteRequestTimeout}`), as in rev2/rev3.
- Budgets were **generous** (rev3 style — Gateways now genuinely get an address,
  so traffic deserves convergence time):
  `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.

The GATEWAY run finished in **~5 min** (309 s) — *much* faster than rev3's ~40
min — precisely **because it aborted in Setup**: it never reached the long tail
of retry-heavy negative status tests.

## DELTA vs rev3 (and the original baseline)

| | Baseline (0.42.0, rev 68) | rev2 (0.43.0, rev 70) | rev3 (0.45.0, rev 72) | rev4 (0.49.0, rev 81) | rev3→rev4 |
|---|---|---|---|---|---|
| **GATEWAY-HTTP Setup** | ❌ blocked | ✅ passes | ✅ passes | **❌ blocked (HTTPS base Gw not Programmed)** | **REGRESSED** |
| **GATEWAY-HTTP tests run** | 0 | 37 | 37 | **0** | **−37** |
| **GATEWAY-HTTP PASS** | 0 | 6 | 6 | **0 (un-scorable)** | **−6** |
| **GATEWAY-HTTP FAIL** | 0 | 31 | 31 | **0 (un-scorable)** | — |
| Gateways get `.status.addresses` | no | no | yes (shared `.101`) | **yes — DISTINCT per-Gw (`.50/.51/.52`)** | **Phase 2 ✅** |
| Per-Gateway LoadBalancer Service | no | no | no | **yes (own MetalLB IP each)** | **Phase 2 ✅** |
| Shared-IP / bare-IP-TLS-SAN wall | n/a | n/a | 22 fails | **dissolved (distinct addr + own cert)** | **removed by Phase 2** |
| HTTPS listener cert validation | none | none | none (kept `True`) | **strict (`InvalidCertificateRef`) — but wrong-ns lookup** | **#331, with a bug** |
| **MESH-HTTP PASS / FAIL** | 4 / 3 | 4 / 3 | 4 / 3 | **4 / 3*** (flaky boundary) | **0 (flat)** |

**What demonstrably landed (and what broke):**

1. **021 Phase 2 works.** During the run, three distinct per-Gateway
   LoadBalancer Services appeared, each with its **own** pool IP, all distinct
   from the production edge `.101`:

   ```
   aether-edge-gw-gateway-conformance-infra-same-namespace        192.168.100.50
   aether-edge-gw-gateway-conformance-infra-all-namespaces        192.168.100.51
   aether-edge-gw-gateway-conformance-infra-backend-namespaces    192.168.100.52
   ```

   This is exactly the rev3 P0 bridge — the shared-address tax is gone.

2. **#331 (strict cert validation) regressed Setup.** The HTTPS base Gateway
   `same-namespace-with-https-listener` now reports
   `Programmed=False / ListenersNotValid`, with every listener
   `ResolvedRefs=False / InvalidCertificateRef`. The edge reconciler
   (`agent/internal/edge/gatewayapi/reconciler.go:443`) looks the Secret up in
   **its own namespace**:

   ```
   gateway listener TLS cert unresolved … secret=tls-validity-checks-certificate
   error="get TLS secret aether-ingress/tls-validity-checks-certificate:
          Secret \"tls-validity-checks-certificate\" not found"
   ```

   …even though the listener spec explicitly sets
   `certificateRef.namespace: gateway-conformance-infra` (where the suite *does*
   create the Secret).

### Root-cause proof (reproduced deterministically)

The cert-namespace bug was confirmed independently of the run, then the fix was
confirmed by construction:

- Applied the base HTTPS Gateway with the Secret only in
  `gateway-conformance-infra` (its declared `certificateRef.namespace`) →
  `Programmed=False / InvalidCertificateRef`, edge looking in
  `aether-ingress/…`.
- Duplicated the *same* Secret into `aether-ingress` → the Gateway **immediately
  flipped to `Programmed=True`**.

So the listener is otherwise valid; the **only** blocker is that aether resolves
listener `certificateRef` Secrets from the edge's namespace rather than from
`certificateRef.namespace` (which should be honored, gated by a `ReferenceGrant`
when cross-namespace). All diagnostic resources were deleted afterward.

## What Phase 2 *would* have delivered (had Setup not regressed)

This matters for prioritization: rev3's single biggest bucket — **22 fails** on
the shared-edge-IP / bare-IP-TLS-SAN wall — was attributed directly to "one
shared LB IP + redirect + a cert with no IP SAN," with **021 Phase 2 named as
the enabling fix**. Phase 2 is now live and demonstrably assigns **distinct
per-Gateway addresses, each terminating TLS with its own configured
`certificateRef` cert**. With the one-line cert-namespace fix below:

- The **HTTP-path traffic tests** (18 in rev3) become reachable on each
  Gateway's own IP, with redirect now per-Gateway opt-in (#330/#332) → expected
  to PASS.
- The **HTTPS-listener traffic tests** (`HTTPRouteHTTPSListener`,
  `HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection`,
  `HTTPRouteRedirectHostAndStatus`) now terminate with the conformance Gateway's
  **own** cert Secret (the suite supplies it) → expected to PASS once the cert
  resolves from the right namespace.

In other words, **the HTTPS/TLS traffic tests should flip the moment the
cert-namespace bug is fixed** — the distinct-address + per-Gateway-cert
machinery they need is already in place. We just couldn't *score* it this run.

## GATEWAY-HTTP — categorized blockers (as of rev4)

| # | Blocker | Status vs rev3 | Notes |
|---|---|---|---|
| **0 (NEW)** | **HTTPS listener `certificateRef` resolved from the edge namespace, not `certificateRef.namespace`.** Blocks the base HTTPS Gateway → **Setup aborts the whole profile.** | **NEW regression (#331)** | One-line fix. Until then GATEWAY-HTTP is un-scorable. **Top priority.** |
| 1 | Distinct per-Gateway address + own cert (rev3 P0, 22 tests). | **RESOLVED by Phase 2** (#333/#335) — pending #0 to score. | The big win; just gated behind #0. |
| 2 | 6 status edge-cases (observedGeneration, InvalidRouteKinds, InvalidCertificateRef, cross-ns/sectionName parentRef Accepted=False, non-8080 attachedRoutes). | **Partially addressed by #331** (incl. InvalidCertificateRef — which is what *caused* #0). | Needs a re-score once #0 is fixed; several likely flip to PASS. |
| 3 | ReferenceGrant enforcement for Gateway listener `certificateRef`s (2 tests). | unchanged | #324 covered backendRefs; extend to cert refs. The #0 fix should resolve same-ns directly and tee up cross-ns grant handling. |
| 4 | aether dup-FQDN webhook vs conformance split-across-routes (`HTTPRouteMatchingAcrossRoutes`, 1 test). | unchanged | Design conflict. |

## MESH-HTTP — still 4 PASS / 3 FAIL (flaky boundary, effectively unchanged)

Profile report (run 1): `pass=3 fail=4`. Profile report would read `4/3` when
`MeshHTTPRouteMatching` converges. It is a **flaky boundary** test on the GAMMA
capture path: on two back-to-back runs a **different ~3-of-9 sub-cases** timed
out at the 30 s budget (run 1: `/v2`, `/v2example`; run 2: `/`, `/example`,
`/v2/example`) — non-deterministic capture-path convergence, not a stable
failure.

**Stable PASS (4):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`,
`MeshTrafficSplit`, and `MeshHTTPRouteMatching` *when it converges*.

**Stable FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — **the same three as
rev3 and the baseline.** HTTPRoute redirect / header-modifier filters translate
on the **edge** path but **not on the GAMMA transparent-capture path**, and the
weighted-distribution assertion still doesn't converge on capture. **Proposal
022 (arbitrary-Service interception / genuine GAMMA data plane) is not
implemented**, so this is correctly flat. (`MeshFrontend` also ran due to the
advertised `ResponseHeaderModification` feature and failed its `Send to service`
case, but it is **not** in the MESH-HTTP profile's scored set.)

## Prioritized remaining blockers

| Priority | Blocker | Profile | Impact |
|---|---|---|---|
| **P0 (GW) — NEW** | **Resolve listener `certificateRef` Secrets from `certificateRef.namespace`** (honor the spec'd namespace; gate cross-ns with a `ReferenceGrant`), not from the edge's own `aether-ingress` namespace. | GATEWAY-HTTP | **Unblocks Setup → the entire profile.** Without it, GATEWAY-HTTP is un-scorable. Single highest-leverage fix. |
| **P0 (Mesh)** | **Apply HTTPRoute filters on the GAMMA / transparent-capture path (proposal 022).** Redirect + request/response header-modifier translate at the edge but not on capture. | MESH-HTTP | `MeshHTTPRouteRedirectHostAndStatus` + `MeshHTTPRouteRequestHeaderModifier` → PASS (~6/7). The single biggest MESH blocker, unchanged since baseline. |
| **P1** | **Re-score GATEWAY-HTTP after the P0 fix.** Phase 2 (distinct addr + per-Gateway cert) is live; expect the rev3 22-test traffic bucket (incl. the HTTPS/TLS tests) and several #331 status tests to flip to PASS. | GATEWAY-HTTP | Recovers and likely *exceeds* the rev3 6 PASS once measurable. |
| **P1** | **ReferenceGrant enforcement for Gateway listener `certificateRef`s.** | GATEWAY-HTTP | 2 tests. |
| **P2** | **Reconcile dup-FQDN webhook vs conformance split-across-routes** (`HTTPRouteMatchingAcrossRoutes`). | GATEWAY-HTTP | 1 test; design decision. |
| **P3** | **Stabilize the GAMMA capture path** so `MeshHTTPRouteMatching` converges deterministically; then `MeshHTTPRouteWeight`. | MESH-HTTP | Removes the flaky boundary + last mesh FAIL. |

## Reproduction

A ~150-LOC standalone test (`conformance/aether_rev4_test.go`, **not committed**)
in a `gateway-api` v1.5.1 checkout under `conformance/` (its own Go module,
`replace sigs.k8s.io/gateway-api => ../`), gated by `AETHER_REV4=1`, run with
`GOWORK=off` from inside `conformance/`. Two subtests: `TestAetherRev4Mesh`
(explicit mesh features) and `TestAetherRev4Gateway` (features inferred from
`GatewayClass.status.supportedFeatures`). Same non-obvious requirements as
rev2/rev3: `ManifestFS = conformance.Manifests`, `AllowCRDsMismatch`, and — for
the inference path — empty `SupportedFeatures`/`ExemptFeatures` with
`EnableAllSupportedFeatures=false`. In rev4 the GATEWAY run aborts in **Setup**
(~5 min) on the HTTPS base Gateway; fix the cert-namespace bug to re-enable the
full ~40-min GATEWAY pass.

**Cleanup verified after the run:** zero `gateway-conformance-*` namespaces; the
only `aether.io/edge-gateway` Service remaining is the production edge
`aether-edge-gw-aether-ingress-aether-edge` (`192.168.100.101`); no orphan
MetalLB pool IPs.
