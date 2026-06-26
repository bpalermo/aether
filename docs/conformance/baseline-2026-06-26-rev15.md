# Gateway API conformance — rev15: match vocabulary + 500/status fixes + feature advertising (2026-06-26)

A fifteenth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev 94)**, image/commit **`45205aa`**. Two PRs landed since rev14
(22 PASS / 15 FAIL):

- **#362** — full HTTPRoute match vocabulary (header / method / query matching with
  specificity-aware sort) **and** unresolvable-backendRef → HTTP **500**
  `direct_response`.
- **#361** — `observedGeneration` stamping on invalid-TLS Gateways; **advertised +5
  redirect/rewrite features** (HTTPRoutePortRedirect / SchemeRedirect / PathRedirect /
  HostRewrite / PathRewrite); **removed GRPCRoute from the edge GatewayClass** so the
  GRPC tests no longer run against an edge that can't serve them.

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces and
per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR — biggest single-rev jump yet: 22/15 → 33/10

| Profile | Tests run | PASS | FAIL | SKIP | DELTA vs rev14 (22/15) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **43** | **33** | **10** | **0** | **+11 PASS / −5 FAIL** | The 404 wall is gone. 7 of the rev14 fails flipped on status/match fixes; the +5 redirect/rewrite features made those tests *run*, 2 of which now pass and 2 reveal real bugs. |
| **MESH-HTTP** | 7 | **4** | **3** | 0 | flat (rev14 read 4/3) | Unchanged. Proposal 022 (filters on the GAMMA capture path) still not implemented; the 3 stable capture-path FAILs are identical to every prior rev. |

GATEWAY-HTTP profile breakdown (authoritative, from the report):

- **core:** 27 PASS / 6 FAIL (rev14: 20/13)
- **extended:** 6 PASS / 4 FAIL (rev14: 2/2)

The GATEWAY run finished in **810 s (~13.5 min)** — *3× faster* than rev14's ~36 min,
because the uniform-404 wall (every test burning its full 60 s consistency budget) is
gone: passing tests now succeed in a few seconds.

## What flipped (rev14 FAIL → rev15 PASS)

Eleven tests moved to PASS. By driver:

**#362 status/match fixes (7):**

| Test | Why it now passes |
|---|---|
| `HTTPRouteHeaderMatching` | header-match vocabulary implemented |
| `HTTPRouteMatchingAcrossRoutes` | the residual header sub-probe now matches; full PASS |
| `HTTPRouteInvalidBackendRefUnknownKind` | status-only assertion (`ResolvedRefs=False`, `InvalidKind`) now correct |
| `HTTPRouteInvalidCrossNamespaceBackendRef` | status-only assertion now correct |
| `HTTPRouteInvalidReferenceGrant` | status-only assertion now correct |
| `HTTPRouteReferenceGrant` | ReferenceGrant resolution status correct |
| `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant` | status-only assertion now correct |

**#361 feature advertising — redirect tests that now run *and* pass (2):**

| Test | Note |
|---|---|
| `HTTPRouteRedirectPath` | path-redirect works |
| `HTTPRouteRedirectHostAndStatus` | host + status redirect works |

(`HTTPRouteSimpleSameNamespace`, `HTTPRouteWeight`, `HTTPRouteHTTPSListener` were
already green at rev14 and stayed green — **no regression** from the route-builder
changes in #362. The api.palermo.dev-shape / single-`*` cases did not regress.)

## The 500 question — answered: it did NOT take effect on the traffic path

**`got 500` count across the whole run = 0.** The 3 invalid-ref tests that flipped did
so on their **status** assertions, not on a 500 response. The one invalid-ref test that
still fails — **`HTTPRouteInvalidNonExistentBackendRef`** — fails *specifically on the
traffic sub-probe*:

- Its status sub-test (`ResolvedRefs=False` / `BackendNotFound`) **passes**.
- Its HTTP sub-test (`HTTP Request to invalid nonexistent backend receive a 500`)
  **fails: expected 500, got 503** — 209 times across the 60 s window.

So #362's `direct_response: 500` for an unresolvable backendRef is **not reaching the
data plane for the nonexistent-backend case**: Envoy returns its native **503** (no
healthy upstream / empty cluster) instead of a programmed 500 `direct_response`. The
expected bucket-B flip to `got 500` **did not materialise** — the route still falls
through to an empty cluster rather than a 500 direct response. This is the one
correction to the rev15 expectations.

## GRPC tests now SKIP (as expected)

With GRPCRoute removed from the edge GatewayClass (#361), the suite reads
`supportedFeatures` *without* `GRPCRoute` and **skips** the 5 GRPC tests
(`GRPCExactMethodMatching`, `GRPCRouteHeaderMatching`,
`GRPCRouteListenerHostnameMatching`, `GRPCRouteNamedRule`, `GRPCRouteWeight`) with
`suite does not support GRPCRoute`. In rev14 these ran (and could not be served). This
is a clean removal of dead weight, not a score change in the in-profile sense — the
GATEWAY-HTTP profile SKIP count stays 0 (GRPC tests are out-of-profile and don't count
toward the profile stats), but they no longer *run*.

The advertised `supportedFeatures` set the suite inferred from
`GatewayClass.status.supportedFeatures` this rev:

```
Gateway, GatewayPort8080, HTTPRoute, HTTPRouteHostRewrite, HTTPRouteMethodMatching,
HTTPRoutePathRedirect, HTTPRoutePathRewrite, HTTPRoutePortRedirect,
HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, HTTPRouteSchemeRedirect,
ReferenceGrant
```

(vs rev14: `Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant, GRPCRoute`
— i.e. **−GRPCRoute, +5 redirect/rewrite features**, exactly #361.)

## Re-bucketed remaining 10 failures

| Bucket | Test | Profile | Failure mode | Status |
|---|---|---|---|---|
| **A — wildcard/hostname promotion** | `HTTPRouteListenerHostnameMatching` | core | `observedGeneration` stale (0/2 conditions updated to gen 1) | KNOWN-REMAINING; failure mode *moved* from traffic catch-all to a status/observedGeneration stamp gap |
| **A — wildcard/hostname promotion** | `HTTPRouteHostnameIntersection` | core | same `observedGeneration` stale (0/2) | KNOWN-REMAINING; same status-stamp gap |
| **B — invalid backendRef → 500** | `HTTPRouteInvalidNonExistentBackendRef` | core | traffic sub-probe `expected 500, got 503` (209×); status sub-probe PASSES | NEW localisation: status fixed, **500 direct_response not on the data path** |
| **C — match vocabulary** | `HTTPRouteMatching` | core | path-match probes 503 (`/v2`, `/v2example` not routed) | partial: header-match fixed, **path-match still drops to 503** |
| **C — match vocabulary** | `HTTPRouteMethodMatching` | extended | method-match probes 503 (`/path1` GET → 503) | partial: **method-match not selecting backend** |
| **D — request timeout** | `HTTPRouteTimeoutRequest` | extended | `expected 504, got 200`; 1 s backend delay returns 200 in ~1003 ms — **timeout not enforced** | advertised `HTTPRouteRequestTimeout` but **not wired into the route** |
| **D — request timeout** | `HTTPRouteRedirectPortAndScheme` | extended | mixed: a `504→200` timeout sub-assertion on the shared route + scheme/port probes | feature *runs*; fails on the un-enforced timeout + port/scheme handling |
| **G — path rewrite bug** | `HTTPRouteRewritePath` | extended | `expected path /three, got //three` — **double-slash** | NEW: PathRewrite implemented but off-by-one slash |
| **E — headless** | `HTTPRouteServiceTypes` | core | headless/ExternalName Service backends | KNOWN-REMAINING, unchanged |
| **F — invalid-TLS** | `GatewayInvalidTLSConfiguration` | core | listener with bad cert ref reports `ResolvedRefs=True` instead of `False`/`InvalidCertificateRef` | NOT a flake this rev — **still red**; #361 stamped `observedGeneration` but did not add malformed-cert-ref detection |

### Bucket roll-up with counts

- **A (wildcard/hostname promotion):** 2 — `HTTPRouteListenerHostnameMatching`,
  `HTTPRouteHostnameIntersection`. *Failure mode changed*: no longer the traffic
  catch-all + `Host:port`; now a stale `observedGeneration` on Accepted/Programmed.
- **B (invalid-ref 500):** 1 — `HTTPRouteInvalidNonExistentBackendRef`. 3 of the rev14
  bucket-B four flipped on status; this one needs the **500 on the data path**.
- **C (match vocabulary):** 2 — `HTTPRouteMatching`, `HTTPRouteMethodMatching`. Header
  matching is fixed (HeaderMatching PASS); path and method matching still 503.
- **D (request timeout):** 2 — `HTTPRouteTimeoutRequest`,
  `HTTPRouteRedirectPortAndScheme`. Advertised `HTTPRouteRequestTimeout` is not
  enforced (1 s delay → 200, not 504).
- **E (headless):** 1 — `HTTPRouteServiceTypes`. Unchanged.
- **F (invalid-TLS):** 1 — `GatewayInvalidTLSConfiguration`. **Re-checked: still red**,
  not a timing flake; needs malformed/terminate-cert-ref detection.
- **G (path-rewrite bug, NEW):** 1 — `HTTPRouteRewritePath`. Double-slash `//three`.

SKIP count change from the feature edits: the 5 GRPC tests **no longer run** (were
running in rev14); they are out-of-profile so the GATEWAY-HTTP profile SKIP stat stays
**0** in both revs. The redirect/rewrite tests that previously did not run now run (and
appear as 2 PASS + 2 FAIL above).

## MESH-HTTP — unchanged

`MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`,
`MeshHTTPRouteWeight` fail; the other 4 pass — **bit-for-bit identical to rev14** and to
every prior rev. Proposal 022 (HTTPRoute filters on the GAMMA/capture path) is not
implemented; #361/#362 are edge-only and do not touch the mesh data path, so this is
correctly flat.

## DELTA vs prior revs

| | baseline (rev1) | rev3 (rev 72) | rev13 (rev ~92) | rev14 (rev 94, pre) | **rev15 (rev 94)** |
|---|---|---|---|---|---|
| GATEWAY-HTTP PASS / FAIL | 0 / 0 (setup-blocked) | 6 / 31 | 20 / 17 | 22 / 15 | **33 / 10** |
| GATEWAY-HTTP SKIP (in-profile) | (all) | 0 | 0 | 0 | **0** |
| GATEWAY core PASS / FAIL | — | 6 / 31 | 18 / 15 | 20 / 13 | **27 / 6** |
| GATEWAY extended PASS / FAIL | — | — | 2 / 2 | 2 / 2 | **6 / 4** |
| Traffic-phase result | n/a | TLS-SAN wall | uniform 404 | uniform 404 | **mostly real traffic; residual 503/504/slash** |
| GRPC tests | run | run | run | run | **SKIP (GRPCRoute removed)** |
| Run wall time (GATEWAY) | — | ~2200 s | ~2200 s | ~2210 s | **810 s** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 4 / 3 | 4 / 3 | 4 / 3 | **4 / 3** |

## Prioritized remaining blockers (rev15)

| Priority | Blocker | Fix |
|---|---|---|
| **P1 (GW)** | `HTTPRouteMatching` / `HTTPRouteMethodMatching` path+method probes 503. | #362 fixed header matching; extend the same specificity-aware match to path-prefix/exact and method so they select the backend instead of falling through to an empty cluster. |
| **P1 (GW)** | Request timeout not enforced (`HTTPRouteTimeoutRequest`, and the 504 sub-probe of `HTTPRouteRedirectPortAndScheme`). | Wire `HTTPRoute.spec.rules.timeouts.request` into the route's `route.timeout` so a 1 s upstream delay yields 504, not 200. |
| **P2 (GW)** | Invalid nonexistent backendRef returns native 503, not the configured 500. | Program the `direct_response: 500` (or a 500 local-reply) when the *only* backendRef is unresolvable, instead of an empty cluster that Envoy answers 503. |
| **P2 (GW)** | `HTTPRouteRewritePath` emits `//three` (double slash). | Off-by-one in the prefix-strip / path-rewrite join; normalise the leading slash. |
| **P2 (GW)** | `HTTPRouteListenerHostnameMatching` / `HTTPRouteHostnameIntersection` stale `observedGeneration`. | Stamp `observedGeneration` on Accepted/Programmed for these Gateways (the same stamping #361 added for invalid-TLS, applied to the hostname-intersection path). |
| **P3 (GW)** | `GatewayInvalidTLSConfiguration` reports `ResolvedRefs=True` for a bad cert ref. | Add malformed / missing terminate-cert detection → `ResolvedRefs=False` / `InvalidCertificateRef`. |
| **P3 (GW)** | `HTTPRouteServiceTypes` headless backends. | Support headless / ExternalName Service backends. |
| **P0 (Mesh)** | HTTPRoute filters on the GAMMA/capture path (proposal 022). | Unchanged since baseline. |

## Is aether GATEWAY-HTTP-conformant now?

**Not yet — but rev15 is the largest jump yet (22/15 → 33/10) and the remaining tail is
small, well-localised, and individually fixable.** The headline corrections to the
pre-run expectations: (1) the invalid-ref **500 did not reach the data path** — 3 of the
4 bucket-B tests flipped on *status* only, and the one traffic-500 test still gets a
native **503**; (2) **bucket F is still red**, not a flake — `observedGeneration`
stamping helped the invalid-TLS Gateway but the malformed-cert detection it needs is
still absent; (3) the +5 features made redirect/rewrite tests run: **2 redirect tests
pass, but `HTTPRouteRewritePath` (double-slash) and `HTTPRouteRedirectPortAndScheme`
(un-enforced timeout) fail**, and the previously-extended `HTTPRouteTimeoutRequest`
fails for the same un-enforced-timeout reason. No regression on any previously-green
test. Bucket A's two tests are unchanged in count but their *failure mode moved* from a
traffic 404 to a status `observedGeneration` gap.

## How it was run

Same programmatic runner as rev2–rev14 (`conformance/aether_rev15_test.go`, a copy of
the rev14 one-off with the env gate renamed `AETHER_REV15` and the version held at
`0.51.0`, in a `gateway-api` v1.5.1 checkout with `replace sigs.k8s.io/gateway-api =>
../`, **not committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures` (empty `SupportedFeatures` +
  `EnableAllSupportedFeatures=false` → suite inference). The inferred set **changed via
  #361** (−GRPCRoute, +5 redirect/rewrite features; full list above).
- **MESH-HTTP:** mesh features advertised explicitly
  (`Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
  `RequestTimeout=10s`.

The GATEWAY run took **810 s (~13.5 min)**; MESH finished in ~3 min. Admin
(`127.0.0.1:9901`) was available but not needed — the data-plane behaviour is now
readable straight from the suite's expectation diffs (503/504/`//three`), no
config-dump required.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces after each
profile; the per-Gateway LoadBalancer Services GC'd back to the single production edge.
Final state:

```
$ kubectl get ns | grep conformance                       → none
$ kubectl get gateway -A | grep conformance               → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

No aether MetalLB pool IPs leaked — the only `aether.io/edge-gateway` Service is the
production `.101`. (Unrelated `.99`/`.100` LoadBalancers live in
`envoy-gateway-system`, a pre-existing Envoy Gateway install, not an aether conformance
leak.) Port-forwards killed.
