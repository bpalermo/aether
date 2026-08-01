# Gateway API conformance — re-run rev2 (2026-06-25)

A second run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now
at aether **0.43.0** (rev 70). This re-run measures the **delta** from the
[2026-06-25 baseline](./baseline-2026-06-25.md) after three changes landed:

- **(a) Namespace-agnostic edge Gateway reconcile + status** — the edge
  controller now caches/lists Gateways and Routes cluster-wide and publishes
  `Accepted`/`Programmed` (and per-listener `attachedRoutes`, `ResolvedRefs`)
  for class-`aether` Gateways in **any** namespace (#323).
- **(b) `GatewayClass.status.supportedFeatures`** is now advertised, so the
  suite self-configures (skips unimplemented features instead of failing).
- **(c) HTTPRoute `RequestHeaderModifier` / `ResponseHeaderModifier` +
  `RequestRedirect` + `URLRewrite` filters** (#319, #322).

Aether was **not** modified or redeployed for this run. All
`gateway-conformance-*` namespaces were cleaned up afterward (verified removed).

## TL;DR

| Profile | Setup | Tests run | PASS | FAIL | SKIP (by feature) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** (north-south) | **PASSED** ✅ | 37 | **6** | **31** | 31 | The big unlock: `NamespacesMustBeReady` now passes — every class-`aether` Gateway in `gateway-conformance-infra` reaches `Accepted=True` + `Programmed=True`, so the suite **enumerates and runs the whole profile** for the first time. The 6 passes are all **status-only** tests. Every test that needs **live traffic through a Gateway fails on the missing per-Gateway address**. |
| **MESH-HTTP** (east-west / GAMMA) | passed | 7 | **4** | **3** | 0 | **Unchanged from baseline.** The redirect/header-modifier filters that now ship at the route level are **not applied on the GAMMA transparent-capture path**, so the same two filter tests still fail. |

**Headline delta:** GATEWAY-HTTP went from **0 tests run (aborted in Setup)** →
**37 tests run, 6 PASS / 31 FAIL**. MESH-HTTP is **flat at 4/3** — the shipped
filters did not move the mesh number.

## How it was run

Same programmatic runner approach as the baseline: a standalone Go test
(`conformance/aether_rev2_test.go`, **not committed** — a one-off diagnostic in a
checkout of `gateway-api` v1.5.1, which wires the `replace
sigs.k8s.io/gateway-api => ../` for the nested conformance module) drives
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests` (required when calling the suite
  programmatically — only the CLI wires the embedded base/mesh manifest FS).
- `AllowCRDsMismatch: true` — talos-main carries standard **and** experimental
  Gateway API CRDs (aether's L4 routes); benign for these two profiles.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures` (the rev2 unlock —
  `SupportedFeatures`/`ExemptFeatures` left empty,
  `EnableAllSupportedFeatures=false`). The suite read the advertised set
  `{Gateway, GatewayPort8080, HTTPRoute, GRPCRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification}` and the
  GATEWAY-HTTP profile injected its mandatory `ReferenceGrant` on top — so the
  effective set the suite tested was those 7 plus `ReferenceGrant`. Everything
  else was **skipped cleanly** (not failed) — confirming (b) works as intended.
- **MESH-HTTP:** mesh support cannot be inferred from a GatewayClass, so
  `{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification,
  HTTPRouteMethodMatching, HTTPRouteRequestTimeout}` were advertised explicitly
  (as the baseline did).
- For GATEWAY-HTTP the per-Gateway address / traffic budgets
  (`GatewayMustHaveAddress`, `MaxTimeToConsistency`, `RequestTimeout`) were
  shortened so the many address-dependent tests **fail fast** rather than each
  burning the full 5-minute default (this only changes how quickly a doomed test
  gives up; it does not change pass/fail).

## DELTA vs baseline

| | Baseline (0.42.0, rev 68) | rev2 (0.43.0, rev 70) | Delta |
|---|---|---|---|
| **GATEWAY-HTTP Setup** | ❌ blocked in `Setup` (Gateways never got status → `NamespacesMustBeReady` timed out) | ✅ **passes** (all 4 infra Gateways `Accepted`+`Programmed`) | **Unblocked** |
| **GATEWAY-HTTP tests run** | 0 | 37 | **+37** |
| **GATEWAY-HTTP PASS** | 0 | 6 | **+6** |
| **GATEWAY-HTTP FAIL** | 0 | 31 (Core 28, Extended 3) | +31 (now visible) |
| **GatewayClass `supportedFeatures`** | absent (features passed manually) | advertised; suite **inferred** + skipped cleanly | **Works** |
| **MESH-HTTP PASS / FAIL** | 4 / 3 | 4 / 3 | **0** (no change) |
| MESH `RequestHeaderModifier` | FAIL | FAIL | unchanged |
| MESH `RedirectHostAndStatus` | FAIL | FAIL | unchanged |
| MESH `Weight` | FAIL | FAIL | unchanged |

The expectation going in was that the two filter-gap mesh tests would now PASS
(filters shipped). **They did not** — see below.

## MESH-HTTP — still 4 PASS / 3 FAIL (filters don't reach the capture path)

Profile report: `pass=4 fail=3 skip=0`.

**PASS (4):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`,
`MeshHTTPRouteMatching`, `MeshTrafficSplit`.

**FAIL (3):** `MeshHTTPRouteRequestHeaderModifier`,
`MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteWeight`.

The important finding: the `RequestHeaderModifier` and redirect filters **did
ship** (#319/#322) and are advertised — but they are applied on the **edge /
route-level** HTTPRoute path, **not on the GAMMA transparent-capture path** that
the MESH profile exercises. The conformance signal is unambiguous:

```
MeshHTTPRouteRedirectHostAndStatus:
  wanted status code to be one of [302], got 200
```

i.e. the request still reaches the backend (200) instead of being turned into a
3xx redirect — identical to the baseline behavior. The header-modifier subtests
likewise time out waiting for the modified header that never arrives. `MeshWeight`
remains a stricter weighted-distribution assertion that doesn't converge on the
capture path (`MeshTrafficSplit`, the looser split test, still passes).

**Conclusion:** filter translation needs to be wired into the **mesh/capture**
route generation, not only the edge. This is the single biggest remaining
blocker for MESH-HTTP.

## GATEWAY-HTTP — now runs: 6 PASS / 31 FAIL

Profile report: Core `pass=5 fail=28`, Extended `pass=1 fail=3` → **6 PASS / 31
FAIL / 0 skip-in-profile** (31 further tests skipped by unadvertised feature —
by design).

### The 6 PASS — all status / reconcile, zero traffic

| Test | What it proves |
|---|---|
| `GatewayModifyListeners` | Listener add/remove re-reconciles + status updates. |
| `GatewayObservedGenerationBump` | Gateway status `observedGeneration` tracks spec. |
| `GatewayWithAttachedRoutesWithPort8080` | `attachedRoutes` count + `ResolvedRefs` for a port-8080 listener. |
| `HTTPRouteObservedGenerationBump` | HTTPRoute status `observedGeneration` tracks spec. |
| `GatewaySecretReferenceGrantAllInNamespace` | Gateway TLS secret accepted via an all-in-namespace `ReferenceGrant`. |
| `GatewaySecretReferenceGrantSpecific` | …and via a specifically-named grant. |

Every one of these asserts **status conditions only** — exactly the surface
(a)+(b) unlocked. No request is ever sent through the Gateway.

### The 31 FAIL — three buckets

**1. Missing per-Gateway address → no live traffic (the dominant bucket).**
The edge is still **one Deployment on one LoadBalancer** (`192.168.100.101`) with
**no per-Gateway address allocation**: every conformance Gateway is
`Programmed=True` but its `.status.addresses` is **empty**. The suite's traffic
tests call `WaitForGatewayAddress` first and fail with:

```
error waiting for Gateway to have at least one IP address in status
```

This fails ~17 Core tests that need to route a request through a specific
Gateway: `HTTPRouteSimpleSameNamespace`, `HTTPRouteMatching`,
`HTTPRouteMatchingAcrossRoutes`, `HTTPRouteHeaderMatching`,
`HTTPRouteExactPathMatching`, `HTTPRoutePathMatchOrder`,
`HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection`,
`HTTPRouteHTTPSListener`, `HTTPRouteWeight`, `HTTPRouteServiceTypes`,
`HTTPRouteRedirectHostAndStatus`, `HTTPRouteRequestHeaderModifier`,
`HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidBackendRefUnknownKind`,
`HTTPRouteInvalidParentRefNotMatchingSectionName`, plus the Extended trio
`HTTPRouteMethodMatching`, `HTTPRouteResponseHeaderModifier`,
`HTTPRouteTimeoutRequest`. **This confirms the #1-agent finding precisely:**
status/setup passes, but the single shared edge address with no per-Gateway
allocation means every live-traffic test fails at the address step.

**2. `ReferenceGrant` / cross-namespace enforcement not in the routing model
(~8 tests).** `GatewaySecretInvalidReferenceGrant`,
`GatewaySecretMissingReferenceGrant`, `HTTPRouteCrossNamespace`,
`HTTPRouteInvalidCrossNamespaceBackendRef`,
`HTTPRouteInvalidCrossNamespaceParentRef`, `HTTPRouteInvalidReferenceGrant`,
`HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`, `HTTPRouteReferenceGrant`.
Aether's data-plane cluster name is namespace-free (`<svc>.<meshDomain>`), so
cross-namespace `backendRef` **enforcement** (allow with a grant, reject without
one) is not modeled. Note the two *positive* secret-grant tests (specific +
all-in-namespace) **do** pass — aether honors a present grant for a TLS secret;
it's the *negative*/enforcement and cross-namespace-backend cases that fail.

**3. Status edge-cases (~3 tests).** `GatewayClassObservedGenerationBump`
(`observedGeneration should increment` — the GatewayClass status generation isn't
bumped on spec change), and `GatewayInvalidRouteKind` (listener `ResolvedRefs`
must go `False` with reason `InvalidRouteKinds` and surface the supported kinds —
not produced correctly). `GatewayInvalidTLSConfiguration` also fails here
(invalid-cert listener status expectations).

### Inferred-features confirmation

The suite logged:

```
Supported features for GatewayClass aether: [ReferenceGrant GRPCRoute Gateway
  GatewayPort8080 HTTPRoute HTTPRouteMethodMatching HTTPRouteRequestTimeout
  HTTPRouteResponseHeaderModification]
```

`ReferenceGrant` is the GATEWAY-HTTP profile's mandatory feature, auto-added on
top of the 7 aether advertises. All other features (redirect status codes,
rewrite, mirror, CORS, backend-protocol, static addresses, listener-sets,
BackendTLSPolicy, client-cert, …) were **skipped, not failed** — (b) working.

## Prioritized blockers (what to do next)

| Priority | Blocker | Profile | Impact |
|---|---|---|---|
| **P0 (GW)** | **Per-Gateway address allocation.** One edge Deployment / one LB (`192.168.100.101`) with no per-Gateway `.status.addresses`. | GATEWAY-HTTP | Converts the single largest bucket (~17 Core + 3 Extended traffic tests) from FAIL toward PASS. Needs either per-Gateway LB addresses or a shared-address/host-multiplexing model the suite tolerates (publish a reachable address in `.status.addresses` and route by host). |
| **P0 (Mesh)** | **Apply HTTPRoute filters on the GAMMA / transparent-capture path.** Redirect + request/response header-modifier translate at the edge but not on capture. | MESH-HTTP | Converts `MeshHTTPRouteRequestHeaderModifier` + `MeshHTTPRouteRedirectHostAndStatus` from FAIL to PASS (~6/7 target). |
| **P1** | **`ReferenceGrant` cross-namespace enforcement** (reject cross-ns backendRef without a grant; honor with one). | GATEWAY-HTTP | ~8 Core tests; mandatory feature of the profile. |
| **P2** | **`RequestMirror` filter** (advertised as `Planned`). | both | Unlocks `HTTPRouteRequestMirror*` (currently skipped, not failed). |
| **P2** | **Status edge-cases**: GatewayClass `observedGeneration` bump; listener `InvalidRouteKinds` `ResolvedRefs`+`supportedKinds`; invalid-TLS listener status. | GATEWAY-HTTP | ~3 Core status tests. |
| **P3** | **Mesh interception model**: tighten weighted distribution on capture (`MeshHTTPRouteWeight`); decide arbitrary-Service interception vs. aether's generated mesh VIPs. | MESH-HTTP | Last mesh FAIL after filters land. |

## Reproduction

Identical to the baseline: a ~130-LOC standalone test in a `gateway-api` v1.5.1
checkout under `conformance/`, gated by `AETHER_REV2=1`, run with `GOWORK=off`.
Two subtests: `TestAetherRev2Mesh` (explicit mesh features) and
`TestAetherRev2Gateway` (features **inferred** from
`GatewayClass.status.supportedFeatures`). Non-obvious requirements when driving
the suite programmatically: set `ManifestFS = conformance.Manifests`, set
`AllowCRDsMismatch` on the mixed-channel cluster, and — for the inference path —
leave `SupportedFeatures`/`ExemptFeatures` empty with
`EnableAllSupportedFeatures=false` so the suite reads the GatewayClass status.
