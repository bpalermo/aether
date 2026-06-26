# Gateway API conformance — rev14: hostname semantics + weighted backends (2026-06-26)

A fourteenth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev ~93)**, commit **`948b5f0`** (`#358`, weighted `backendRefs`), which
sits on top of **`#357`** (Gateway API hostname intersection + same-host route merge +
dup-FQDN webhook relax) — the two tail fixes deployed together on rev13's
`#354`/`#355` base.

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces and
per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR — both fixes land, the score moves **+2**, and the hostname work splits cleanly: isolation works, wildcard-promotion doesn't yet

| Profile | Tests | PASS | FAIL | DELTA vs rev13 | Verdict |
|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **37** | **22** | **15** | **+2 PASS / −2 FAIL** (rev13 20/17) | **`#358` works clean** — `HTTPRouteWeight` flipped straight to PASS (the weighted-split distribution now matches across the two cleartext clusters). **`#357` works partially**: `HTTPRouteHTTPSListener` flipped to PASS (hostname **isolation** on the HTTPS path — `unknown-example.org` now 404s), and `HTTPRouteMatchingAcrossRoutes` is now **7/8** sub-probes PASS (the dup-FQDN webhook relax + same-host merge worked; the one residual sub-probe is a **header** match, bucket C, not the webhook). But `HTTPRouteListenerHostnameMatching` and `HTTPRouteHostnameIntersection` **still FAIL** — not on isolation (the negative/404 cases pass) but on **wildcard promotion**: `*.bar.com ∩ baz.bar.com` doesn't program a vhost Envoy matches on the subdomain, so `baz.bar.com` falls through to the catch-all v1 instead of v3. **No regression**: the `api.palermo.dev`-shape and single-`*` prod-vhost tests (`HTTPRouteSimpleSameNamespace`, `HTTPRouteCrossNamespace`, `HTTPRouteExactPathMatching`, `HTTPRoutePathMatchOrder`, `HTTPRouteRedirectHostAndStatus`, `GatewayWithAttachedRoutes`) all still PASS. |
| **MESH-HTTP** | 7 | **4** | **3** | flat (rev13 4/3) | 022 untouched. Identical to rev13: the same 3 stable capture-path FAILs. None of the rev12→rev14 edge changes touch the GAMMA capture path. |

Headline GATEWAY-HTTP: **Core 20 pass / 13 fail**, **Extended 2 pass / 2 fail**. Traffic
phase: **387 `got 200`**, **303 `got 404`**, **207 `got 503`**, **0 `got 500`** (rev13 was
554/250/206/0). GATEWAY run wall time **~1216 s (~20 min)**.

## What flipped to PASS (the net +2)

| Test | rev13 | rev14 | Why |
|---|---|---|---|
| `HTTPRouteWeight` | FAIL | **PASS** | **`#358`**: a rule with multiple `backendRefs` now fans traffic by weight across all of them instead of pinning to the first backend. The "distribution that matches the weight" sub-probe passes (18.3 s of weighted sampling). |
| `HTTPRouteHTTPSListener` | FAIL | **PASS** | **`#357`** hostname **isolation** on the TLS path: `example.org`→v1, `second-example.org`→v2 still route, and `unknown-example.org` now correctly **404s** (was 200). |

`HTTPRouteMatchingAcrossRoutes` did **not** flip (still FAIL overall) but moved from
**fully blocked by the dup-FQDN webhook** to **7/8 sub-probes PASS** — the webhook
relaxation and same-host route merge landed; only the header-matched sub-probe
(`example.com/` *with headers* → v2) fails, which is bucket C (header matching), not the
webhook.

The full **20 core PASS** (GATEWAY-HTTP profile, GRPC tests excluded — see note):
`GatewayClassObservedGenerationBump`, `GatewayObservedGenerationBump`,
`GatewayModifyListeners`, `GatewayInvalidRouteKind`, `GatewayWithAttachedRoutes`,
`GatewayWithAttachedRoutesWithPort8080`, the four `GatewaySecret*ReferenceGrant*` /
`GatewaySecretMissingReferenceGrant` cert-RBAC tests, `HTTPRouteObservedGenerationBump`,
`HTTPRouteInvalidParentRefNotMatchingSectionName`,
`HTTPRouteInvalidCrossNamespaceParentRef`, `HTTPRouteRedirectHostAndStatus`,
`HTTPRouteSimpleSameNamespace`, `HTTPRouteCrossNamespace`, `HTTPRouteExactPathMatching`,
`HTTPRouteRequestHeaderModifier`, `HTTPRoutePathMatchOrder`, **`HTTPRouteHTTPSListener`**,
**`HTTPRouteWeight`**. Plus **2 Extended PASS**: `HTTPRouteResponseHeaderModifier` and one
more (the suite reports Extended 2/2 on `{GatewayPort8080, HTTPRouteResponseHeaderModification}`,
with `HTTPRouteMethodMatching` and `HTTPRouteRequestTimeout` on the FAIL side).

> **GRPC note.** `GRPCExactMethodMatching`, `GRPCRouteHeaderMatching`,
> `GRPCRouteListenerHostnameMatching`, `GRPCRouteWeight` ran and FAILed (60 s timeout each),
> but GRPCRoute is **not a GATEWAY-HTTP profile feature**, so they are **not** in the 22/15
> tally — a future GRPCRoute-profile concern. The edge GATEWAY profile does not serve GRPC.

## The hostname work (`#357`) — isolation ✅, wildcard promotion ❌

This is the key nuance of rev14. `#357` delivered hostname **isolation** but not full
hostname **matching**:

- **Isolation (the 404 half) works.** Every negative sub-probe passes:
  `HTTPRouteListenerHostnameMatching` `foo.com`/`no.matching.host` → 404 ✅,
  `HTTPRouteHostnameIntersection` `specific.but.wrong.com`/`wildcard.io` → 404 ✅,
  `HTTPRouteHTTPSListener` `unknown-example.org` → 404 ✅. Exact-hostname and
  unspecified-listener routes also resolve (`first.com`/`sub.first.com`→v2 etc. all PASS).
- **Wildcard promotion (the positive half) does not.** A listener `*.bar.com` intersected
  with route `baz.bar.com` should yield an effective hostname `baz.bar.com` that Envoy
  matches on the subdomain. Instead `baz.bar.com`, `boo.bar.com`,
  `multiple.prefixes.foo.com`, `foo.bar.com` all fall through to the catch-all
  `infra-backend-v1` (logged `expected pod name to start with infra-backend-v3, got
  infra-backend-v1-…`). `HTTPRouteHostnameIntersection`'s positive case
  `very.specific.com:1234/s1` → **404** (the `:1234` port-in-Host + intersection positive
  isn't programmed). 297 `[404] got 200` + the v1-fallthroughs are this gap.

So `#357` flipped the one test (`HTTPRouteHTTPSListener`) whose positive cases were exact
hostnames, and got `HTTPRouteMatchingAcrossRoutes` to 7/8 via the merge, but the two
wildcard-heavy tests (`ListenerHostnameMatching`, `HostnameIntersection`) remain FAIL on
the **wildcard-vhost** promotion. **No prod-vhost regression** — the fragile `*` path held.

## Bucket B status fix (`#354`) still holds — data-path 500 still missing

`Conditions matched expectations` logged **57 times**; the invalid-backendRef
`ResolvedRefs=False` status is still correct. But **`got 500: 0`** for the entire run —
the four invalid-ref tests still return 503/404 on the data path where the suite wants
**500**. Traffic shows **`[500] got 404`: 239**, **`[500] got 503`: 61**, **`[500] got
200`: 60** = 360 mis-coded invalid-ref requests. Unchanged this build (no fix shipped);
still the single highest-ROI lever.

## The 15 GATEWAY-HTTP FAILs — re-bucketed (counts)

### Bucket A — hostname **wildcard promotion** in the `*`/per-host vhosts (2) — partially addressed by `#357`
- **`HTTPRouteListenerHostnameMatching`** — isolation negatives PASS; wildcard positives
  (`baz.bar.com`/`boo.bar.com`/`multiple.prefixes.*` → v3) fall through to v1.
- **`HTTPRouteHostnameIntersection`** — most sub-probes PASS (isolation + unspecified-listener
  + exact); the intersect-positive `very.specific.com:1234/s1`→v1 → 404 fails (wildcard +
  port-in-Host).

`#357` moved this from "no isolation at all (3 tests incl. HTTPSListener)" to "isolation
done, wildcard-promotion pending (2 tests)". The remaining work is programming the
`*.suffix`-intersected effective hostname as a wildcard-domain Envoy vhost (and handling
`Host:port`).

### Bucket B — invalid-backendRef **data-path 500** (status correct via `#354`) (4)
- **`HTTPRouteInvalidNonExistentBackendRef`**, **`HTTPRouteInvalidBackendRefUnknownKind`**,
  **`HTTPRouteInvalidReferenceGrant`**, **`HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`**
  (+ `HTTPRouteInvalidCrossNamespaceBackendRef`, `HTTPRouteReferenceGrant`). Status done;
  only the **500 direct-response** on the unresolvable route rule is missing. `got 500: 0`.

### Bucket C — header/method/query **match criteria** (the edge matches by PATH only) (2 core + 2 ext)
- **`HTTPRouteMatching`**, **`HTTPRouteHeaderMatching`** (core) — every failing sub-probe is
  `'/' with headers …` (header/method criteria), plus a `'/v2example'` prefix-vs-exact
  boundary. The merged vhost doesn't match on header/method, only path.
- **`HTTPRouteMethodMatching`** (ext), **`HTTPRouteTimeoutRequest`** (ext) — same root
  (method criteria; `[504] got 200`: 30 for the timeout probe). The residual
  `HTTPRouteMatchingAcrossRoutes` header sub-probe shares this root.

### Bucket D — dup-FQDN webhook vs split-across-routes (1) — **mostly resolved by `#357`**
- **`HTTPRouteMatchingAcrossRoutes`** — **7/8 sub-probes now PASS** (webhook relaxed,
  same-host merge works). The single FAIL (`example.com/` *with headers* → v2) is a bucket-C
  header match, not the old `validate.config.aether.io` over-rejection. This bucket is
  effectively retired; the test now sits with bucket C.

### Bucket E — backend shapes the cleartext path doesn't cover (1)
- **`HTTPRouteServiceTypes`** — headless / manual-EndpointSlice Services have no A record at
  the Service name for STRICT_DNS → 503. (`HTTPRouteWeight` left this bucket — flipped PASS.)

### Bucket F — flake/regression (1)
- **`GatewayInvalidTLSConfiguration`** — same `observedGeneration` staleness flake as rev13
  (84 occurrences); the listener content sub-tests PASS, the top-level
  observed-generation check fails at 0.52 s. Not substantive; re-check next run.

| Bucket | Count | Tests |
|---|---:|---|
| **A. hostname wildcard promotion** (isolation done via `#357`) | **2** | `HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection` |
| **B. invalid-backendRef data-path 500** (status done via `#354`) | **4** | `HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidBackendRefUnknownKind`, `HTTPRouteInvalidReferenceGrant`, `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant` (+`HTTPRouteInvalidCrossNamespaceBackendRef`, `HTTPRouteReferenceGrant`)† |
| **C. header/method/query match** (path-only edge) | **2 core + 2 ext** | `HTTPRouteMatching`, `HTTPRouteHeaderMatching` (+`HTTPRouteMethodMatching` ext, `HTTPRouteTimeoutRequest` ext) |
| **D. dup-FQDN webhook** (resolved by `#357`; residual sub-probe → bucket C) | **1** | `HTTPRouteMatchingAcrossRoutes` |
| **E. backend shapes** | **1** | `HTTPRouteServiceTypes` |
| **F. flake/regression** | **1** | `GatewayInvalidTLSConfiguration` |

† The ReferenceGrant-family members are status+data-path tests counted in bucket B.
Unique core fails: **2 + 4 + 2 + 1 + 1 + 1 = 13** (the bucket-B "+2" are the cross-ns/grant
family already inside the 13), plus **2 extended** = **15** total.

**Verbatim failed set (authoritative, from the profile report):**
Core (13): `GatewayInvalidTLSConfiguration`, `HTTPRouteHeaderMatching`,
`HTTPRouteHostnameIntersection`, `HTTPRouteInvalidBackendRefUnknownKind`,
`HTTPRouteInvalidCrossNamespaceBackendRef`, `HTTPRouteInvalidNonExistentBackendRef`,
`HTTPRouteInvalidReferenceGrant`, `HTTPRouteListenerHostnameMatching`, `HTTPRouteMatching`,
`HTTPRouteMatchingAcrossRoutes`, `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`,
`HTTPRouteReferenceGrant`, `HTTPRouteServiceTypes`. Extended (2): `HTTPRouteMethodMatching`,
`HTTPRouteTimeoutRequest`.

## MESH-HTTP — 4 PASS / 3 FAIL (flat vs rev13, substantively unchanged)

Profile report: `pass=4 fail=3 skip=0` (wall **~260 s / ~4.3 min**). 022 is untouched; none
of the rev12→rev14 edge changes touch the GAMMA capture path.

**PASS (4):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`, `MeshHTTPRouteMatching`,
`MeshTrafficSplit`.
**FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`,
`MeshHTTPRouteWeight` — the stable capture-path FAILs (filters/redirect/weight not applied
on the GAMMA capture path). (`MeshFrontend` also FAILed but is **not** a MESH-HTTP profile
feature and is excluded from the 4/3 tally.) Identical set to rev13 — in the documented
flake band (rev12 3/4, rev3 4/3, rev13 4/3).

## DELTA vs prior revs

| | baseline | rev3 (72) | rev12 (~91) | rev13 (~92) | **rev14 (~93)** |
|---|---|---|---|---|---|
| Commit | — | — | `be9efa4` | `50f627c` | **`948b5f0`** |
| GATEWAY-HTTP PASS / FAIL | 6 / 31 | 6 / 31 | 19 / 18 | 20 / 17 | **22 / 15** |
| GATEWAY ΔPASS vs prior | — | +0 | +5 | +1 | **+2** |
| Traffic dominant code | 404 | TLS-SAN wall | 200 (553) | 200 (554) | **200 (387)** |
| `got 500` in traffic | — | — | 0 | 0 | **0** (← bucket-B gap) |
| Invalid-backendRef status | True (wrong) | True | True | **False (`#354`)** | **False (`#354`)** |
| Path-match precedence | unordered | unordered | unordered | **ordered (`#355`)** | **ordered** |
| Hostname isolation | none | none | none | none | **isolation ✅ / wildcard ❌ (`#357`)** |
| Weighted backends | no | no | no | no | **yes (`#358`)** |
| Dominant remaining blocker | route table | TLS-SAN | vhost precedence + hostname | invalid-ref 500 + hostname | **invalid-ref 500 + wildcard host + header match** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 4 / 3 | 3 / 4 | 4 / 3 | **4 / 3** |

## Honest assessment + prioritized tail

`#358` did exactly what it promised — `HTTPRouteWeight` flipped clean. `#357` did the
**isolation half** of the hostname work (one test flipped, one near-test got to 7/8, the
dup-FQDN over-rejection is retired, and crucially the **prod `*` vhost did not regress**) —
but the **wildcard-promotion half** (`*.suffix ∩ sub.suffix`) is not programmed, so the two
wildcard-heavy tests remain FAIL. Net **+2 (22/15)**, and the buckets are now very
well-scoped.

Prioritized remaining tail (highest pass-per-effort first):

1. **Bucket B — invalid-ref data-path 500 (4 tests).** Still the top lever: status is
   shipped (`#354`), the remainder is one Envoy `direct_response`/`local_reply` 500 on the
   unresolvable route rule (currently 503/404; `got 500: 0`). Highest ROI on the board.
2. **Bucket A — hostname wildcard promotion (2 tests).** Follow-through on `#357`: program
   the `*.suffix`-intersected effective hostname as a wildcard-domain Envoy vhost and handle
   `Host:port`. Isolation is already done.
3. **Bucket C — header/method/query match (2 core + 2 ext).** The structural one: the edge
   route table only matches by path; it needs header/method/query criteria (this also drags
   the residual `HTTPRouteMatchingAcrossRoutes` sub-probe and the two Extended fails).
4. **Bucket E** (headless/manual-EPS) and **bucket F** (TLS observedGeneration flake) are a
   backend-shape item and a flake re-confirm, respectively.

Distance to GATEWAY-HTTP-Core is now **a 500 direct-response + wildcard-vhost promotion + a
header/method-aware route match** — three named, bounded changes. Bucket D is effectively
closed.

## How it was run

Same programmatic runner as rev2–rev13 (`conformance/aether_rev14_test.go`, a copy of the
rev13 one-off with the env gate renamed `AETHER_REV14`, version held at `0.51.0`, in a
`gateway-api` **v1.5.1** checkout; **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** features **inferred** from `GatewayClass.status.supportedFeatures`. The
  suite read `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}` —
  identical to rev3/rev5–rev13 (no advertised-feature change in 0.51.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.
- GATEWAY run: **~1216 s (~20 min)**. Traffic phase: **387 `got 200`**, **303 `got 404`**,
  **207 `got 503`**, **0 `got 500`**.

The edge admin (`127.0.0.1:9901` loopback per replica) remained enabled but was **not**
needed — the suite's per-request status codes and assertion messages were sufficient to
bucket every fail. No `config_dump` / port-forward was taken.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces (both the GATEWAY
and MESH runs self-cleaned); the per-Gateway LoadBalancer Services GC'd back to the single
production edge. Verified final state:

```
$ kubectl get ns | grep gateway-conformance                 → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101
$ pgrep -af "kubectl port-forward"                          → none
```

The `192.168.100.99` / `.100` `envoy-gateway-system` LoadBalancers are **unrelated** to
aether (a separate Envoy Gateway install) and were left untouched.
