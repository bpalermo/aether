# Proposal: Arbitrary-Service interception (the genuine GAMMA / Mesh data plane)

**Status:** Design — 2026-06-25
**Relates:** proposal 018 (Gateway API/GAMMA — the Phase 3 "interception model" this
details), proposal 020 (namespace-aware services + the mesh-Service model this builds on),
proposal 004 (demand-scoped distribution — the scope-limiter), proposal 005 (multi-port);
the conformance rev2 + the MESH root-cause investigation (2026-06-25);
[[project_gateway_api_gamma]], [[project_capture_odcds_stuck_404]], [[project_demand_scoped]].

## Summary

aether's transparent capture intercepts only the **aether-generated mesh VIPs on the
single mesh port `:18081`**. Real Kubernetes Services on their **real ports** (e.g. `:80`)
are *not* captured — traffic to them goes straight through kube-proxy and **never touches
Envoy**. The consequence, root-caused via the upstream Mesh-conformance failures: GAMMA
routes and filters apply only on the `<svc>.<meshDomain>` / `:18081` path, never on
standard Service traffic. The upstream Mesh filter tests fail, and — soberingly — the four
that "pass" do so by **kube-proxy coincidence**, not interception. This proposes capturing
arbitrary in-mesh Services on their real ports (the proposal-018 Phase 3 interception
model) so GAMMA routing/filters apply to the traffic apps actually send.

## Motivation

- **The root cause (2026-06-25).** Conformance runs `client http://echo/` = port **80**
  against a real Service (`selector: app=echo`, `80→8080`, no `aether.io/mesh-service`
  label). aether's CNI redirects only `tcp dport 18081`, so port-80 traffic bypasses
  Envoy → plain `200`, no GAMMA route/filter. The filter translation is **correct** — it
  just never fires on the real-Service path (`echo.aether.internal:18081` → 302 works).
- **Illusory passes.** `MeshBasic`/`Matching`/`Split`/`SameNamespace` pass via kube-proxy
  natural routing (version-scoped Services with their own selectors), **not** aether
  interception — they only assert `200 + backend name`, not a mutation. So aether's
  Mesh-conformance standing is overstated.
- **Real GAMMA value** (canary, header/redirect, timeout, mirror) only materializes if
  aether intercepts what clients actually send — the real Service `ClusterIP:port` — not a
  parallel `:18081` VIP the client has to be told to use.

## The shift this represents (state it plainly)

aether's capture is deliberately **scoped** today: one mesh port (`:18081`),
aether-generated VIPs, opt-in. Arbitrary-Service interception moves toward the
Istio/ambient "**capture what the app sends**" model (redirect outbound to in-mesh
destinations on all ports). This is a genuine philosophical change — more powerful
(transparent to apps; conformance-real) but a **bigger data plane** (the proxy sees all
in-mesh outbound) and more surface. It is not an incremental feature.

## Design

Two sub-questions: **which** destinations to capture, and **how** the proxy demuxes them.

### Which destinations

In-mesh Service `ClusterIP:port` pairs (their *real* ClusterIPs and ports, not a parallel
VIP). Two ways to get there:

- **(i) Scoped redirect.** The CNI redirects outbound TCP to the *set of in-mesh
  ClusterIPs* (any port) into the capture listener. Captures only mesh traffic, but the
  ClusterIP set must be synced into the per-pod nftables rules and kept current.
- **(ii) Redirect-all + Envoy demux.** The CNI redirects *all* outbound TCP to non-local
  destinations; the capture listener uses `original_dst` and **passes through** any
  destination Envoy doesn't recognize as in-mesh. Simpler in the CNI (one broad rule),
  but pushes the in-mesh-vs-passthrough decision into Envoy and requires a clean
  pass-through path. (The capture cold-path / ODCDS catch-all already exists as the seed
  of this.)

This `(i)` vs `(ii)` fork is the central decision — resolve it with an early spike.

### How (demux)

The capture listener recovers the destination via `original_dst` and demuxes by
`(ClusterIP, port)` — per-`(ClusterIP, port)` filter chains (`filter_chain_match`
prefix_ranges = the ClusterIP, matched per port). This **generalizes** the two mechanisms
aether already has: the per-ClusterIP TCP floor chains and the `cap_http` HCM — extended
to *all* services, *all* ports, with `tls_inspector`/`http_inspector` selecting HCM (HTTP,
+ the GAMMA route table for that service) vs `tcp_proxy` (L4 floor). Per-port HTTP/L4
heterogeneity already exists (proposal 005 on the inbound).

### The mesh-Service model (ties to 020)

Today aether mints a **separate selectorless VIP** on `:18081`. Arbitrary-Service
interception wants to capture the **real** Service (its ClusterIP, its ports). So aether
should key capture/routing on the *real* Service's ClusterIP+ports, not a parallel shadow
VIP — which is exactly the direction proposal **020** takes (namespace-aware `<ns>/<svc>`
+ EndpointSlice projection making the real Service the mesh Service). 022 therefore builds
on 020 Part 1: the mesh Service becomes the real Service, captured by its ClusterIP, while
`<svc>.<meshDomain>` remains as the explicit/cross-cluster name (two names, one backend).

### Options (from the investigation)

- **Option A (partial).** Auto-label any HTTPRoute-parented Service as a mesh Service →
  captured at `:18081`. But clients/conformance send to the real port, so it doesn't close
  the gap — useful only if clients adopt `:18081` (they don't).
- **Option B (full, recommended).** Capture the real `ClusterIP:port` per the design above.
  The proper interception model; passes the filter tests; transparent to apps.

## Tensions / non-goals

- **Data-plane scope creep.** Capturing all in-mesh ports means the proxy sees far more
  traffic. Mitigate with **demand-scoping** (proposal 004): only capture/serve
  destinations in the node's dependency set.
- **Non-mesh egress** must pass through cleanly (redirect-all, option ii) — an
  on-demand/passthrough path; the ODCDS catch-all is the starting point.
- **Coexistence** with the `<svc>.<meshDomain>` / `:18081` path: keep it working (it's the
  cross-cluster + explicit-opt-in name) while *also* capturing the real Service.
- **Sequencing on 020:** real-Service-as-mesh-Service (020 Part 1) makes this natural; do
  it after, not before.
- **Size:** this is the large item — weeks and a deliberate model shift, not an
  incremental feature. The CNI redirect change is also the riskiest blast radius
  (every meshed pod's egress).

## Verification

- A **real Service on `:80`** with a GAMMA `HTTPRoute{RequestRedirect: 302}`:
  `client http://echo/` → **302** (today: 200). The upstream `MeshHTTPRoute*` filter tests
  pass — and now for real interception, not kube-proxy coincidence.
- A non-HTTP real Service port → the L4 floor (tcp_proxy over mTLS).
- A **non-mesh** destination → passes through unaffected.
- Demand-scoped: only dependency-set services are captured/served on a given node.

## Sequencing

The big architectural item, after the near-term GATEWAY-HTTP work (proposal 021) and
proposal 020 Part 1. Decide the **`(i)` scoped-redirect vs `(ii)` redirect-all** fork with
a spike first — it shapes the CNI rules and the Envoy pass-through path, and it's the
highest-risk change in the proposal.
