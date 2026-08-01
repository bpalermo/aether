# Gateway API conformance — rev21: GATEWAY-HTTP FULLY CONFORMANT 43/43 (2026-06-27)

A twenty-first run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.53.0 (helm rev 110)**, image/commit **`f7b7b35`** (#385). This run is the
**first fully-conformant GATEWAY-HTTP run**: Core **33/33** and Extended **10/10**,
zero failures, zero Core skips. rev20 was Core `33/0` but Extended `9/1` — the lone
remaining FAIL (`HTTPRouteRedirectPortAndScheme`) closed this rev, but only after a
five-PR chain of edge fixes, each exposing the next layer beneath the previous.

Same programmatic runner as rev2–rev20 (`conformance/aether_rev20_test.go` driving
`suite.NewConformanceTestSuite`, `GatewayClassName "aether"`, controller
`gateway.aether.io/edge`), **not committed** to the repo. Aether was modified and
redeployed this rev (rev20 was a re-run of an unchanged image; rev21 deploys the
`f7b7b35` edge fixes). `api.palermo.dev` verified **200 / 301** before and after — no
production-shape regression.

## TL;DR — GATEWAY-HTTP fully conformant; MESH-HTTP unchanged (3 known fails)

| Profile | Tests run | PASS | FAIL | SKIP | DELTA vs rev20 (42/1) | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **43** | **43** | **0** | 0 | **+1 PASS / −1 FAIL** | Core **33/33** AND Extended **10/10** = **FULLY CONFORMANT**. |
| **MESH-HTTP** | 7 | **4** | **3** | 0 | **+0** | **Untouched.** Same 3 GAMMA capture-path fails, blocked on proposal 022. |

**Headline:** **GATEWAY-HTTP is FULLY CONFORMANT — 33/33 Core + 10/10 Extended,
zero failures.** Test output: `--- PASS: TestAetherRev20GatewayHTTP (108.9s)`,
`"Core tests succeeded. Extended tests succeeded."`

> **MESH-HTTP is NOT conformant and was not touched this rev.** It remains at
> **4 PASS / 3 FAIL** — `MeshHTTPRouteRedirectHostAndStatus`,
> `MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — the GAMMA
> transparent-capture-path fails blocked on **proposal 022 (arbitrary-service
> interception)**. Do not read this doc as "everything passes": only the **edge
> GATEWAY-HTTP profile** is fully conformant.

## GATEWAY-HTTP — Core 33/33, Extended 10/10 (FULLY CONFORMANT)

```
GATEWAY-HTTP  core:     Passed: 33  Failed: 0   (result: success)
GATEWAY-HTTP  extended: Passed: 10  Failed: 0   (result: success)
summary: Core tests succeeded. Extended tests succeeded.
--- PASS: TestAetherRev20GatewayHTTP (108.9s)
```

**Core count: 33/33. Extended count: 10/10. Zero failures, zero Core skips.** Both
profile sub-results `success`. `HTTPRouteRedirectPortAndScheme` — the single FAIL that
held rev20 at Extended 9/1 — **flipped to PASS** this rev.

## rev20 → rev21: closing the last Extended FAIL took five layered fixes

rev20 classified the lone `HTTPRouteRedirectPortAndScheme` FAIL as a "Gateway
address-convergence timeout." That was the **outermost** symptom. Driving it to PASS
peeled back five distinct edge bugs, each one revealed only after the one above it was
fixed — a layered failure where every fix moved the timeout/error one stage deeper into
the request path:

1. **#380 — per-Gateway Service GC label value > 63 bytes.** The per-Gateway
   LoadBalancer Service NAME was already capped (#376), but the per-Gateway GC **label
   VALUE** was not, breaching the Kubernetes label-value 63-byte cap. The API server
   rejected the Service create → the Gateway never received `status.addresses` →
   `GatewayMustHaveAddress` timed out. Fix: truncate the label value too.
2. **#381 — shared mutable `hostCerts` map across Gateways.** Catch-all (no-hostname)
   TLS listeners all wrote the `""` key into one shared map → last-writer-wins → the
   wrong certificate was served for a given SNI → the HTTPS handshake was rejected (60s
   timeout). Fix: scope `hostCerts` per-Gateway.
3. **#382 — `effectiveHostnames` catch-all suppressed by sibling listeners (HTTPS
   404).** `effectiveHostnames` computed the gateway-level **union** of listener
   hostnames, so a catch-all listener's "admit all" was narrowed by sibling
   specific-hostname listeners on the same 443 port → the HTTPS vhost excluded
   `example.org` → **404** (instead of the expected 302). Fix: a catch-all listener +
   no-hostname route returns nil (true `*`).
4. **#383 — reconciler ran only on the leader after #379.** `gatewayapi.Reconciler`
   defaulted to `NeedLeaderElection=true`, so once leader election was enabled (#379) it
   ran on **only the leader pod**. But that reconciler feeds **each replica's** local
   Envoy via `SetEdgeGateways` — so the follower pod's Envoy had **no per-Gateway
   listeners**, yielding `connection refused` whenever kube-proxy/MetalLB routed to the
   follower. Fix: run the reconciler on every replica (`NeedLeaderElection=false`).
5. **#385 — `RedirectAction.port_redirect=0` leaks the internal bound port.** With no
   port/scheme override, `port_redirect=0` makes Envoy emit the listener's **bound
   (internal) port** (e.g. `18101`) instead of the **external** port (`8080`) the client
   actually hit → `Location: host:18101`. (The 80/443 cases passed because the default
   port is correctly omitted; the explicit port/scheme override cases passed because
   they set `port_redirect`.) Fix: set `port_redirect` to the listener's ExternalPort
   for the no-override, non-default-port case.

Plus two supporting PRs:

- **#384 (cleanup)** — removed the now-inert edge leader election. After #383 every edge
  runnable opts out of leader election, so the Lease gated nothing: edge.go + the Lease
  RBAC were removed and the chart bumped to **0.53.0**; the orphaned runtime Lease was
  deleted post-deploy.
- **#378 (earlier)** — retry-on-conflict on Gateway status writes, handling the
  active-active 409 churn that #383 restored by running the reconciler on every replica.

### Delta lineage of GATEWAY-HTTP PASS/FAIL

| Rev | PASS/FAIL | Core | Extended | Note |
|---|---|---|---|---|
| **rev18** | **39 / 4** | **32 / 1** | **7 / 3** | per-Gateway addressing + routing landed |
| **rev19** | **39 / 4** | **32 / 1** | **7 / 3** | +0; FIX B regressed (`/$1`), FIX A ineffective |
| **rev20** | **42 / 1** | **33 / 0** | **9 / 1** | Core COMPLETE; only `HTTPRouteRedirectPortAndScheme` (address-convergence) remained |
| **rev21** | **43 / 0** | **33 / 0** | **10 / 0** | **FULLY CONFORMANT.** #380→#385 chain closed the last Extended FAIL |

## MESH-HTTP — 4 PASS / 3 FAIL (proposal 022, unchanged)

```
MESH-HTTP core: Passed: 4  Failed: 3  (result: failure)
```

**FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight` — the same three stable
GAMMA capture-path fails as rev3/rev18/rev19/rev20/baseline. The HTTPRoute redirect /
header-modifier filters and weighted distribution apply on the edge route path but not
on the GAMMA transparent-capture path the MESH profile exercises. Nothing in the
#378–#385 edge chain touches the mesh data path → correctly **flat**. These are blocked
on **proposal 022 (arbitrary-service interception)** — Design status, untouched this
work. **MESH-HTTP confirmed unchanged.**

## Headline verdict

- **GATEWAY-HTTP IS FULLY CONFORMANT: 33/33 Core + 10/10 Extended = 43/43, zero
  failures.** Both profile sub-results `success`; `"Core tests succeeded. Extended
  tests succeeded."` This is the first fully-conformant GATEWAY-HTTP run.
- **The last Extended FAIL (`HTTPRouteRedirectPortAndScheme`) closed via a five-PR
  layered chain** (#380 → #381 → #382 → #383 → #385, plus #378/#384), each exposing the
  next defect: label-value cap → cross-Gateway cert map → catch-all hostname suppression
  → follower-replica reconciler → internal-port leak in the redirect Location.
- **MESH-HTTP unchanged at 4/3** — the three GAMMA capture-path fails remain, blocked on
  **proposal 022**. The edge work this rev did not touch the mesh data path.
- **No `api.palermo.dev`-shape regression:** 200 / 301 before and after the deploy.

### Correction to the rev20 record

The rev20 doc described the `HTTPRouteRedirectPortAndScheme` Gateway
(`same-namespace-with-http-listener-on-8080`) as "multi-listener (80/8080/443)." That
was **incorrect**: the conformance Gateway has a **single HTTP listener on port 8080**.
The rev20 failure was not a multi-listener addressing gap but the five layered edge bugs
documented above (culminating in the #385 internal-port leak). This rev21 doc corrects
the record.

## Reproduction

Same programmatic runner as rev2–rev20 (`conformance/aether_rev20_test.go`, a one-off in
a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

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

GATEWAY-HTTP took ~108.9s (`--- PASS: TestAetherRev20GatewayHTTP`). Admin was available
on the edge (`127.0.0.1:9901`) but not required for the score. Per-Gateway LoadBalancer
Services and `gateway-conformance-*` namespaces were cleaned up afterward.
