# Gateway API conformance — rev8: route-config dump pins the suite 404 (2026-06-26)

An eighth run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against **talos-main**, now at
aether **0.51.0 (rev 87)**, image/commit **`e96b8b1`**. Unlike rev2–rev7, this run's
purpose was **not** the score — it was to capture the **definitive diagnostic** the
prior six revs deferred: with the edge Envoy **admin now enabled** (`#345`,
`edge.admin.enabled`, `127.0.0.1:9901` loopback), **dump a failing Gateway's real
Envoy route config at the 404 instant** and answer which of these it is:

- **(i)** route table ABSENT entirely,
- **(ii)** present but EMPTY (only the `edge_not_found` `*` 404-default),
- **(iii)** present-with-the-route-but-not-matching, or
- **(iv)** being NACK'd / warming.

Aether was **not** modified or redeployed. All `gateway-conformance-*` namespaces and
per-Gateway LoadBalancer Services were cleaned up afterward.

## TL;DR — the answer is (ii), and we have the JSON

| Profile | Tests run | PASS | FAIL | DELTA vs rev7 | Verdict |
|---|---|---|---|---|---|
| **GATEWAY-HTTP** | **37** | **12** | **25** | **+0** vs rev7 (12/25) | `#345` did **not** move the 404. Traffic phase is still a uniform **404**. |
| **MESH-HTTP** | 7 | **3** | **4** | flaky band (rev7 read 4/3) | 022 untouched; this run on the lower side — `MeshHTTPRouteMatching` flapped to FAIL on top of the 3 stable capture-path FAILs. |

**The captured smoking gun** — a conformance Gateway that reports
`status.listeners[0].attachedRoutes = 1` (aether's own controller says the HTTPRoute
IS attached), probed at its real per-Gateway IP, returns **404**, and its Envoy route
config is **case (ii): present but EMPTY** — only the `edge_not_found` `*` → 404
default vhost, with **no real route**. Verbatim, from
`/config_dump?resource=dynamic_route_configs` at the 404 instant:

```json
{
  "name": "edge_rt_gateway-conformance-infra_gateway-with-one-attached-route",
  "version_info": "72e1f1ea2655f084d25b01c0bd38c4edb75750b7ac36b1eccf43480c4b5fd51a",
  "virtual_hosts": [
    {
      "name": "edge_not_found",
      "domains": ["*"],
      "routes": [
        { "match": { "prefix": "/" }, "direct_response": { "status": 404 } }
      ]
    }
  ]
}
```

That is the whole route config — one vhost, one route, a `direct_response: 404`. **No
backend route was ever programmed.** The route table is *present* (so it is not (i)),
it has the route config's full name `edge_rt_<ns>_<gw>`, and it is *not* warming or
NACK'd (Envoy accepted the snapshot; `version_info` is a clean hash). It is simply
**empty of the route the suite expects** — exactly **(ii)**.

**Both replicas are byte-identical** — same `version_info` hash on replica A
(`…tgkpq`) and replica B (`…w4zcr`) for every captured Gateway. There is **no
per-replica divergence**; a stale replica is **not** the cause.

## The captured evidence (verbatim)

Captured **concurrently with the suite** via a monitor loop that (a) listed
conformance Gateways, (b) probed each address from an in-cluster pod
(`client` in `aether-test`, empty Host, like the suite), (c) on any Gateway with
`attachedRoutes > 0` that returned **404**, immediately dumped **both** edge replicas'
`dynamic_route_configs`. Four distinct Gateways were caught in the exact bug state —
**attached per status, empty in the data plane:**

| Gateway | `attachedRoutes` | probe code | route table (both replicas) |
|---|---|---|---|
| `same-namespace` | 1 | 404 | only `edge_not_found` `*`→404 |
| `backend-namespaces` | 1 | 404 | only `edge_not_found` `*`→404 |
| `gateway-with-one-attached-route` | 1 | 404 | only `edge_not_found` `*`→404 |
| `gateway-with-two-attached-routes` | 2 | 404 | only `edge_not_found` `*`→404 |

Every monitor probe across the run (**54/54**) returned **404** — never a 503, never a
200 — matching the suite's uniform 404×4080 wall. Replica version-hash comparison for
two of the captured Gateways:

```
gateway-with-one-attached-route:  A=72e1f1ea…  B=72e1f1ea…  IDENTICAL
backend-namespaces:               A=63552c13…  B=63552c13…  IDENTICAL
```

For contrast, in the **same** dump a handful of route configs *do* carry real
routes — e.g. `edge_rt_…_same-namespace-with-https-listener` has a `*` catch-all vhost
with 4 real routes + the merged 404, and `edge_rt_…_gateway-add-listener` has 2. So the
empty tables are **selective**, not a global outage: at any instant a small subset of
Gateways is programmed and the rest (including the one being probed) are not.

## Root cause — the route is never *projected into the snapshot* for the probed Gateway

This is **not** a routing-logic bug (that was fixed and validated in rev7:
hand-applied hostname-less routes return 503, never 404). It is **not** an Envoy NACK
or warming state (the snapshot is accepted; `version_info` is clean). It is **not**
replica divergence (both replicas identical). The route config is empty because the
**agent's gateway-api reconciler never put that Gateway's route into the snapshot at
the time the suite probed.** Three log facts, taken together, pin it:

**1. The projected vhost set is almost always near-empty.** Over the run, the agent's
`projected gateway-api routes` debug line reported the *global* virtual-host count per
reconcile as:

```
  66×  "virtualHosts":1
  11×  "virtualHosts":2
  10×  "virtualHosts":3
   6×  "virtualHosts":6
```

i.e. **~66 of the reconciles emitted a snapshot with a single vhost.** With ~20
conformance Gateways and 7+ HTTPRoutes listed (`"httpRoutes":7`), the vast majority of
the time the conformance route is simply **not in the projected vhosts**, even while
its Gateway's `attachedRoutes` status says it is attached. The status write and the
snapshot projection are **not atomic** — `attachedRoutes` is computed from the listed
routes, but the vhost set the edge is *currently serving* came from a different
(earlier/coalesced) reconcile whose `vhosts` did not include that route. The suite
deletes each test's route within seconds (apply → probe within the
`MaxTimeToConsistency`/`RequestTimeout` window → delete), so the brief window in which
a given route *is* in the served snapshot rarely overlaps the probe — hence a 404, not
a 503.

**2. Only 5–9 of ~20 Gateways are ever projected.** The `projected per-Gateway
entries` count distribution:

```
  22×  "gateways":5    17×  "gateways":6    8×  "gateways":7
   7×  "gateways":8    40×  "gateways":9
```

never reaches the ~20 Gateways that have addresses. In
`buildEdgeGatewayEntries` (`agent/internal/edge/gatewayapi/service.go`), a Gateway with
no port allocation is **skipped** (`if len(allocs) == 0 { continue }`), so it never
gets an `EdgeGatewayEntry` and its route config falls back to the cache's bare
`edge_not_found` `*`→404 default — exactly the empty table we dumped.

**3. The per-Gateway Service reconcile is failing hard, 171 times.** The agent logged
`failed to reconcile per-Gateway Service` **171 times** during the run, every one a
Kubernetes validation error of the form:

```
Service "...gateway-conformance-infra-..." is invalid:
  spec.ports[1].name: Duplicate value: "port-80"
  spec.ports[1].nodePort: Duplicate value: 31469
  spec.ports[1]: Duplicate value: {Port:80, Protocol:TCP, ...}
```

Conformance Gateways with **multiple listeners on the same port** (e.g.
`gateway-with-two-attached-routes`, `same-namespace-with-https-listener` with two
HTTP/HTTPS listeners both on :80/:443) produce a per-Gateway Service with **duplicate
port entries**, which the API server rejects. `#345`'s "idempotent per-Gateway Service
reconcile" did **not** eliminate this — it surfaces now as a hard, repeated validation
failure (one of the most frequent log lines of the run). A Service that never reconciles
never gets a `status.loadBalancer.ingress` IP, so its Gateway never gets an address and
its route is never reachable; it also keeps the reconcile churning, amplifying the
snapshot/status non-atomicity in (1).

**Conclusion (the rev8 answer):** the suite 404 is **case (ii) — route table present
but empty — caused by the per-Gateway route never being projected into the served
Envoy snapshot at probe time.** The contributing mechanics are (a) **status/snapshot
non-atomicity** — `attachedRoutes` is set without a guarantee that the served snapshot
already contains that route, and the projected vhost set is single-entry in ~66 of the
reconciles; (b) **per-Gateway entries are gated on a port allocation** and only 5–9 of
~20 Gateways are projected at a time; and (c) a **duplicate-port per-Gateway Service
validation failure (171×)** for multi-listener-same-port Gateways that `#345` did not
fix, which both starves those Gateways of an address and keeps the reconcile thrashing.
There is **no NACK, no warming, no replica divergence** — Envoy faithfully serves the
empty table the agent gave it.

## Did `#345` move the 404?

**not.** `#345` (idempotent per-Gateway Service reconcile +
`edge.admin.enabled`) delivered the admin endpoint that made this diagnosis possible,
but it did **not** cut the 404s: the run reproduced the uniform-404 traffic wall, and
the `already exists` reconcile churn it targeted is now a **`Duplicate value:
"port-80"` validation error** (171×), not an idempotency race. The smell rev7 flagged
(`…Service already exists`) was a symptom of a deeper problem: per-Gateway Services for
multi-listener-same-port Gateways are **structurally invalid**, so making the create
idempotent doesn't help — the Service still can't be persisted.

## How it was run

Same programmatic runner as rev2–rev7 (`conformance/aether_rev8_test.go`, a copy of the
rev7 one-off with the env gate renamed `AETHER_REV8` and the version held at `0.51.0`,
in a `gateway-api` v1.5.1 checkout with `replace sigs.k8s.io/gateway-api => ../`,
**not committed**), driving `suite.NewConformanceTestSuite` directly:

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`.
- `ManifestFS: conformance.Manifests`; `AllowCRDsMismatch: true`.
- **GATEWAY-HTTP:** supported features **inferred** from
  `GatewayClass.status.supportedFeatures`. The suite read
  `{GRPCRoute, Gateway, GatewayPort8080, HTTPRoute, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout, HTTPRouteResponseHeaderModification, ReferenceGrant}` —
  **identical to rev3/rev5/rev6/rev7** (no advertised-feature change in 0.51.0).
- **MESH-HTTP:** mesh features advertised explicitly
  (`{Mesh, HTTPRoute, HTTPRouteResponseHeaderModification, HTTPRouteMethodMatching,
  HTTPRouteRequestTimeout}`).
- Budgets: `GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`,
  `RequestTimeout=10s`.

The **concurrent monitor** (`/tmp/rev8-diag/monitor2.sh`, not committed) ran two
persistent `kubectl port-forward`s (one per replica, admin `:9901` → local `:19901`
/`:19902`) and a probe-and-dump loop keyed on `(route_config, attachedRoutes, code)` so
each distinct attached-but-404 Gateway was dumped once on both replicas, plus an envoy
log grep around the moment.

The GATEWAY run took **2212 s (~36.9 min)** — essentially identical to rev7's 2210 s,
the same shape. MESH finished in **177 s**. The traffic phase logged **4080 `got 404`**
lines — bit-for-bit the rev6/rev7 tally (zero 503s, zero TLS/x509 errors, zero other
codes). MESH-HTTP read **3/4** this run; rev7 read 4/3. The substance is unchanged — the
delta is the single **`MeshHTTPRouteMatching`** test, which sits on a known flaky
boundary on the GAMMA capture path (its sub-probe `8_request_to_'/foo/v2/example'`
timed out this run). The 3 stable capture-path FAILs (`MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`) are **identical to every
prior rev**; proposal 022 is not implemented, so this is correctly flat — #341/#342/#343
and #345 are edge-only and don't touch the mesh data path.

## DELTA vs prior revs

| | rev3 (rev 72) | rev5 (rev 82) | rev6 (rev 83) | rev7 (rev 86) | **rev8 (rev 87)** |
|---|---|---|---|---|---|
| GATEWAY-HTTP PASS / FAIL | 6 / 31 | 7 / 30 | 12 / 25 | 12 / 25 | **12 / 25** |
| Traffic-phase result | TLS-SAN wall | route-status wall | uniform 404 | uniform 404 | **uniform 404** |
| Hostname-less route in isolation | — | — | NO (rev6) | 503 in ~4 s (fixed) | 503 in isolation (unchanged) |
| **Route config at the 404 (NEW)** | — | — | — | not captured | **(ii) present-but-empty, both replicas identical** |
| Edge admin enabled | no | no | no | no | **YES (#345)** |
| MESH-HTTP PASS / FAIL | 4 / 3 | 3 / 4* | 3 / 4* | 4 / 3 | **3 / 4** |

## Prioritized remaining blockers (updated with rev8 evidence)

| Priority | Blocker | Fix |
|---|---|---|
| **P0 (GW) — now localised** | Per-Gateway route is not in the served snapshot at probe time. | (a) Make `attachedRoutes` status and the served vhost projection **atomic / ordered** — do not mark a Gateway `Programmed`/attached until its route config has been pushed and ACK'd. (b) **De-duplicate per-Gateway Service ports** (collapse multiple listeners on the same port into one Service port) so multi-listener Gateways get an address — kills the 171× `Duplicate value: "port-80"` failures. (c) Project **all** our Gateways' route configs, not the 5–9 that currently get an allocation. |
| **P1** | `*`-vhost route precedence (still masked behind the 404). | Sort merged `*` routes by Gateway-API match specificity in `buildEdgeVhostsLocked`. |
| **P0 (Mesh)** | HTTPRoute filters on the GAMMA/capture path (proposal 022). | Unchanged since baseline. |
| P2 | `GatewayInvalidTLSConfiguration` malformed/terminate-cert detection; `ResolvedRefs=False` reason for invalid backendRefs. | small, independent. |

## Is aether GATEWAY-HTTP-conformant now?

**No — but rev8 finally has the receipt.** The 404 is **not** a transient timing
mystery and **not** a routing-logic bug: it is a **present-but-empty per-Gateway route
table** (case (ii)), byte-identical on both replicas, because the agent never projects
that Gateway's route into the served Envoy snapshot at the probe instant. The three
mechanics — status/snapshot non-atomicity, the allocation gate that drops 11+ of ~20
Gateways, and the 171× duplicate-port per-Gateway Service validation failure that
`#345` did not fix — are now concrete, log-backed, and individually addressable. That
is the rev8 deliverable: **the smoking-gun JSON and a localised root cause**, not a new
score.

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
production `.101`. (A `.99` LoadBalancer exists in `envoy-gateway-system`, but that is a
pre-existing, unrelated Envoy Gateway install, not an aether conformance leak.) Both
edge-admin `kubectl port-forward`s and both monitor loops were killed.

## Reproduction

A ~130-LOC standalone test (`conformance/aether_rev8_test.go`, **not committed**) — a
copy of the rev7 one-off with the env gate renamed `AETHER_REV8` (version `0.51.0`) — in
a `gateway-api` v1.5.1 checkout under `/tmp/gateway-api-src/conformance` (its own Go
module, `replace sigs.k8s.io/gateway-api => ../`), run with `GOWORK=off` from inside
`conformance/`. Plus the concurrent monitor `/tmp/rev8-diag/monitor2.sh` for the
route-config dumps. Same non-obvious requirements as prior revs:
`ManifestFS = conformance.Manifests`, `AllowCRDsMismatch`, empty
`SupportedFeatures`/`ExemptFeatures` with `EnableAllSupportedFeatures=false` for the
GATEWAY inference path. Budgets `GatewayMustHaveAddress=180s`,
`MaxTimeToConsistency=60s`, `RequestTimeout=10s`.
