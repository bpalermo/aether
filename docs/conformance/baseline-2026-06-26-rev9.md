# Gateway API conformance — rev9: per-external-port allocation fixed the Service, not the 404 (2026-06-26)

A ninth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev 88)**, image/commit **`ea18fe3`**. The hypothesis going in: rev8's
root cause — per-*listener* port allocation produced duplicate `port-80` Service ports,
which the API server rejected (`Duplicate value: "port-80"`, 171×), which starved
multi-listener Gateways of an address — was fixed by **#348** (allocate **one internal
port per external port**, listeners on the same port share one edge listener via
hostname/SNI demux). The expectation was a **large GATEWAY-HTTP jump**.

**It did not jump.** GATEWAY-HTTP is **flat at 12 PASS / 25 FAIL** — bit-identical to
rev6/rev7/rev8. #348 is a real fix (the duplicate-port Service failures are **gone**,
multi-listener Gateways now get distinct addresses `.50`–`.53`), but it was **not** the
load-bearing cause of the traffic 404. The dominant blocker — the per-Gateway route
never being projected into the served Envoy snapshot at probe time (rev8's "case (ii):
present-but-empty") — **persists**, and is now isolated as the *primary*, not secondary,
cause. Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces
and per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR

| Profile | Tests run | PASS | FAIL | SKIP | DELTA vs rev8 | Verdict |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** | 37 | **12** | **25** | 0 | **+0** (12/25 → 12/25) | #348 fixed the Service-validation layer but **not** the traffic 404. **Zero 200s** in the entire traffic phase (4410× 404, 177× 503). |
| **MESH-HTTP** | 7 | **3** | **4** | 0 | flat (rev8 read 3/4) | 022 untouched. All 46 mesh probes 200; the 4 FAILs are filter/redirect/weight assertion gaps on the GAMMA capture path. `MeshHTTPRouteMatching` on its known flaky boundary (FAIL this run). |

GATEWAY CORE: **11 PASS / 22 FAIL**. GATEWAY EXTENDED: **1 PASS / 3 FAIL**.
Run wall time: GATEWAY **1969 s (~32.8 min)**, MESH **189 s**.

## The headline finding — #348 worked at the Service layer, the 404 wall is untouched

Two things are now simultaneously true, and that is the whole story of rev9:

**1. #348 is a genuine fix.** The rev8 smell — `Service ... is invalid:
spec.ports[1].name: Duplicate value: "port-80"` (logged 171× in rev8) — is **gone**.
Multi-listener Gateways now reconcile a valid Service and get an address. During the
run, the four shared infra base Gateways held **distinct** MetalLB addresses
simultaneously:

```
gateway-conformance-infra/same-namespace                       192.168.100.50
gateway-conformance-infra/backend-namespaces                   192.168.100.51
gateway-conformance-infra/same-namespace-with-https-listener   192.168.100.52
gateway-conformance-infra/all-namespaces                       192.168.100.53
```

No `Duplicate value` Service-validation errors were observed. The distinct-address /
multi-listener Service layer that rev8 fingered as the gate is **fixed**.

**2. The traffic phase is still a uniform wall — and there were ZERO 200s.** Across the
entire GATEWAY-HTTP run, every single traffic probe returned **404** (4410×) or **503**
(177×). **Not one 200.** Every routing / matching / redirect / header-modifier / HTTPS
traffic test failed exactly as in rev6–rev8. The response is the bare `edge_not_found`
`*`→404 `direct_response` (`Server: envoy`, `Content-Length: 0`).

So the rev8 conclusion needs an amendment: the duplicate-port Service failure was **a
real but secondary** contributor. With it fixed and addresses assigned, the **primary**
cause stands fully exposed: **the per-Gateway route config is present-but-empty in the
served snapshot at the instant the suite probes** — exactly rev8's "case (ii)", now
without the Service red herring in front of it.

## The captured evidence — 19 of 22 route configs are present-but-EMPTY

Captured concurrently with the suite via an admin-port monitor (`#345`'s
`edge.admin.enabled` is still on at rev 88). A representative mid-run snapshot of
`/config_dump?resource=dynamic_route_configs` on edge replica A — **22 conformance
route configs exist, only 3 carry a real route, 19 are 404-only**:

```
conformance route configs in snapshot: 22
  with real routes: 3   empty(404-only): 19
  OK    edge_rt_..._gateway-add-listener                       vhosts=1 real=2
  OK    edge_rt_..._same-namespace-with-https-listener         vhosts=1 real=7
  OK    edge_rt_..._unresolved-...-one-attached-unresolved-route vhosts=2 real=4
  EMPTY edge_rt_..._same-namespace                             vhosts=1 real=0
  EMPTY edge_rt_..._backend-namespaces                         vhosts=1 real=0
  EMPTY edge_rt_..._gateway-with-one-attached-route            vhosts=1 real=0
  EMPTY edge_rt_..._gateway-with-two-attached-routes           vhosts=1 real=0
  EMPTY edge_rt_..._all-namespaces                             vhosts=1 real=0
  EMPTY edge_rt_..._httproute-listener-hostname-matching       vhosts=1 real=0
  ... (13 more EMPTY)
```

And the concurrent monitor caught the live bug-state directly: a Gateway reporting
`status.listeners[0].attachedRoutes = 1` (aether's controller says the route IS
attached), probed at its real per-Gateway IP, returns **404**, with its route config
**present but empty** — only `edge_not_found`. Verbatim from the captured dump of
`same-namespace` (attachedRoutes=1, probe code 404):

```
ROUTE CONFIG: edge_rt_gateway-conformance-infra_same-namespace  vhosts=1
   vhost edge_not_found  domains=['*']  routes=1  real=0   (direct_response 404)
```

Same for `backend-namespaces`. Meanwhile, in the *same* dump,
`same-namespace-with-https-listener` carries real routes — so the empty tables are
**selective**, not a global outage: at any instant a small subset of Gateways is
programmed and the probed one usually is not. This is **identical in shape to rev8** —
#348 did not change it.

This confirms the rev8 diagnosis was correct on the mechanism (status/snapshot
non-atomicity — `attachedRoutes` is written without a guarantee the served snapshot
already contains that route) but **over-weighted** the duplicate-port Service failure as
the gating cause. With #348 in, the Service failure is gone and the score is unchanged —
so the snapshot-projection non-atomicity is, by elimination, the **dominant** blocker.

## GATEWAY-HTTP: the 12 PASS / 25 FAIL breakdown

**The 12 that PASS are all status / config-plane tests** (no traffic, or
status-only assertions):

```
GatewayClassObservedGenerationBump          GatewayObservedGenerationBump
GatewayInvalidRouteKind                      GatewaySecretInvalidReferenceGrant
GatewayModifyListeners                       GatewaySecretMissingReferenceGrant
GatewaySecretReferenceGrantAllInNamespace    GatewaySecretReferenceGrantSpecific
GatewayWithAttachedRoutesWithPort8080        HTTPRouteInvalidCrossNamespaceParentRef
HTTPRouteInvalidParentRefNotMatchingSectionName   HTTPRouteObservedGenerationBump
```

**The 25 FAIL split into two categories:**

### Category A — traffic-wall (404, present-but-empty route table). The dominant bucket.
Every test that sends real traffic and expects a 200 fails on the uniform 404:

```
HTTPRouteSimpleSameNamespace   HTTPRouteCrossNamespace        HTTPRouteMatching
HTTPRouteHeaderMatching         HTTPRouteExactPathMatching     HTTPRouteMatchingAcrossRoutes
HTTPRoutePathMatchOrder         HTTPRouteListenerHostnameMatching
HTTPRouteHostnameIntersection   HTTPRouteHTTPSListener         HTTPRouteRedirectHostAndStatus
HTTPRouteRequestHeaderModifier  HTTPRouteWeight                HTTPRouteServiceTypes
HTTPRouteReferenceGrant         HTTPRoutePartiallyInvalidViaInvalidReferenceGrant
HTTPRouteMethodMatching (EXT)   HTTPRouteResponseHeaderModifier (EXT)   HTTPRouteTimeoutRequest (EXT)
```

All blocked by the **same** root cause (route not in served snapshot → 404). None of
these is a routing-*logic* bug — when a route IS in the snapshot it returns 200 (proven
by the mesh profile's 46× 200 and by the 3 OK gateway route configs). They are blocked
upstream of routing, at projection time. **`HTTPRouteHTTPSListener` and
`HTTPRouteHostnameIntersection` produced the only 503s** (177 total) — the HTTPS path
now reaches a cluster (TLS terminates, listener present) but finds no route/endpoint and
returns 503 rather than 404; a small movement, still not a pass.

### Category B — status-assertion gaps (newly visible now that addresses exist). NEW.
With Gateways finally getting addresses and reconciling, three status-plane gaps that
were masked behind rev8's address starvation are now exercised and fail concretely:

| Test | Assertion the suite makes | What aether does | Gap |
|---|---|---|---|
| `GatewayInvalidTLSConfiguration` (Malformed secret) | `observedGeneration` bumped to 1 on all conditions | For cert-error Gateways, **0/2 conditions updated** — Accepted/Programmed stay at `generation 0` (stale) | Controller does not write status (Accepted/Programmed) for Gateways whose listener cert is malformed/unsupported. **NEW, concrete.** |
| `GatewayWithAttachedRoutes` (unresolved refs) | `ResolvedRefs=False` when a backend is unresolvable | aether sets `ResolvedRefs=True` | Invalid/unresolved backendRef not reflected as `ResolvedRefs=False`. |
| `HTTPRouteInvalidNonExistentBackendRef` | `ResolvedRefs=False`/`BackendNotFound` + HTTP 500 | neither status nor 500 | Nonexistent-backend detection (status + 500 synthetic response) absent. |

Category B is **independent of and additive to** Category A: even if the snapshot
projection were fixed tomorrow, these three would still fail on the status assertions.
They are small, well-scoped controller-status fixes.

## Route-precedence ordering (the P1 follow-up) — confirmed real, still masked

The known follow-up to watch: merged `*`-vhost routes are emitted in namespace/name
order, **not** Gateway-API path-specificity order. This is **confirmed present** in the
data plane. The one route config with many merged routes
(`same-namespace-with-https-listener`, 7 real routes from several concurrently-running
tests) shows all routes as flat `prefix=/` with empty header-matchers in arbitrary
order, first-match-wins:

```
vhost *  domains=['*']
   match prefix=/  ->  infra-backend-v1...   (x4)
   match prefix=/  ->  infra-backend-v2...   (x3)
   match prefix=/  ->  404
```

So `HTTPRoutePathMatchOrder` / `HTTPRouteMatchingAcrossRoutes` would fail on
**precedence** even after the projection fix — but they currently fail one layer earlier,
on the 404. Precedence is a real, queued P1, still masked behind the projection 404.

The **dup-FQDN webhook** (rejects two HTTPRoutes sharing a hostname per Gateway) was
**not** tripped by any conformance test this run — no webhook-rejection failures appeared
in the logs.

## MESH-HTTP — unchanged (022 untouched), with the known flake

3 PASS / 4 FAIL. **All 46 mesh traffic probes returned 200** — connectivity on the GAMMA
capture path is fine. The 4 FAILs are the unchanged proposal-022 gap (filters not applied
on the capture path):

- `MeshHTTPRouteRedirectHostAndStatus` — stable FAIL (redirect filter)
- `MeshHTTPRouteRequestHeaderModifier` — stable FAIL (header filter)
- `MeshHTTPRouteWeight` — stable FAIL (weighted backends)
- `MeshHTTPRouteMatching` — **flaky**; FAIL this run (same as rev8). Sits on a known
  flaky boundary on the capture path.

This is correctly flat vs every prior rev — #341/#342/#343/#345/#348 are all edge-only
and do not touch the mesh data path. The rev8↔rev9 read (3/4 both) is within the
documented flake band (rev3 read 4/3, rev5/rev6 read 3/4).

## DELTA vs prior revs

| | rev3 (rev 72) | rev6 (rev 83) | rev8 (rev 87) | **rev9 (rev 88)** |
|---|---|---|---|---|
| GATEWAY-HTTP PASS / FAIL | 6 / 31 | 12 / 25 | 12 / 25 | **12 / 25** |
| Traffic-phase result | TLS-SAN wall | uniform 404 | uniform 404 | **uniform 404 (4410×404, 177×503, 0×200)** |
| Duplicate-port Service failures | — | — | **171×** | **0 (fixed by #348)** |
| Multi-listener Gateways get an address | no | partial | no (starved) | **yes (.50–.53 distinct)** |
| Route config at the 404 | — | — | (ii) present-but-empty | **(ii) present-but-empty — 19/22 empty, unchanged** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 3 / 4 | 3 / 4 | **3 / 4** |

**Net:** #348 collapsed the rev8 secondary blocker (duplicate-port Service → no address)
to zero, but the GATEWAY-HTTP score did **not** move because the **primary** blocker —
route-into-snapshot non-atomicity — was always underneath it.

## Prioritized remaining tail

| Priority | Profile | Blocker | Fix |
|---|---|---|---|
| **P0** | GW | Per-Gateway route is **not in the served snapshot** at probe time (19/22 route configs present-but-empty). This is now the single dominant cause of the 25-fail wall. | Make `attachedRoutes`/`Programmed` status and the served vhost projection **atomic/ordered**: do not mark a Gateway attached/Programmed until its route config is in a pushed+ACK'd snapshot. Project **all** our Gateways' routes every reconcile, not the coalesced 3–9. (This is the rev8 P0(a)/(c); rev9 confirms it is THE blocker, with the Service red herring removed.) |
| **P1** | GW | `*`-vhost route precedence — merged routes are in namespace/name order, not GW-API path-specificity order (confirmed in the data plane, masked behind the 404). | Sort merged `*` routes by Gateway-API match specificity in `buildEdgeVhostsLocked`. |
| **P1** | GW | Status gaps (Category B): `observedGeneration` not bumped for cert-error Gateways; `ResolvedRefs=False`/`BackendNotFound` + 500 for invalid/nonexistent backendRefs not emitted. | Small, independent controller-status fixes; each unblocks 1 test directly and is not gated on the P0 projection fix. |
| **P0** | Mesh | HTTPRoute filters (redirect/header/weight) on the GAMMA/capture path (proposal 022). | Unchanged since baseline; edge-only PRs don't touch it. |

## Is aether GATEWAY-HTTP-conformant now?

**No — and rev9 corrects the rev8 narrative.** The hoped-for jump did not happen. #348
is a real fix (the duplicate-port Service failures are gone and multi-listener Gateways
get distinct addresses), but it was a **secondary** blocker. With it removed, the score
held at 12/25 and the traffic phase recorded **zero 200s**, proving the **primary**
blocker is the route-into-snapshot non-atomicity: 19 of 22 per-Gateway route configs are
present-but-empty at probe time. The realistic path to a large jump is the **P0 snapshot
projection fix** (atomic status↔snapshot, project all Gateways) — that single fix should
flip most of the ~19 Category-A traffic tests at once, since routing logic itself is
proven correct (mesh 46/46 200, and the 3 populated edge route configs route correctly).
Category-B status gaps (3 tests) and route precedence (P1) are the smaller, independent
tail behind it.

## How it was run

Same programmatic runner as rev2–rev8 (`conformance/aether_rev9_test.go`, a copy of the
rev8 one-off with the env gate renamed `AETHER_REV9`, version held at `0.51.0`, in a
`gateway-api` v1.5.1 checkout under `/tmp/gateway-api-src/conformance` with `replace
sigs.k8s.io/gateway-api => ../`, **not committed**), driving
`suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}` —
  **identical to rev3/rev5/rev6/rev7/rev8** (no advertised-feature change in 0.51.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
  `RequestTimeout=10s`.
- `EnableAllSupportedFeatures=false`, empty `SupportedFeatures`/`ExemptFeatures` for the
  GATEWAY inference path.

The **concurrent monitor** (`/tmp/rev9-diag/monitor.sh`, not committed) probed each
conformance Gateway **directly from the host** (the MetalLB pool `192.168.100.0/24` is
on-network — no in-cluster probe pod needed) and, on any Gateway with `attachedRoutes>0`
that returned 404, dumped both edge replicas' `dynamic_route_configs` via two
`kubectl port-forward`s to the admin `:9901`. It captured the present-but-empty 404 state
for `same-namespace` and `backend-namespaces` (attachedRoutes=1, code 404, route table =
`edge_not_found` only). 371 monitor probes total (329× 404, 42× 000 during transient
listener swaps), zero 200 — matching the suite's traffic wall.

## Cleanup

`CleanupBaseResources=true` removed all `gateway-conformance-*` namespaces; the
per-Gateway LoadBalancer Services GC'd back to the single production edge. Final state:

```
$ kubectl get ns | grep conformance                          → none
$ kubectl get gateway -A | grep conformance                  → none
$ kubectl get svc -n aether-ingress -l aether.io/edge-gateway
NAME                                        EXTERNAL-IP
aether-edge-gw-aether-ingress-aether-edge   192.168.100.101   ← production edge only
```

No aether MetalLB pool IPs leaked — the only `aether.io/edge-gateway` Service cluster-wide
is the production `.101`. (`.99`/`.100` are a pre-existing, unrelated `envoy-gateway-system`
install, not an aether conformance leak.) Both edge-admin `kubectl port-forward`s and the
monitor loop were killed.
