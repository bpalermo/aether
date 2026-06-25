# Proposal: Per-Gateway addressing for the edge

**Status:** Design — 2026-06-25
**Relates:** proposal 018 (Gateway API/GAMMA — north-south edge), proposal 003 (edge
proxy); the conformance baseline (`docs/conformance/baseline-2026-06-25*.md`) that
surfaced this, and #323 (namespace-agnostic edge) which exposed it; [[project_edge_proxy_plan]],
[[project_gateway_api_gamma]].

## Summary

After #323 the edge reconciles class-`aether` Gateways **cluster-wide** and publishes
their `Accepted`/`Programmed` status — but every Gateway shares the single edge
LoadBalancer IP and **none gets its own address**. That blocks the GATEWAY-HTTP
conformance *traffic* tests (which assert each Gateway has its own `status.addresses` and
serves only its routes there) and real multi-tenant isolation. This proposes per-Gateway
addressing in **phases**, reusing aether's single edge proxy via per-Gateway LoadBalancer
Services + **internal-port demux** (forced because `externalTrafficPolicy: Cluster` DNATs
the destination IP, so the proxy can't demux by the Gateway's LB IP), with a
per-Gateway-Deployment model held in reserve for hard isolation/scale.

## Motivation

- **Conformance.** GATEWAY-HTTP tests provision Gateways, expect each to get its own
  `status.addresses`, and send traffic to that address expecting only that Gateway's
  routes. Today aether publishes status but no address → the address-dependent tests
  (and the distinct-address subset) fail at the traffic step.
- **Multi-tenant.** Different teams' Gateways should be isolatable by address (and blast
  radius), not all funnelled through one shared IP.
- **Where we are.** One edge Deployment behind one `LoadBalancer` Service
  (`192.168.100.101`); the proxy binds the union of all Gateways' listeners; Gateways get
  no `status.addresses`.

## Current shape — the constraints that drive the design

- The edge is a **normal Deployment** (not host-network) behind **one LoadBalancer
  Service** with `externalTrafficPolicy: Cluster`. Consequence: kube-proxy DNATs the LB
  IP to the pod IP, so **the proxy never sees the Gateway's LB IP** — demux by
  destination IP is not available. Demux must be by **the port the connection lands on
  inside the pod**.
- The edge already maps a Gateway listener's external port to a bind/container port (it
  fronts `:80`/`:443` on container ports today). Per-Gateway internal-port allocation is
  an extension of that existing mapping, not a new mechanism.
- **MetalLB** has one auto-assign pool (`192.168.100.50–254`) — ample addresses for a
  Service-per-Gateway approach.

## Design — phased

### Phase 1 — publish a (shared) address on every Gateway (minimal unlock)

- Set `status.addresses` on every class-`aether` Gateway to the **shared edge LB IP**.
  The edge demuxes by listener **hostname + port** across all Gateways (it already serves
  the union of their listeners).
- **Unblocks the bulk:** address-dependent setup and every test that does *not* require a
  *distinct* address per Gateway passes; the edge serves each Gateway's routes via
  Host/SNI/port.
- **Honest limitation:** two Gateways sharing a `(hostname, port)` collide, and the
  distinct-address test subset still fails. This is the cheap, incremental step.

### Phase 2 — distinct addresses via per-Gateway LB Service + internal-port demux (recommended)

Per class-`aether` Gateway, the edge controller:

1. **allocates a unique internal (container) port** for each of the Gateway's listeners
   (from a bounded range; freed when the Gateway is deleted);
2. **creates a `LoadBalancer` Service** for the Gateway — MetalLB assigns an IP from the
   pool — mapping each listener's external port → that listener's allocated internal port
   (`targetPort`);
3. sets `status.addresses` = the per-Gateway LB Service's assigned IP.

The edge **proxy** generates a listener per `(Gateway, listener)` bound on the internal
port, serving only that Gateway's attached routes. **The Gateway is identified by the
internal port the connection lands on** (since dest-IP demux is unavailable under
`externalTrafficPolicy: Cluster`).

- Reuses the **single edge proxy** — no per-Gateway pods. The new state is the per-Gateway
  LB Services + the internal-port allocation, both owned/GC'd by the controller alongside
  the Gateway.
- Gives genuinely **distinct addresses** (per-Gateway LB IPs) → the distinct-address
  conformance tests and real isolation work.

### Phase 3 — per-Gateway Deployment (isolation/scale fallback)

- The controller provisions a **Deployment + LB Service per Gateway** (the Envoy-Gateway /
  Istio model). Each proxy serves only its own Gateway → no demux at all, hard isolation,
  independent scale — at the cost of a Deployment per Gateway.
- **Reserve** for tenants that need real isolation or where shared-proxy demux proves
  insufficient (e.g. heavy noisy-neighbor concerns). Opt-in (annotation), not the default.

## Trade-offs / tensions

- **`externalTrafficPolicy: Cluster` forces internal-port demux** (Phase 2). Switching to
  `Local` + a host-network edge could enable dest-IP demux (bind the LB IPs directly), but
  that re-architects the edge's networking — out of scope here.
- **Internal-port allocation** is the fiddly part of Phase 2: a bounded range, conflict
  avoidance, and GC on Gateway delete. Keep it deterministic (hash/seq) and observable.
- **Shared proxy (Phase 1–2) vs isolation (Phase 3):** one proxy is lighter and aether-
  native but couples Gateways' fate; Phase 3 isolates at a resource cost.
- Phase 1's shared address is **honest but conformance-incomplete** — ship it as the quick
  win, not the end state.

## Verification

- **Phase 1:** every class-`aether` Gateway shows `status.addresses` = the edge LB IP;
  routes served by Host/port; conformance gets past address-dependent setup.
- **Phase 2:** two Gateways in different namespaces get **distinct** LB IPs; traffic to
  Gateway-A's IP reaches only A's routes (not B's); the distinct-address conformance tests
  pass.
- **Phase 3:** a Gateway with the isolation annotation gets its own Deployment + IP; its
  blast radius is independent.

## Sequencing

After the current conformance feature work. **Phase 1** is a quick win (just publish
`status.addresses`) and likely flips a chunk of GATEWAY-HTTP from "setup-blocked" to
running. **Phase 2** is the real distinct-addressing and the bulk of the work. **Phase 3**
is optional/opt-in. Independent of proposals 019/020 (connectivity / registry naming).
