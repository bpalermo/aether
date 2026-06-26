# Gateway API conformance — rev19: FIX B regressed, Core still 32/33 (2026-06-26)

A nineteenth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev 101)**, image/commit **`9b7ddd5`** (#373). This run targeted the
expected **GATEWAY-HTTP-Core-complete** confirmation: rev18 was 39 PASS / 4 FAIL
(Core 32/1, Extended 7/3), and `#373` shipped **FIX B** (empty-remainder
`ReplacePrefixMatch` rewrite via `regex_rewrite`) for `HTTPRouteRewritePath`, plus
**FIX A** (a regression test asserting the per-Gateway HCM strips the host-port) for
the `HTTPRouteHostnameIntersection` `very.specific.com:1234` Core case.

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces
and per-Gateway LoadBalancer Services were cleaned up afterward (admin on, Phase 2
on). `api.palermo.dev` verified **200** (LB `192.168.100.101`, HTTP→HTTPS **301**)
both before and after the run — no production-shape regression.

## TL;DR — flat vs rev18, both expected flips did NOT happen

| Profile | Tests run | PASS | FAIL | SKIP | DELTA vs rev18 (39/4) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **43** | **39** | **4** | 0 | **+0** (byte-identical) | Core **32 PASS / 1 FAIL**, Extended **7 PASS / 3 FAIL**. Same four fails as rev18. |
| **MESH-HTTP** | 7 | **4** | **3** | 0 | **+0** | 022 untouched; same 3 stable capture-path fails. |

**Headline:** GATEWAY-HTTP is **byte-for-byte identical to rev18** — `39/4`, Core
`32/33`, Extended `7/10`. **Neither expected flip materialized:**

- **`HTTPRouteRewritePath` (FIX B) did NOT flip — it REGRESSED into a deterministic
  bug.** The empty-remainder `regex_rewrite` emits the substitution string **`/$1`
  literally** instead of substituting the capture group. The echo backend reports
  `expected path to be /, got /$1` and `expected path to be /three, got /$1`
  (119 probes, all literal `/$1`). Root cause: Envoy's RE2
  `RegexMatchAndSubstitute.substitution` references capture groups with **`\1`**, not
  **`$1`**. `#373` used `Substitution: "/$1"`. **Real code bug.**
- **`HTTPRouteHostnameIntersection` (Core, FIX A) did NOT pass.** Despite FIX A's
  claim, the `Host: very.specific.com:1234` (host-with-port) assertion returns a
  **deterministic 404** — reproduced in both the full suite (60/60 probes) and an
  isolated re-run (35/35 probes), **never a single 200**. This is **not** the
  environmental/transient blip rev18's notes hypothesized; it is a **reproducible
  code gap**. **GATEWAY-HTTP-Core is therefore 32/33 — NOT complete.**

## GATEWAY-HTTP — Core 32/33 (NOT complete), Extended 7/10

```
GATEWAY-HTTP  core:     Passed: 32  Failed: 1   (HTTPRouteHostnameIntersection)
GATEWAY-HTTP  extended: Passed: 7   Failed: 3   (HTTPRouteMethodMatching,
                                                 HTTPRouteRedirectPortAndScheme,
                                                 HTTPRouteRewritePath)
```

**Core count: 32 / 33. GATEWAY-HTTP-Core is NOT complete** (one deterministic Core
fail: `HTTPRouteHostnameIntersection`).

DELTA lineage of GATEWAY-HTTP PASS/FAIL:

| Rev | PASS/FAIL | Core | Extended | Note |
|---|---|---|---|---|
| baseline / rev2 / rev3 | 6 / 31 | — | — | setup→address→bare-IP-TLS walls |
| rev6 / rev7 | 12 / 25 | — | — | uniform-404 traffic wall |
| rev8 | 12 / 25 | — | — | diagnostic (route-not-projected) |
| **rev18** | **39 / 4** | **32 / 1** | **7 / 3** | per-Gateway addressing + routing landed |
| **rev19** | **39 / 4** | **32 / 1** | **7 / 3** | **+0; FIX B regressed, FIX A ineffective** |

## Each remaining FAIL — flake-vs-real classification (with isolated re-runs)

Each failing test was re-run **3×in isolation** (single-test `RunTest`, base resources
set up per iteration, namespace fully drained between iterations to avoid setup
collisions). Verdicts:

| Test | Profile | Failure mode | Isolated re-run pass-rate | Verdict |
|---|---|---|---|---|
| **HTTPRouteHostnameIntersection** | **Core** | `Host: very.specific.com:1234 /s1` → **404** (host-with-port not stripped before vhost match; falls to `*` catch-all). The no-port `very.specific.com` case PASSES. | **0/1 valid** (35/35 probes 404; iter2/iter3 hit a back-to-back ns-terminating setup collision → invalid, not aether) | **REAL — deterministic code gap.** Not a flake. |
| **HTTPRouteRewritePath** | Extended | Echo backend gets path **`/$1` literally** (`got /$1`, expected `/` and `/three`). | **0/3** (119 literal-`/$1` probes; reproduced every run) | **REAL — deterministic code bug** (FIX B regression: `$1` should be `\1`). |
| **HTTPRouteMethodMatching** | Extended | A single **503** on the first method probe (`/`, empty Host) during cluster warmup; the 11/12 method-routing assertions themselves are correct. | **0/3** (1 transient 503 each run; never a logic/404 error) | **AVAILABILITY (warmup) — reproducible, NOT a routing-logic gap.** Cold-cluster convergence races the suite's `MaxTimeToConsistency`/`RequestTimeout`. |
| **HTTPRouteRedirectPortAndScheme** | Extended | In the full suite: redirect probes `302/301 → 404`. In isolation: the test's multi-listener redirect **Gateway never reaches `Programmed/Ready=True`** within 180s (`Ready condition set to False, expected True`). | **0/3** (Gateway-readiness 180s timeout each run) | **READINESS/CONVERGENCE — reproducible, environment-sensitive.** A redirect-Gateway-programming convergence gap, not a redirect-logic gap (PortRedirect/SchemeRedirect features are advertised and the simple redirect tests pass). |

> **De-flake note:** none of the four "flipped to PASS" on isolated re-run, so by the
> strict definition none is a *transient* flake. But the failure **classes** differ
> sharply: two are **deterministic code defects** (HostnameIntersection host-port,
> RewritePath `$1`), and two are **convergence/availability** gaps (MethodMatching
> warmup 503, RedirectPortAndScheme Gateway-readiness timeout) that are reproducible
> under the suite's tight timing but are **not routing-logic errors**.

### Root cause — `HTTPRouteRewritePath` (FIX B regression)

`agent/internal/xds/proxy/route.go:501-504`, the empty-remainder branch of
`applyURLRewrite`:

```go
ra.RegexRewrite = &matcherv3.RegexMatchAndSubstitute{
    Pattern:      &matcherv3.RegexMatcher{Regex: "^" + regexp.QuoteMeta(matchPrefix) + `/?(.*)$`},
    Substitution: "/$1",   // BUG: RE2 uses \1, not $1 — Envoy forwards "/$1" verbatim
}
```

Envoy's `RegexMatchAndSubstitute.substitution` is RE2 replacement syntax, where a
capture group is referenced as **`\1`** (the proto docs' own example is
`\1`). `$1` is not interpreted, so the literal two characters `$1` are appended,
yielding the upstream path `/$1`. The fix is `Substitution: "/\\1"`. The
`ReplaceFullPath` branch immediately below (line 511-513) does **not** use a capture
group, so it is unaffected and `HTTPRouteRewritePath`'s `ReplaceFullPath` cases (if
any) are not the failing ones — only the `ReplacePrefixMatch` empty-remainder
substitution.

### Analysis — `HTTPRouteHostnameIntersection` (Core, host-port)

The per-Gateway HCM **does** set
`strip_port_mode: strip_any_host_port: true` on every edge listener
(`agent/internal/xds/proxy/edge.go:121/141/167/351`), and the Go uses the correct
`HttpConnectionManager_StripAnyHostPort` oneof variant — so the proto is not the
`strip_matching_host_port` foot-gun. `strip_any_host_port` should strip an arbitrary
`:1234` before vhost domain selection. Yet `Host: very.specific.com` routes (200) and
`Host: very.specific.com:1234` falls through to the `*` catch-all (404),
deterministically, on a **multi-listener** Gateway (listener-1 `very.specific.com`,
listener-2/3 `*.wildcard.io` / `*.anotherwildcard.io`) served by one merged RDS route
config. The strip is configured but **ineffective for the ported authority on this
multi-listener shape** — the most likely cause (per Envoy semantics review) is that
the `:1234` request lands on a filter chain / HCM path that does not carry the strip,
or the merged-listener authority retains the port at vhost-selection time. This needs
a live `/config_dump` capture of the multi-listener Gateway to pinpoint, but the
**conformance verdict is unambiguous: a reproducible Core failure, not a flake.**
rev18's "environmental/transient" hypothesis is **disproven** by two independent runs.

## MESH-HTTP — 4 PASS / 3 FAIL (022 unchanged)

```
MESH-HTTP core: Passed: 4  Failed: 3
```

**FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — the same three stable
capture-path fails as rev3/rev18/baseline. The HTTPRoute redirect / header-modifier
filters and weighted distribution apply on the edge route path but not on the GAMMA
transparent-capture path the MESH profile exercises. Nothing in `#373` touches the
mesh data path → correctly **flat**. **MESH-HTTP confirmed unchanged.**

## Headline verdict

- **GATEWAY-HTTP-Core is NOT conformant: 32 / 33.** One deterministic Core failure
  remains — `HTTPRouteHostnameIntersection`'s host-with-port (`very.specific.com:1234`)
  case 404s reproducibly despite `strip_any_host_port` being set, on the
  multi-listener shape. **CORE-COMPLETE is NOT confirmed.** FIX A did not close it.
- **The honest Extended remainder is 7 / 10**, with **one real code bug** and **two
  convergence/availability gaps**:
  - `HTTPRouteRewritePath` — **real code bug** (FIX B regressed: `/$1` should be
    `/\1`); trivially fixable, but currently a deterministic FAIL, not a flake.
  - `HTTPRouteMethodMatching` — **availability/warmup**, reproducible; routing logic
    is correct, one cold-start 503.
  - `HTTPRouteRedirectPortAndScheme` — **Gateway-readiness/convergence**,
    reproducible; the multi-listener redirect Gateway doesn't reach `Programmed=True`
    in the window.
- **Net delta vs rev18: +0.** The two expected flips (Core via FIX A, Extended via
  FIX B) **both failed to land**: FIX A was ineffective for the multi-listener
  host-port shape, and FIX B introduced a deterministic `$1`-literal regression.

## Reproduction

Same programmatic runner as rev2–rev18 (`conformance/aether_rev19_test.go`, a copy of
the rev18 one-off with the env gate renamed `AETHER_REV19` and the version held at
`0.51.0`, in a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api`, **not
committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`,
  `AllowCRDsMismatch: true`, `CleanupBaseResources: true`.
- **GATEWAY-HTTP:** features **inferred** from `GatewayClass.status.supportedFeatures`
  (empty `SupportedFeatures` + `EnableAllSupportedFeatures=false`).
- **MESH-HTTP:** features advertised explicitly (`SupportMesh`, `SupportHTTPRoute`,
  `…ResponseHeaderModification`, `…MethodMatching`, `…RequestTimeout`).
- Timeouts: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
  `RequestTimeout=10s`.
- Isolated de-flake re-runs via a `RunTest`-scoped helper
  (`conformance/aether_rev19_isolated_test.go`, env `AETHER_ISO_TEST=<ShortName>`),
  each draining `gateway-conformance-infra` before the next iteration.

Admin was available on the edge (`127.0.0.1:9901`) but not required for the score.
GATEWAY-HTTP took ~7.7 min; each isolated re-run added the base-resource setup cost.
