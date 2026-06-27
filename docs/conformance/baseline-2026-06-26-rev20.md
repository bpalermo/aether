# Gateway API conformance — rev20: GATEWAY-HTTP-Core 33/33 COMPLETE (2026-06-26)

A twentieth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev 103)**, image/commit **`9d6f53c`** (#376). This run targeted the
**GATEWAY-HTTP-Core-complete (33/33)** confirmation. rev18/rev19 were both `39/4`
(Core `32/1`, Extended `7/3`). Since rev19, two fix PRs deployed:

- **#375** (FIX B) — empty-remainder prefix-rewrite `ReplacePrefixMatch` now uses the
  correct **RE2 substitution syntax `/\1`** (rev19 regressed on `/$1`, which Envoy
  forwarded verbatim).
- **#376** — three real bugs the rev19 diagnostic found via the live runner +
  `config_dump`:
  1. **`HTTPRouteHostnameIntersection`** (Core) — replaced the over-aggressive
     `strip_any_host_port` HCM mode with `RouteConfiguration.ignore_port_in_host_matching:
     true`. The suite asserts the **backend receives the ported authority**
     (`very.specific.com:1234`), which `strip_any_host_port` was destroying.
  2. **`HTTPRouteMethodMatching`** (Extended) — fixed route-specificity sort to rank
     **method ABOVE header-count** per Gateway API precedence.
  3. **`HTTPRouteRedirectPortAndScheme`** (Extended) — fixed `gatewayServiceName`
     63-char truncation collision (added an 8-char hash suffix) so co-located Gateways
     don't share a per-Gateway Service name / lose their address.

Aether was **not** modified or redeployed (HEAD `9d6f53c` == deployed image commit;
the `/\1`, `IgnorePortInHostMatching: true`, and gatewayServiceName changes were
verified present in the deployed source before the run). All `gateway-conformance-*`
namespaces and per-Gateway LoadBalancer Services were cleaned up afterward (admin on,
Phase 2 on). `api.palermo.dev` verified **200** (HTTP→HTTPS **301**) both before and
after the run — no production-shape regression from the host-port HCM/route change.

## TL;DR — Core 33/33 COMPLETE, the two expected flips both landed

| Profile | Tests run | PASS | FAIL | SKIP | DELTA vs rev18 (39/4) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **43** | **42** | **1** | 0 | **+3 PASS / −3 FAIL** | Core **33 PASS / 0 FAIL = COMPLETE**, Extended **9 PASS / 1 FAIL**. |
| **MESH-HTTP** | 7 | **4** | **3** | 0 | **+0** | 022 untouched; same 3 stable capture-path fails. |

**Headline:** **GATEWAY-HTTP-Core is COMPLETE at 33 / 33** — both expected flips
materialized this rev:

- **`HTTPRouteHostnameIntersection` (Core) FLIPPED to PASS.** The host-with-port case
  `request to 'very.specific.com:1234/s1' should go to infra-backend-v1` **PASSES**
  (the runner asserts the **backend receives the ported authority**; `#376`'s
  `ignore_port_in_host_matching` matches the vhost without stripping the port). This
  closes the one deterministic Core gap that held rev18/rev19 at 32/33.
- **`HTTPRouteRewritePath` (Extended, FIX B) FLIPPED to PASS.** All 6 subtests pass,
  including the empty-remainder `/strip-prefix → infra-backend-v1` case that rev19
  failed with the literal `/$1`. `#375`'s `/\1` substitution is correct.
- **`HTTPRouteMethodMatching` (Extended) FLIPPED to PASS.** `#376`'s method-above-header
  precedence sort fixed it; no warmup 503 this run.

The **single remaining FAIL is `HTTPRouteRedirectPortAndScheme` (Extended)** — a
**Gateway address-convergence timeout**, not a redirect-logic gap (see below).

## GATEWAY-HTTP — Core 33/33 (COMPLETE), Extended 9/10

```
GATEWAY-HTTP  core:     Passed: 33  Failed: 0   (result: success)
GATEWAY-HTTP  extended: Passed: 9   Failed: 1   (HTTPRouteRedirectPortAndScheme)
summary: Core tests succeeded. Extended tests failed with 1 test failures.
```

**Core count: 33 / 33. GATEWAY-HTTP-Core is COMPLETE.** Profile result `success`,
zero Core failures, zero Core skips.

The 4 targeted tests from the brief — all resolved except the address-convergence one:

| Targeted test | Profile | rev18/19 | rev20 | Note |
|---|---|---|---|---|
| `HTTPRouteHostnameIntersection` | **Core** | FAIL (`:1234`→404) | **PASS** | `very.specific.com:1234/s1` → infra-backend-v1; **backend receives the ported Host** (suite asserts it). `ignore_port_in_host_matching` works. |
| `HTTPRouteRewritePath` | Extended | FAIL (`/$1`) | **PASS** | all 6 incl. empty-remainder `/strip-prefix`; `/\1` substitution correct. |
| `HTTPRouteMethodMatching` | Extended | FAIL (warmup 503) | **PASS** | method-above-header sort; clean. |
| `HTTPRouteRedirectPortAndScheme` | Extended | FAIL | **FAIL** | NOT a redirect-logic gap — **Gateway never gets an address in status** within 180s (convergence). See classification. |

DELTA lineage of GATEWAY-HTTP PASS/FAIL:

| Rev | PASS/FAIL | Core | Extended | Note |
|---|---|---|---|---|
| baseline / rev2 / rev3 | 6 / 31 | — | — | setup→address→bare-IP-TLS walls |
| rev6 / rev7 | 12 / 25 | — | — | uniform-404 traffic wall |
| rev8 | 12 / 25 | — | — | diagnostic (route-not-projected) |
| **rev18** | **39 / 4** | **32 / 1** | **7 / 3** | per-Gateway addressing + routing landed |
| **rev19** | **39 / 4** | **32 / 1** | **7 / 3** | +0; FIX B regressed (`/$1`), FIX A ineffective |
| **rev20** | **42 / 1** | **33 / 0** | **9 / 1** | **+3/−3; Core COMPLETE.** Hostname/RewritePath/MethodMatching all flip; only RedirectPortAndScheme (address-convergence) remains |

## The remaining FAIL — flake-vs-real classification (with isolated re-runs)

`HTTPRouteRedirectPortAndScheme` was re-run **3× in isolation** (single-test
`RunTest`, base resources set up per iteration, namespace drained between iterations).

| Test | Profile | Failure mode | Isolated re-run pass-rate | Verdict |
|---|---|---|---|---|
| **HTTPRouteRedirectPortAndScheme** | Extended | In both full suite (180.57s) and isolation: the multi-listener (80/8080/443) `same-namespace-with-http-listener-on-8080` Gateway **never reaches "at least one IP address in status"** within the 180s `GatewayMustHaveAddress` window — `error fetching Gateway: context deadline exceeded` / `Ready condition set to False, expected True`. The redirect probes never run because addressing never converges. | **0/3** (iter1 reproduced the full 180s address-wait timeout; iter2/iter3 failed fast at 0.6–0.8s on a back-to-back ns-terminating setup collision → invalid as score, not aether). | **READINESS/CONVERGENCE — reproducible, NOT a redirect-logic gap.** The standalone `HTTPRouteRedirectPort`, `HTTPRouteRedirectScheme`, `HTTPRouteRedirectPath`, `HTTPRouteRedirectHostAndStatus` tests all **PASS**, and `HTTPRoutePortRedirect`/`HTTPRouteSchemeRedirect` are advertised. The failure is per-Gateway LB address assignment on a multi-listener shape (proposal 021 Phase 2 territory). |

> **De-flake note:** the one valid isolated iteration reproduced the exact full-suite
> failure (180s address-wait), so this is **not a transient flake** — it is a
> **reproducible address-convergence gap** for the multi-listener redirect Gateway. It
> is **not** a routing/redirect-logic defect: every redirect-logic test that does not
> depend on this specific multi-listener Gateway's address passes.

## MESH-HTTP — 4 PASS / 3 FAIL (022 unchanged)

```
MESH-HTTP core: Passed: 4  Failed: 3  (result: failure)
```

**FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — the same three stable
capture-path fails as rev3/rev18/rev19/baseline. The HTTPRoute redirect /
header-modifier filters and weighted distribution apply on the edge route path but not
on the GAMMA transparent-capture path the MESH profile exercises. Nothing in
`#375`/`#376` touches the mesh data path → correctly **flat. MESH-HTTP confirmed
unchanged.** (`MeshHTTPRouteMatching`, `MeshHTTPRouteSimpleSameNamespace` pass;
`MeshBasic`/`MeshTrafficSplit` pass; `MeshFrontend` is an extended Mesh frontend test
unrelated to the core mesh-route score.)

## Headline verdict

- **GATEWAY-HTTP-Core IS CONFORMANT: 33 / 33 — COMPLETE.** Profile `core` result
  `success`, zero Core failures. The two long-standing blockers both closed this rev:
  `HTTPRouteHostnameIntersection`'s host-with-port case now routes (and the backend
  receives the ported authority) via `ignore_port_in_host_matching`, and
  `HTTPRouteRewritePath`'s empty-remainder rewrite is fixed (`/\1`).
- **The honest Extended remainder is 9 / 10**, with **zero real code bugs**:
  - `HTTPRouteRewritePath` — **fixed** (`/\1`), now PASS.
  - `HTTPRouteMethodMatching` — **fixed** (method-above-header precedence), now PASS.
  - `HTTPRouteRedirectPortAndScheme` — **the lone remaining FAIL**, classified as a
    **reproducible Gateway address-convergence gap** (multi-listener per-Gateway LB
    address never lands in status within 180s), **NOT a redirect-logic defect**
    (PortRedirect/SchemeRedirect features advertised; all standalone redirect tests
    pass). This is proposal 021 Phase 2 (distinct per-Gateway addressing) territory.
- **Net delta vs rev18: +3 PASS / −3 FAIL** (`39/4` → `42/1`). Both expected flips
  (Core via `ignore_port_in_host_matching`, Extended via `/\1` + method-sort) landed;
  `HTTPRouteRedirectPortAndScheme` flipped from a redirect-404 (rev18) to a clean
  address-convergence timeout, which is a convergence gap, not a regression.
- **No `api.palermo.dev`-shape regression:** 200 / 301 before and after — the host-port
  HCM/route change (`strip_any_host_port` → `ignore_port_in_host_matching`) did not
  disturb the production edge.

## Reproduction

Same programmatic runner as rev2–rev19 (`conformance/aether_rev20_test.go`, a copy of
the rev19 one-off with the env gate renamed `AETHER_REV20` and the version held at
`0.51.0`, in a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api`, **not
committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`,
  `AllowCRDsMismatch: true`, `CleanupBaseResources: true`.
- **GATEWAY-HTTP:** features **inferred** from `GatewayClass.status.supportedFeatures`
  (empty `SupportedFeatures` + `EnableAllSupportedFeatures=false`). Inferred set:
  `Gateway`, `GatewayPort8080`, `HTTPRoute`, `HTTPRouteHostRewrite`,
  `HTTPRouteMethodMatching`, `HTTPRoutePathRedirect`, `HTTPRoutePathRewrite`,
  `HTTPRoutePortRedirect`, `HTTPRouteRequestTimeout`,
  `HTTPRouteResponseHeaderModification`, `HTTPRouteSchemeRedirect`, `ReferenceGrant`.
- **MESH-HTTP:** features advertised explicitly (`SupportMesh`, `SupportHTTPRoute`,
  `…ResponseHeaderModification`, `…MethodMatching`, `…RequestTimeout`).
- Timeouts: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
  `RequestTimeout=10s`.
- Isolated de-flake re-runs via a `RunTest`-scoped helper
  (`conformance/aether_rev20_isolated_test.go`, env `AETHER_ISO_TEST=<ShortName>`),
  each draining `gateway-conformance-infra` before the next iteration.

GATEWAY-HTTP took ~4.8 min (288.8s); MESH-HTTP ~5.0 min (302.0s). Admin was available
on the edge (`127.0.0.1:9901`) but not required for the score.
