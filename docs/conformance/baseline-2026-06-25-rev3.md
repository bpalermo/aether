# Gateway API conformance — re-run rev3 (2026-06-25)

A third run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now
at aether **0.45.0** (rev 72). This re-run measures the **delta** from the
[rev2 baseline](./baseline-2026-06-25-rev2.md) (0.43.0, rev 70) after two
fixes landed:

- **(a) Gateway `status.addresses`** — every class-`aether` Gateway now publishes
  `.status.addresses = [192.168.100.101]` (the shared edge LoadBalancer IP),
  021 **Phase 1** (#327).
- **(b) `ReferenceGrant` cross-namespace enforcement** — cross-namespace
  HTTPRoute `backendRef`s are now gated by a `ReferenceGrant` (#324).

Aether was **not** modified or redeployed for this run (edge image digest
unchanged: `agent@sha256:7ffafb8f…`). All `gateway-conformance-*` namespaces were
cleaned up afterward (verified removed).

## TL;DR

| Profile | Setup | Tests run | PASS | FAIL | SKIP (in-profile) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** (north-south) | passed | 37 | **6** | **31** | 0 | **Totals flat vs rev2 (6/31), but the failure mode moved an entire layer deeper.** Gateways now have an address (#327), so the ~20 tests that died at `WaitForGatewayAddress` in rev2 now sail past setup and **attempt real traffic** — where they hit a *new* wall: the edge redirects `http://192.168.100.101/` → `https://…`, and the conformance client can't verify the bare-IP TLS cert (no IP SAN). The score didn't move; the blocker did. |
| **MESH-HTTP** (east-west / GAMMA) | passed | 7 | **4** | **3** | 0 | **Unchanged from rev2 and the original baseline.** The interception-model gap is untouched: filters still aren't applied on the GAMMA capture path. |

**Headline delta:** GATEWAY-HTTP is **6 PASS / 31 FAIL — identical totals to
rev2** — but the dominant failure category flipped from *"Gateway has no address"*
(rev2, ~20 tests) to *"HTTP→HTTPS redirect + bare-IP TLS has no IP SAN"* (rev3,
**18 HTTP + 4 HTTPS-listener tests = 22 traffic tests**). MESH-HTTP is flat at
**4/3**.

## How it was run

Same programmatic runner as rev2 (`conformance/aether_rev3_test.go`, a one-off
diagnostic in a `gateway-api` v1.5.1 checkout that wires `replace
sigs.k8s.io/gateway-api => ../`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true` (mixed
  standard + experimental CRDs on talos-main).
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}`
  (note **`ReferenceGrant` is now advertised by the class** — #324 — in addition
  to being the profile's mandatory feature). Everything else was skipped cleanly.
- **MESH-HTTP:** mesh support advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification,
  HTTPRouteMethodMatching, HTTPRouteRequestTimeout}`), as in rev2.
- Unlike rev2, the `GatewayMustHaveAddress` budget was **not** shortened to
  fail-fast (Gateways now genuinely get an address, so there is nothing to
  fail-fast on). `MaxTimeToConsistency` was left generous (60s) so the
  now-reachable traffic tests get a fair chance to converge.

## DELTA vs rev2 (and the original baseline)

| | Baseline (0.42.0, rev 68) | rev2 (0.43.0, rev 70) | rev3 (0.45.0, rev 72) | rev2→rev3 |
|---|---|---|---|---|
| **GATEWAY-HTTP Setup** | ❌ blocked in `Setup` | ✅ passes | ✅ passes | — |
| **GATEWAY-HTTP tests run** | 0 | 37 | 37 | 0 |
| **GATEWAY-HTTP PASS** | 0 | 6 | **6** | **0** |
| **GATEWAY-HTTP FAIL** | 0 | 31 (Core 28, Ext 3) | **31 (Core 28, Ext 3)** | **0** |
| Gateways get `.status.addresses` | no | no | **yes (`192.168.100.101`)** | **fixed (#327)** |
| `WaitForGatewayAddress` failures | n/a | ~20 | **0** | **−20 (gone)** |
| Bare-IP TLS-SAN traffic failures | n/a | 0 | **22** | **+22 (new)** |
| HTTPRoute cross-ns backendRef **status** correct | no | no | **yes (conditions pass)** | **fixed (#324)** |
| **MESH-HTTP PASS / FAIL** | 4 / 3 | 4 / 3 | **4 / 3** | **0** |

**The score is unchanged but two fixes provably landed:**

1. **#327 works.** The ~20 rev2 failures that read
   *"error waiting for Gateway to have at least one IP address in status"* are
   **completely gone** — zero such errors in the rev3 run. Every conformance
   Gateway now reports `.status.addresses = [192.168.100.101]` and the suite
   proceeds into traffic.
2. **#324 works for HTTPRoute backendRefs.** The cross-namespace-backendRef
   ReferenceGrant tests (`HTTPRouteReferenceGrant`,
   `HTTPRouteInvalidReferenceGrant`, `HTTPRouteInvalidCrossNamespaceBackendRef`,
   `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`) now **pass their status /
   conditions phase** ("Conditions matched expectations", "Parents matched
   expectations") — i.e. aether correctly sets `ResolvedRefs=False /
   RefNotPermitted` without a grant and resolves with one. They still appear in
   the FAIL column **only because the subsequent traffic probe hits the bare-IP
   TLS-SAN wall** (below). Absent that wall, these would flip to PASS.

The net headline stayed 6/31 because the address fix *unblocked* ~20 tests
straight into a *different* blocker (TLS), and that blocker is now the single
largest bucket.

## GATEWAY-HTTP — the 31 FAIL, fully categorized

Profile report: `Core pass=5 fail=28`, `Extended pass=1 fail=3` → **6 PASS / 31
FAIL / 0 skip-in-profile**.

### The 6 PASS — identical set to rev2 (all status / reconcile, zero traffic)

`GatewayModifyListeners`, `GatewayObservedGenerationBump`,
`GatewayWithAttachedRoutesWithPort8080`, `HTTPRouteObservedGenerationBump`,
`GatewaySecretReferenceGrantAllInNamespace`,
`GatewaySecretReferenceGrantSpecific`.

### The 31 FAIL — four buckets

**1. Bare-IP TLS-SAN on the traffic path — the new dominant bucket (22 tests).**
This is the rev3 story. Gateways now have an address, so the suite issues a real
`GET http://192.168.100.101/`. The aether edge answers with an **HTTP→HTTPS
redirect**, the client follows to `https://192.168.100.101/`, and the TLS
handshake fails:

```
Request failed, not ready yet: Get "https://192.168.100.101/":
  tls: failed to verify certificate: x509: cannot validate certificate for
  192.168.100.101 because it doesn't contain any IP SANs
```

- **18 HTTP-path tests:** `HTTPRouteSimpleSameNamespace`, `HTTPRouteMatching`,
  `HTTPRouteExactPathMatching`, `HTTPRouteHeaderMatching`,
  `HTTPRoutePathMatchOrder`, `HTTPRouteServiceTypes`, `HTTPRouteWeight`,
  `HTTPRouteRequestHeaderModifier`, `HTTPRouteResponseHeaderModifier` (Ext),
  `HTTPRouteMethodMatching` (Ext), `HTTPRouteTimeoutRequest` (Ext),
  `HTTPRouteCrossNamespace`, `HTTPRouteReferenceGrant`,
  `HTTPRouteInvalidReferenceGrant`,
  `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`,
  `HTTPRouteInvalidCrossNamespaceBackendRef`,
  `HTTPRouteInvalidNonExistentBackendRef`,
  `HTTPRouteInvalidBackendRefUnknownKind`.
- **4 HTTPS-listener / hostname tests** that target `https://192.168.100.101/`
  directly (same SAN failure): `HTTPRouteHTTPSListener`,
  `HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection`,
  `HTTPRouteRedirectHostAndStatus`.

This bucket is a consequence of the **single shared edge LB IP + always-on
HTTP→HTTPS redirect + a serving cert with no IP SAN**. It is the
shared-address model's tax, and the bridge to fixing it is **021 Phase 2** (see
blockers). Note: it is *not* literally a "distinct address" assertion — the
suite never complains the addresses are shared; it complains it cannot speak TLS
to a bare IP. **Distinct per-Gateway addresses (021 Phase 2) is the enabling
change** (each Gateway gets its own routable address with a matching cert /
hostname), but the proximate fix is *cert-SAN + redirect handling on the edge
address*.

**2. Status edge-cases — not traffic-related (6 tests).**

| Test | Gap |
|---|---|
| `GatewayClassObservedGenerationBump` | GatewayClass `status.observedGeneration` not bumped on spec change. |
| `GatewayInvalidRouteKind` | listener `ResolvedRefs` should go `False`/`InvalidRouteKinds` and surface `supportedKinds`; aether keeps it `True`. |
| `GatewayInvalidTLSConfiguration` | invalid/malformed/nonexistent cert refs should drive listener `ResolvedRefs=False` (`InvalidCertificateRef`); aether keeps `True`. |
| `HTTPRouteInvalidCrossNamespaceParentRef` | route should be `Accepted=False`/`NotAllowedByListeners`; aether sets `Accepted=True`. |
| `HTTPRouteInvalidParentRefNotMatchingSectionName` | route attaching to a non-matching `sectionName` should be `Accepted=False`; aether sets `Accepted=True`. |
| `GatewayWithAttachedRoutes` | the non-port-8080 attached-routes variant doesn't produce the expected `attachedRoutes`/`ResolvedRefs` shape (the port-8080 variant passes). |

**3. ReferenceGrant enforcement for Gateway TLS *secrets* — still missing (2
tests).** `GatewaySecretInvalidReferenceGrant`,
`GatewaySecretMissingReferenceGrant`: aether keeps the listener
`ResolvedRefs=True` when a cross-namespace **certificateRef** lacks a valid
grant; it should be `False`/`RefNotPermitted`. **#324 added cross-ns enforcement
for HTTPRoute `backendRef`s but not for Gateway listener `certificateRef`s** —
this is the residual ReferenceGrant gap. (The two *positive* secret-grant tests
still pass.)

**4. aether dup-FQDN webhook conflict (1 test).** `HTTPRouteMatchingAcrossRoutes`
fails before any traffic because aether's admission webhook rejects the second
route:

```
admission webhook "validate.config.aether.io" denied the request:
  host "example.com" is already claimed by HTTPRoute
  gateway-conformance-infra/matching-part1 on the same Gateway;
  each external FQDN may be served by only one HTTPRoute per Gateway
```

The conformance model (split a host across multiple HTTPRoutes on one Gateway)
collides head-on with aether's one-FQDN-per-HTTPRoute constraint. This is a
**design conflict**, not a missing feature.

### Distinct-address subset vs genuinely-other

Per the brief's question — of the 31 GATEWAY fails:

- **22 are the shared-edge-address / bare-IP-TLS subset** (→ **021 Phase 2**:
  distinct per-Gateway addresses, each with a hostname/cert the conformance
  client can verify). These are not "address present but shared is rejected"
  — the address *is* present and accepted; they fail one step later on TLS to
  the bare shared IP.
- **9 are genuinely other:** 6 status edge-cases, 2 TLS-secret ReferenceGrant
  enforcement, 1 aether dup-FQDN webhook conflict.

## MESH-HTTP — still 4 PASS / 3 FAIL (unchanged)

Profile report: `pass=4 fail=3 skip=0`.

**PASS (4):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`,
`MeshHTTPRouteMatching`, `MeshTrafficSplit`.

**FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — **the same three as
rev2 and the baseline**. The HTTPRoute redirect / header-modifier filters are
applied on the **edge** route path but **not on the GAMMA transparent-capture
path** the MESH profile exercises, and the weighted-distribution assertion still
doesn't converge on capture. Nothing in rev3 (#327 / #324) touches the mesh
data path, so this is correctly **flat**.

## Prioritized remaining blockers

| Priority | Blocker | Profile | Impact |
|---|---|---|---|
| **P0 (GW)** | **Per-Gateway addressing with a verifiable cert (021 Phase 2).** One shared edge LB IP + HTTP→HTTPS redirect + a serving cert with no IP SAN means the conformance client can't complete TLS to `https://192.168.100.101/`. Give each Gateway its own routable address with a matching hostname/cert (or serve a cert with the LB IP in SANs / let the suite use plain HTTP). | GATEWAY-HTTP | **The single biggest lever: 22 of 31 fails.** Many would flip immediately (the status/conditions phases already pass, including the #324 backendRef ReferenceGrant cases). |
| **P0 (Mesh)** | **Apply HTTPRoute filters on the GAMMA / transparent-capture path.** Redirect + request/response header-modifier translate at the edge but not on capture. | MESH-HTTP | `MeshHTTPRouteRedirectHostAndStatus` + `MeshHTTPRouteRequestHeaderModifier` → PASS (~6/7). **The single biggest MESH blocker, unchanged since baseline.** |
| **P1** | **ReferenceGrant enforcement for Gateway listener `certificateRef`s.** #324 covers HTTPRoute backendRefs; extend the same gate to cross-ns TLS-secret refs. | GATEWAY-HTTP | `GatewaySecretInvalidReferenceGrant`, `GatewaySecretMissingReferenceGrant`. |
| **P1** | **Status edge-cases.** GatewayClass `observedGeneration` bump; listener `InvalidRouteKinds`/`InvalidCertificateRef` `ResolvedRefs=False`; route `Accepted=False` for cross-ns/sectionName parentRef mismatch; non-port-8080 `attachedRoutes`. | GATEWAY-HTTP | 6 status-only tests, no traffic needed. |
| **P2** | **Reconcile the dup-FQDN webhook vs conformance's split-across-routes model** (`HTTPRouteMatchingAcrossRoutes`). Decide whether to relax one-FQDN-per-HTTPRoute on a shared Gateway. | GATEWAY-HTTP | 1 test; design decision. |
| **P3** | **Mesh weighted distribution / interception model** (`MeshHTTPRouteWeight`). | MESH-HTTP | Last mesh FAIL after filters land. |

## Reproduction

A ~150-LOC standalone test (`conformance/aether_rev3_test.go`, **not committed**)
in a `gateway-api` v1.5.1 checkout under `conformance/`, gated by `AETHER_REV3=1`,
run with `GOWORK=off`. Two subtests: `TestAetherRev3Mesh` (explicit mesh
features) and `TestAetherRev3Gateway` (features inferred from
`GatewayClass.status.supportedFeatures`). Same non-obvious requirements as rev2:
`ManifestFS = conformance.Manifests`, `AllowCRDsMismatch` on the mixed-channel
cluster, and — for the inference path — empty
`SupportedFeatures`/`ExemptFeatures` with `EnableAllSupportedFeatures=false`.
The GATEWAY-HTTP run takes ~40 min (the many negative status tests each retry to
their consistency timeout).
