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

## M2-default: redirect-all by default for managed pods (amendment, 2026-06-28)

The `(i)` scoped vs `(ii)` redirect-all fork is **decided: redirect-all** (the only model
that captures the *real* `ClusterIP:port` a client actually dials — which is what proposal
023's route targets need). Today redirect-all is a per-pod **opt-in** annotation
(`capture.aether.io/redirect-all`, the M2a spike). This amendment makes it the **default for
managed pods**, Istio-style, so route targets and arbitrary Services are captured with zero
per-pod config:

- **Default on.** A pod with `aether.io/managed=true` (label or namespace auto-injection)
  gets redirect-all from the CNI by default. The annotation flips to an **opt-out**
  (`capture.aether.io/redirect-all=false`) for pods that must not be captured (infra, the
  prober, hostNetwork workloads).
- **Port/range exclusions (Istio parity).** `aether.io/exclude-outbound-ports` and
  `aether.io/exclude-outbound-ip-ranges` annotations carve out destinations that must bypass
  the mesh (a DB port, a metrics scrape target, an external dependency). The CNI emits
  `RETURN` rules ahead of the redirect. This is safe and verification-independent — implement
  first.

### Proxy self-traffic: netns scoping makes this narrow (don't over-exclude)

The redirect rule lives in the **pod netns `nat/output` hook**, so it only fires on traffic
*originating in that pod's netns*. The node proxy's own traffic is mostly out of scope:

- **Mesh upstreams egress proxy-side, not the pod netns** — proven empirically: the scoped
  rule (`dport == 18081`) runs hitless in production soaks; if the proxy's mesh mTLS upstream
  originated in the pod netns it would self-match and loop, and it doesn't. So mesh upstreams
  need no exclusion.
- **Local-app delivery (`127.x`)** is already excluded by the loopback carve-out, and the
  response path by the conntrack-established accept.
- **The one open risk is the redirect-all `original_dst` passthrough**: non-mesh egress is
  captured in the pod netns and forwarded to the real (non-loopback) destination. The
  redirect-all rule has **no dport match**, so *if* that passthrough connection egresses from
  the pod netns it self-matches → loop → and since all non-mesh egress uses passthrough, that
  is the "breaks all egress" the M2a gate flagged. **This must be verified, not assumed**
  (the in-netns listener model makes it plausible but not certain).

### Self-exclusion mechanism: SO_MARK, not UID

If the passthrough loops, exclude **only** the proxy's own forwarded sockets. **Not `meta
skuid`**: the proxy runs as `runAsUser: 0`, so a UID match would also exempt any root-running
*app* pod's egress and silently break its capture. Use **`SO_MARK`**: Envoy stamps a fixed
mark on the passthrough cluster's upstream sockets (`upstream_bind_config.socket_options`
`SOL_SOCKET`/`SO_MARK`), and the CNI adds `meta mark <value> → RETURN` ahead of the redirect.
The mark is unique to the proxy's passthrough — no UID collision, app-UID-agnostic.

## Sequencing

1. **Exclusion annotations** (`exclude-outbound-ports` / `-ip-ranges`) — safe, additive,
   verification-independent. **Implement first.** ✅ **DONE** — `capture.aether.io/exclude-outbound-ports`
   (comma-separated TCP dports) and `capture.aether.io/exclude-outbound-ip-ranges`
   (comma-separated IPv4 CIDRs; bare addr = /32; destination-based, carves out both TCP
   and UDP) emit nft RETURN rules ahead of the redirect in both the scoped and
   redirect-all capture paths. Malformed/non-IPv4 entries degrade to "exclude what parses".
2. **Passthrough-loop verification spike** — confirm whether the redirect-all `original_dst`
   passthrough upstream socket egresses from the pod netns (and thus self-matches the
   redirect). The gate for the default flip.
3. **SO_MARK self-exclusion** (agent xDS `socket_options` + CNI `meta mark RETURN`) — only if
   (2) shows a loop. Makes the default flip safe.
4. **Flip redirect-all to default-on for managed pods** behind a chart value (default off
   until validated on talos), annotation as opt-out. Validate egress volume on the
   node-shared proxy + the non-mesh cleartext catch-all passthrough (the #288 failure mode).

This is the big architectural item; the CNI redirect change is the riskiest blast radius
(every meshed pod's egress), which is why the default flip is last and gated on the spike.
