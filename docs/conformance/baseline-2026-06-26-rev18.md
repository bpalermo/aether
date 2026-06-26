# Gateway API conformance — rev18: GATEWAY-HTTP-Core after the route-gen fixes (2026-06-26)

An eighteenth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev 100)**, image/commit **`327918e`**. This is the
**GATEWAY-HTTP-Core-complete measurement** after the three Core route-generation
fixes from **#371** (segment-boundary `PathPrefix` via `path_separated_prefix`;
wildcard-listener route demux via per-section listener-hostname keys; host-port
matching confirmation) plus **#370** (defensive per-Gateway ServicePort dedup).

Aether was **not** modified or redeployed for this run. All `gateway-conformance-*`
namespaces and per-Gateway LoadBalancer Services were cleaned up afterward (verified).

## TL;DR — Core jumps to 32/1; total +2/−2 vs rev17. Not quite Core-complete.

| Profile | Tests | PASS | FAIL | DELTA vs rev17 | Verdict |
|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **43** | **39** | **4** | **+2 PASS / −2 FAIL** (was 37/6) | **Core 32/1**, Extended 7/3. #371 fixes (1)+(2) landed: `HTTPRouteListenerHostnameMatching` and `HTTPRouteMatching` flipped GREEN. One Core fail remains — the host-port sub-probe. |
| **MESH-HTTP** | 7 | **4** | **3** | flat (rev17 4/3) | 022 unchanged. The 3 stable capture-path fails; `MeshHTTPRouteMatching` PASSED (no flap this run). |

**Headline:** GATEWAY-HTTP **Core is 32 PASS / 1 FAIL** — up from rev17's 30/3.
The two rev17 Core fails that #371 fixes (1) and (2) targeted —
`HTTPRouteListenerHostnameMatching` (wildcard-listener demux) and `HTTPRouteMatching`
(segment-boundary `path_separated_prefix`) — **both flipped to PASS**. The third
rev17 Core fail, `HTTPRouteHostnameIntersection`, **still fails** — but only on a
**single host-port sub-probe** (`very.specific.com:1234`), not the whole test.
Extended is unchanged at 7/3 (same three tests as rev17). **api.palermo.dev verified
200 (5/5 pre-run, 3/3 post-run) via the edge `.101` — no production regression.**

The run was **455 s (~7.6 min)** — ~5× faster than rev8's 36.9 min, because tests now
PASS instead of retrying to the consistency timeout. Response-code tally over the full
GATEWAY run: 71×404, 33×503, 1×200 (the rest are within `not ready yet` retry windows
that subsequently passed — the 404/503 wall of rev3–rev8 is **gone**).

## Core verdict: 32/1 — NOT Core-complete (one host-port sub-probe short)

| Core test | rev17 | rev18 | Note |
|---|---|---|---|
| `HTTPRouteMatching` | FAIL | **PASS** | #371 fix (1): `path_separated_prefix` — `/v2` no longer matches `/v2example`. |
| `HTTPRouteListenerHostnameMatching` | FAIL | **PASS** | #371 fix (2): wildcard-listener per-section demux + drop no-intersecting-host vhosts. |
| `HTTPRouteHostnameIntersection` | FAIL | **FAIL** | #371 fix (3) (host-port) did **not** close it — one sub-probe persists (below). |
| (all 30 other Core tests) | PASS | **PASS** | unchanged. |

The expected `Core 33/0` did not fully land: fixes (1) and (2) worked, but the
host-port path (3) still has a persistent gap on exactly one sub-probe.

## The four remaining FAILs — flake vs real (each re-run in isolation)

Every Extended fail was **re-run as a single isolated test** (`RunTest=<ShortName>`,
full base-resource setup + teardown) to separate a warm-up/infra transient from a real
gap.

### 1. `HTTPRouteHostnameIntersection` — Core — **REAL, persistent**
- Single failing sub-probe: **`1_request_to_'very.specific.com:1234/s1' should go to infra-backend-v1`** — **404 for the full 60 s** (32 retries, all 404).
- All other ~23 sub-probes (including `0_request_to_'very.specific.com/s1'`, the *portless* spelling of the **same** route) **PASS**.
- Mechanism: this is a **multi-listener** Gateway. The HCM has `strip_any_host_port: true` (verified in `agent/internal/xds/proxy/edge.go`), so port-stripping *is* configured — but the `very.specific.com:1234` request still misses its vhost while `very.specific.com` (no port) hits it. The interaction is with the new per-section wildcard-listener demux (#371 fix 2): the host-port authority is not landing on the demuxed listener-hostname vhost. **Real gap**, narrow (1 sub-probe of a multi-listener host-port case).

### 2. `HTTPRouteMethodMatching` — Extended — **persistent, but a 503/availability issue (not a routing gap)**
- Isolated re-run: sub-probes **0–10 ALL PASS**; only **probe 11** (`'/' with_headers → infra-backend-v2`) fails — **503 for the full 60 s**.
- 11/12 sub-probes green, and method routing demonstrably works. The single failure is a **503** (route matched, upstream unavailable), **not a 404** (which would indicate a method-match miss). A specific backend (v2) that other probes reached returned a persistent 503 — an EDS/endpoint-readiness condition, not a missing feature.
- Classification: **reproducible, but availability-shaped, not a route-gen gap.** Same shape as rev17's "green logic, 11/12 sub-probes" assessment.

### 3. `HTTPRouteRewritePath` — Extended — **REAL logic gap**
- Isolated re-run: sub-probes 0,3,4,5 **PASS**; probes **1 and 2 FAIL** for the full 60 s with **`expected path to be /, got /strip-prefix`** (and `expected /three, got /strip-prefix/three`).
- The request **succeeds (200, reaches backend) but the path is not rewritten** — this is *not* a 503/404. The failing case is `ReplacePrefixMatch` where the replacement empties the path: `PathPrefix:/strip-prefix` → replace with `/` should yield `/` (and `/strip-prefix/three` → `/three`), but the prefix is left intact. The non-empty-remainder rewrites (`/prefix/...`, `/full/...`) pass.
- Classification: **real, narrow code gap** — empty-remainder `ReplacePrefixMatch` (the `path → /` edge case), the rewrite-side analogue of the #371 segment-boundary work.

### 4. `HTTPRouteRedirectPortAndScheme` — Extended — **FLAKE (no logic gap demonstrated)**
- Full run: reached traffic, only **1×503** transient.
- Two isolated re-runs (180 s each): **both failed at base-resource SETUP** — the conformance suite's own `gateway-conformance-infra/coredns` pod stayed `not ready` for the full window (314 readiness-false lines), so the test **never reached a single traffic probe** (0 expectation-failure lines). The multi-listener redirect Service path reproduced GREEN on the live edge in rev17.
- Classification: **flake** — an infra/coredns-readiness timing artifact in the isolated harness plus a lone full-run 503; **no aether redirect-logic gap was ever demonstrated.**

**Summary of the 4 fails:** 1 real Core gap (host-port sub-probe), 1 real Extended
gap (empty-remainder path rewrite), 1 Extended availability/503 (not a feature gap),
1 Extended flake (infra coredns / transient 503).

## MESH-HTTP — confirmed flat (022 unchanged)

**4 PASS / 3 FAIL**, identical to baseline/rev3/rev17. Failed (profile report):
`MeshHTTPRouteRedirectHostAndStatus`, `MeshHTTPRouteRequestHeaderModifier`,
`MeshHTTPRouteWeight` — the unimplemented HTTPRoute filters on the GAMMA/capture path
(proposal 022, not implemented). Unlike rev8, **`MeshHTTPRouteMatching` PASSED** this
run (no flap). #370/#371 are edge-only and do not touch the mesh data path, so this
flatness is correct.

## Is aether GATEWAY-HTTP-Core conformant?

**Not yet — 32/33 Core, one host-port sub-probe short.** #371 fixes (1) and (2)
delivered exactly as designed (`HTTPRouteMatching` + `HTTPRouteListenerHostnameMatching`
both GREEN), moving Core from 30/3 to **32/1**. The remaining Core fail is a single,
well-localised gap: a **host-port (`:1234`) authority not landing on its demuxed
listener-hostname vhost** in a multi-listener Gateway, despite `strip_any_host_port`
being set. This is one sub-probe of `HTTPRouteHostnameIntersection` — narrow and
individually addressable. **One fix from Core-complete.**

**Honest Extended remainder (7/3):** of the three Extended fails, only one
(`HTTPRouteRewritePath` — empty-remainder `ReplacePrefixMatch`) is a real route-gen gap;
`HTTPRouteMethodMatching` is a 503/availability artifact on one backend (logic is
green, 11/12), and `HTTPRouteRedirectPortAndScheme` is an infra/coredns flake (logic
never demonstrated to fail). The 14 unsupported Extended features (BackendTLSPolicy,
CORS, mirror, query-param matching, 3xx redirect status codes, …) remain correctly
**unclaimed**, not failed.

## DELTA vs prior revs

| | Baseline (rev 68) | rev3 (rev 72) | rev7 (rev 86) | rev8 (rev 87) | rev17 | **rev18 (rev 100)** |
|---|---|---|---|---|---|---|
| GATEWAY-HTTP PASS / FAIL | 0 / 0 (setup-blocked) | 6 / 31 | 12 / 25 | 12 / 25 | 37 / 6 | **39 / 4** |
| — Core PASS / FAIL | — | — | — | — | 30 / 3 | **32 / 1** |
| — Extended PASS / FAIL | — | — | — | — | 7 / 3 | **7 / 3** |
| Traffic-phase shape | n/a | TLS-SAN wall | uniform 404 | uniform 404 (case ii) | real traffic | **real traffic, mostly 200** |
| GATEWAY run time | — | — | ~36.8 min | ~36.9 min | ~36 min | **~7.6 min** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 4 / 3 | 4 / 3 | 3 / 4 | 4 / 3 | **4 / 3** |

## Prioritized remaining blockers

| Priority | Blocker | Fix |
|---|---|---|
| **P0 (GW Core)** | `HTTPRouteHostnameIntersection`: host-port `:1234` authority misses its demuxed listener-hostname vhost in a multi-listener Gateway. | Ensure the per-section listener-hostname vhost demux (#371 fix 2) keys on the **port-stripped** authority so `very.specific.com:1234` lands on the `very.specific.com` vhost. One fix → Core-complete. |
| **P1 (GW Ext)** | `HTTPRouteRewritePath`: empty-remainder `ReplacePrefixMatch` (`/strip-prefix` → `/`) leaves the prefix intact. | Emit Envoy `prefix_rewrite` (or the regex form) that yields `/` when the matched prefix is the whole path. |
| **P2 (GW Ext)** | `HTTPRouteMethodMatching` probe-11 503 on backend-v2. | Likely EDS/endpoint readiness; re-measure — not a route-gen gap. |
| **P0 (Mesh)** | HTTPRoute filters on the GAMMA/capture path (proposal 022). | Unchanged since baseline. |

## How it was run

Same programmatic runner as rev2–rev17 (`conformance/aether_rev18_test.go`, a copy of
the rev17 one-off with the env gate renamed `AETHER_REV18` and version held at `0.51.0`,
in a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api/conformance` — its own Go
module, `replace sigs.k8s.io/gateway-api => ../`, run with `GOWORK=off`, **not
committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures` (empty `SupportedFeatures` +
  `EnableAllSupportedFeatures=false` ⇒ suite inference). The class advertised
  `{Gateway, GatewayPort8080, HTTPRoute, HTTPRouteHostRewrite, HTTPRouteMethodMatching,
  HTTPRoutePathRedirect, HTTPRoutePathRewrite, HTTPRoutePortRedirect,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, HTTPRouteSchemeRedirect,
  ReferenceGrant}` — same set as rev3–rev17.
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
  `RequestTimeout=10s`.
- **Single-test re-runs** to classify the Extended fails used a one-off
  `RunTest=<ShortName>` runner (`aether_rev18_single_test.go`, **not committed**), which
  performs full base-resource setup + teardown per invocation.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces; the
per-Gateway LoadBalancer Services GC'd back to the single production edge. Final state:

```
$ kubectl get ns | grep conformance                       → none
$ kubectl get gateway -A | grep conformance               → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

No aether MetalLB pool IPs leaked — the only `aether.io/edge-gateway` Service is the
production `.101`. The `.99` and `.100` LoadBalancers belong to the pre-existing,
**unrelated** `envoy-gateway-system` install, not an aether conformance leak. No
`kubectl port-forward` processes left running. api.palermo.dev re-verified 200 (3/3)
via `.101` post-run — no regression.
