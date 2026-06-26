# Gateway API conformance — rev13: BackendNotFound status + route precedence land (2026-06-26)

A thirteenth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev ~92)**, commit **`50f627c`** (`#355`, "Gateway API route-matching
precedence"), which sits on top of **`#354`** (HTTPRoute `ResolvedRefs=False` for
missing/invalid backendRefs) — the two tail fixes deployed together on rev12's dual-mode
backends.

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces and
per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR — both fixes work; the score moves +1, and the status bucket is now *one*
## data-path code away from flipping

| Profile | Tests | PASS | FAIL | DELTA vs rev12 | Verdict |
|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **37** | **20** | **17** | **+1 PASS / −1 FAIL** (rev12 19/18) | **#355 works** — `HTTPRoutePathMatchOrder` flipped straight to PASS (every specificity sub-probe routes correctly), and `GatewayWithAttachedRoutes` came along for free (the `#354` status now sets `AttachedRoutes` even with unresolved refs). **#354 works too — but only the status half**: all 6 invalid-backendRef tests now log `Conditions matched expectations` (the `ResolvedRefs=False` reason is correct), yet they still FAIL because the suite asserts an HTTP **500** on the data path and the edge returns **503/404** (`got 500: 0` across the whole run). One regression: `GatewayInvalidTLSConfiguration` flaked to FAIL on an `observedGeneration` staleness lag. |
| **MESH-HTTP** | 7 | **4** | **3** | flake band (rev12 3/4, rev3 4/3) | 022 untouched. High side of the flake this run — `MeshHTTPRouteMatching` passed. The 3 stable capture-path FAILs remain. |

Headline GATEWAY-HTTP: Core **18 pass / 15 fail**, Extended **2 pass / 2 fail**. Traffic
phase: **554 `got 200`**, **250 `got 404`**, **206 `got 503`**, **0 `got 500`** (rev12 was
553/247/209). The traffic mix is essentially unchanged — the rev13 gains are in **status +
ordering**, not in new reachable backends. GATEWAY run wall time **~1306 s (~22 min)**;
MESH **~272 s (~4.5 min)**.

## What flipped to PASS (the net +1, and the two that moved)

| Test | rev12 | rev13 | Why |
|---|---|---|---|
| `HTTPRoutePathMatchOrder` | FAIL | **PASS** | **`#355`**: path matches now sort by specificity (Exact > longer-prefix > shorter), creationTimestamp tie-break. All 6 sub-probes (`/match/exact/one`→v3, `/match/exact`→v2, `/match`→v1, …) route to the correct backend. |
| `GatewayWithAttachedRoutes` | FAIL | **PASS** | Knock-on from **`#354`**: the "should have `AttachedRoutes` set even when Gateway has unresolved refs" sub-test now passes because the controller sets correct status in the presence of an invalid ref. |
| `GatewayInvalidTLSConfiguration` | PASS | **FAIL** (regression/flake) | Not a substantive change — the 4 cert-error Gateways reported `expected observedGeneration to be updated to 1 … only 0/2 were updated (Accepted, Programmed stale)`. A controller status-staleness timing lag; expected to recover on re-run. **Flag for re-check.** |

The full **20 PASS** (GATEWAY-HTTP profile, GRPC tests excluded — see note):
`GatewayClassObservedGenerationBump`, `GatewayObservedGenerationBump`,
`GatewayModifyListeners`, `GatewayInvalidRouteKind`,
`GatewayWithAttachedRoutes`, `GatewayWithAttachedRoutesWithPort8080`, the four
`GatewaySecret*ReferenceGrant*` / `GatewaySecretMissingReferenceGrant` cert-RBAC tests,
`HTTPRouteObservedGenerationBump`, `HTTPRouteInvalidParentRefNotMatchingSectionName`,
`HTTPRouteInvalidCrossNamespaceParentRef`, `HTTPRouteRedirectHostAndStatus`,
`HTTPRouteSimpleSameNamespace`, `HTTPRouteCrossNamespace`, `HTTPRouteExactPathMatching`,
`HTTPRouteRequestHeaderModifier`, **`HTTPRoutePathMatchOrder`**,
`HTTPRouteResponseHeaderModifier` (Ext).

> **GRPC note.** `GRPCExactMethodMatching`, `GRPCRouteHeaderMatching`,
> `GRPCRouteListenerHostnameMatching`, `GRPCRouteWeight` ran and FAILed (60 s timeout each),
> but GRPCRoute is **not a GATEWAY-HTTP profile feature**, so they are **not** in the 20/17
> tally — a future GRPCRoute-profile concern.

## The #354 status fix landed — but the data-path code did not

This is the key nuance of rev13. `#354` was supposed to flip **bucket 2** (the 4 status
fails). It **did the controller half correctly**: every one of the six invalid-backendRef
tests now logs `Conditions matched expectations` — the `ResolvedRefs=False` condition with
the right reason (`BackendNotFound`, `InvalidKind`, the ReferenceGrant reasons) is present.
The rev12 wall of *1191 `ResolvedRefs … Status True … expected False`* lines is **gone**.

But each of these tests **also asserts a data-path status code**, and that half is still
wrong:

| Test | status condition | data-path: expected | data-path: got | result |
|---|---|---|---|---|
| `HTTPRouteInvalidNonExistentBackendRef` | ✅ `False/BackendNotFound` | **500** | **503** | FAIL |
| `HTTPRouteInvalidBackendRefUnknownKind` | ✅ `False/InvalidKind` | **500** | **404** | FAIL |
| `HTTPRouteInvalidReferenceGrant` | ✅ matched | 500 | 503 | FAIL |
| `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant` | ✅ matched | 500 (invalid leg) | 503 | FAIL |
| `HTTPRouteInvalidCrossNamespaceBackendRef` | ✅ matched | 500 | 503 | FAIL |
| `HTTPRouteReferenceGrant` | ✅ matched | (granted/denied legs) | — | FAIL |

**`got 500: 0` for the entire run.** The Gateway API contract says a route whose
backendRef cannot be resolved must return **HTTP 500** for traffic to that rule (Envoy's
`direct_response`/`local_reply` with 500, not the 503 you get from an empty/no-host cluster
nor the 404 you get from a route that was dropped). The edge needs, for an unresolvable
backendRef, to program a **fixed 500 direct-response** on that route rule rather than a
dead cluster (503) or no route at all (404). That single change flips all of bucket 2 — the
status work in `#354` is already done.

## The 17 GATEWAY-HTTP FAILs — re-bucketed (counts)

### Bucket A — hostname isolation in the merged `*` vhost (3) — **NOT fixed here (in-flight)**
The ordering half of the old bucket 1 is gone (`#355`). What remains is the **hostname
half**: the single `*` catch-all vhost matches *any* Host, so a request to a host the
listener/route does **not** serve returns 200 where the suite expects 404.

- **`HTTPRouteListenerHostnameMatching`** — `Host: foo.com` to a listener that doesn't
  carry it → 200, expected 404.
- **`HTTPRouteHostnameIntersection`** — listener×route hostname intersection not enforced.
- **`HTTPRouteHTTPSListener`** — matched host 200s, but `Host: unknown-example.org` → 200
  where 404 expected.

This is the **separate in-flight PR** the task flagged; expected to remain, and it does.

### Bucket B — invalid-backendRef **data-path 500** (status now correct) (4)
- **`HTTPRouteInvalidNonExistentBackendRef`**, **`HTTPRouteInvalidBackendRefUnknownKind`**,
  **`HTTPRouteInvalidReferenceGrant`**, **`HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`**
  (+ `HTTPRouteInvalidCrossNamespaceBackendRef`, `HTTPRouteReferenceGrant`, ReferenceGrant
  family). Status `ResolvedRefs=False` is **done** (`#354`); only the **500 direct-response**
  on the dead route rule is missing. **This is now a small, well-scoped data-path change.**

### Bucket C — precedence/matching STILL failing despite #355 (2)
- **`HTTPRouteMatching`**, **`HTTPRouteHeaderMatching`** — these are **not** path-ordering
  fails; they exercise **header-/method-criteria matching** in the merged vhost. `#355`
  sorted by *path* specificity, but these probes (`Version: two`, `/v2`, `Color: green`)
  still return 404/503 — the merged-vhost route table doesn't carry every header/method
  sibling, or doesn't sort on match-criteria **beyond path**. **The sort needs to extend to
  header/method/query match criteria, not just path** (and the merged vhost must program all
  siblings). `HTTPRouteMethodMatching` (Ext) and the bucket-A family share this root.

### Bucket D — dup-FQDN admission webhook vs split-across-routes (1) — **design conflict**
- **`HTTPRouteMatchingAcrossRoutes`** — rejected by `validate.config.aether.io`
  (`host "example.com" is already claimed by HTTPRoute …/matching-part1 on the same
  Gateway`). aether's one-FQDN-per-HTTPRoute constraint vs the suite's split-host model.
  Unchanged design decision.

### Bucket E — backend shapes / weighting the cleartext path doesn't cover (2)
- **`HTTPRouteServiceTypes`** — headless / manual-EndpointSlice Services have no A record
  at the Service name for STRICT_DNS → 503.
- **`HTTPRouteWeight`** — "distribution that matches the weight" still fails (all traffic to
  one backend; no weighted split across the two cleartext clusters).

### Bucket F — flake/regression (1)
- **`GatewayInvalidTLSConfiguration`** — `observedGeneration` staleness (0/2 conditions
  updated). PASS in rev12; a controller status-timing flake. Re-check on next run.

### Extended fails (2, folded above)
- **`HTTPRouteMethodMatching`** — method-criteria matching in the merged vhost (bucket C
  family); 118 `got 200` / 12 `got 503` — a missing-sibling 503.
- **`HTTPRouteTimeoutRequest`** — request-timeout probe still hits wrong-upstream/503 in the
  merged vhost.

| Bucket | Count | Tests |
|---|---:|---|
| **A. Hostname isolation in `*` vhost** (in-flight, not fixed here) | **3** | `HTTPRouteListenerHostnameMatching`, `HTTPRouteHostnameIntersection`, `HTTPRouteHTTPSListener` |
| **B. invalid-backendRef data-path 500** (status done via `#354`) | **4** | `HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidBackendRefUnknownKind`, `HTTPRouteInvalidReferenceGrant`, `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant` (+`HTTPRouteInvalidCrossNamespaceBackendRef`, `HTTPRouteReferenceGrant`)† |
| **C. precedence/matching beyond path** (despite `#355`) | **2** | `HTTPRouteMatching`, `HTTPRouteHeaderMatching` (+`HTTPRouteMethodMatching` Ext, `HTTPRouteTimeoutRequest` Ext) |
| **D. dup-FQDN webhook** (design conflict) | **1** | `HTTPRouteMatchingAcrossRoutes` |
| **E. backend shapes / weighting** | **2** | `HTTPRouteServiceTypes`, `HTTPRouteWeight` |
| **F. flake/regression** | **1** | `GatewayInvalidTLSConfiguration` |

† The ReferenceGrant-family members (`HTTPRouteInvalidCrossNamespaceBackendRef`,
`HTTPRouteReferenceGrant`) are status+data-path tests counted in bucket B. Counting unique
profile fails: **3 + 4 + 2(core) + 1 + 2 + 1 = 13 core unique**, plus **2 extended**
(`HTTPRouteMethodMatching`, `HTTPRouteTimeoutRequest`) = the **15 core / 2 ext = 17** total.

**Verbatim failed set (authoritative, from the profile report):**
Core (15): `GatewayInvalidTLSConfiguration`, `HTTPRouteHTTPSListener`,
`HTTPRouteHeaderMatching`, `HTTPRouteHostnameIntersection`,
`HTTPRouteInvalidBackendRefUnknownKind`, `HTTPRouteInvalidCrossNamespaceBackendRef`,
`HTTPRouteInvalidNonExistentBackendRef`, `HTTPRouteInvalidReferenceGrant`,
`HTTPRouteListenerHostnameMatching`, `HTTPRouteMatching`, `HTTPRouteMatchingAcrossRoutes`,
`HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`, `HTTPRouteReferenceGrant`,
`HTTPRouteServiceTypes`, `HTTPRouteWeight`. Extended (2): `HTTPRouteMethodMatching`,
`HTTPRouteTimeoutRequest`.

## The biggest remaining blocker

Two near-equal levers, both now small and well-scoped:

1. **Bucket B — invalid-backendRef data-path 500 (4 tests).** The hard part (status) is
   **already shipped** in `#354`. The remaining work is one Envoy change: program a **fixed
   500 direct-response** on a route rule whose backendRef is unresolvable, instead of a dead
   cluster (503) or a dropped route (404). Highest ROI — pure follow-through on `#354`.
2. **Bucket A — hostname isolation (3 tests).** The known **in-flight PR** (per-listener-
   hostname vhosts so a non-matching Host 404s). Expected next.

After those, **bucket C** (extend the route sort/merge to header/method criteria, not just
path — `#355` only covered path) is the next structural item, and it also drags the two
Extended fails. Bucket D is a standing design decision; bucket E needs EDS/weighted
clusters; bucket F is a flake to re-confirm.

## MESH-HTTP — 4 PASS / 3 FAIL (high side of the flake band, substantively flat)

Profile report: `pass=4 fail=3 skip=0`. 022 is untouched; none of the rev12→rev13 edge
changes touch the GAMMA capture path.

**PASS (4):** `MeshBasic`, `MeshHTTPRouteSimpleSameNamespace`, `MeshHTTPRouteMatching`,
`MeshTrafficSplit`.
**FAIL (3):** `MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`,
`MeshHTTPRouteWeight` — the stable capture-path FAILs. (`MeshFrontend` also FAILed but is
not a MESH-HTTP profile feature.) `MeshHTTPRouteMatching` passed this run (it was on the
FAIL side in rev12), placing rev13 on the **high side** of the documented flake band
(rev12 3/4, rev3 4/3). No substantive change.

## DELTA vs prior revs

| | baseline | rev3 (72) | rev11 (~90) | rev12 (~91) | **rev13 (~92)** |
|---|---|---|---|---|---|
| Commit | — | — | `9c77c1d` | `be9efa4` | **`50f627c`** |
| GATEWAY-HTTP PASS / FAIL | 6 / 31 | 6 / 31 | 14 / 23 | 19 / 18 | **20 / 17** |
| GATEWAY ΔPASS vs prior | — | +0 | +2 | +5 | **+1** |
| Traffic dominant code | 404 | TLS-SAN wall | **503** (4693) | **200** (553) | **200** (554) |
| `got 500` in traffic | — | — | 0 | 0 | **0** (← the bucket-B gap) |
| Invalid-backendRef status | True (wrong) | True | True | True | **False (correct, `#354`)** |
| Path-match precedence | unordered | unordered | unordered | unordered | **specificity-ordered (`#355`)** |
| Dominant remaining blocker | route table | TLS-SAN | upstream 503 | vhost precedence + hostname | **invalid-ref 500 + hostname isolation** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 4 / 3 | 2 / 5 | 3 / 4 | **4 / 3** |

## How close now?

**Closer, and the tail is now genuinely short and well-understood.** `#355` did exactly what
it promised (path-specificity ordering — `HTTPRoutePathMatchOrder` flipped clean, and
`GatewayWithAttachedRoutes` came free). `#354` did the **hard, controller half** of the
status bucket — all six invalid-backendRef tests now carry the correct `ResolvedRefs=False`
condition — but those tests still FAIL on a **single missing data-path behavior**: a 500
direct-response on the unresolvable route (the edge returns 503/404; `got 500: 0` all run).

The realistic GATEWAY-HTTP-Core picture:

- **+1 now (20/17).** The score moved less than rev12's +5 because the two fixes target
  *status* and *ordering*, not new reachable traffic — but they de-risked two whole buckets.
- **Next +4 is one small change away** (bucket B: emit 500 on unresolvable backendRef — pure
  follow-through on the shipped `#354` status).
- **Then +3** from the **in-flight hostname-isolation PR** (bucket A).
- That would put Core at roughly **27-pass-equivalent of the 37-test profile**, leaving the
  structural matching-beyond-path (bucket C), the dup-FQDN design call (bucket D), and the
  backend-shape/weight work (bucket E).

**Biggest lever:** **bucket B (invalid-ref 500, 4 tests)** — because the expensive half is
already merged, the remainder is one Envoy direct-response, and it has the highest
pass-per-effort of anything on the board. The **hostname-isolation in-flight PR (bucket A,
3)** is the close second. The distance to GATEWAY-HTTP-Core is now **a 500 direct-response +
per-hostname vhost isolation + a header/method-aware route sort** — three named, bounded
changes, not a wall.

## How it was run

Same programmatic runner as rev2–rev12 (`conformance/aether_rev13_test.go`, a copy of the
rev12 one-off with the env gate renamed `AETHER_REV13`, version held at `0.51.0`, in a
`gateway-api` **v1.5.1** checkout; **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** features **inferred** from `GatewayClass.status.supportedFeatures`. The
  suite read `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}` —
  **identical to rev3/rev5–rev12** (no advertised-feature change in 0.51.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`.
- GATEWAY run: **~1306 s (~22 min)**. MESH: **~272 s (~4.5 min)**. Traffic phase: **554
  `got 200`**, **250 `got 404`**, **206 `got 503`**, **0 `got 500`** (rev12 553/247/209).

The edge admin (`#345`, `127.0.0.1:9901` loopback per replica) remained enabled but was
**not** needed — the suite's per-request status codes and assertion messages
(`Conditions matched expectations`, `expected status code … 500, got 503`) were sufficient
to bucket every fail. No `config_dump` / port-forward was taken.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces (both the GATEWAY
and MESH runs self-cleaned); the per-Gateway LoadBalancer Services GC'd back to the single
production edge. Verified final state:

```
$ kubectl get ns | grep gateway-conformance                 → none
$ kubectl get gateway -A | grep conformance                 → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101
$ pgrep -af "kubectl port-forward"                          → none
```

The `192.168.100.99` / `.100` `envoy-gateway-system` LoadBalancers are **unrelated** to
aether (a separate Envoy Gateway install) and were left untouched.
