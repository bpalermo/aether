# 030 — Move the remaining data-plane ports out of Istio's reserved range

Status: Proposed

## Motivation

Istio reserves 15000-15090 for its data plane (15000 admin, 15001 outbound,
15006 inbound, 15008 HBONE, 15021 health, 15053 DNS, 15090 telemetry). Aether
already stays out of that band on the outbound side — `ProxyOutboundPort`
18081, `ProxyCapturePort` 18001, `ProxyDNSResolverPort` 18054, and the
passthrough fwmark `0xae7e` (vs Istio's 1337) were all chosen explicitly so
the two meshes can share a node (see `common/constants/proxy.go`). Three
ports still sit inside the band, two of them on Istio's *exact* well-known
numbers:

| port | role | collision |
|---|---|---|
| 15008 | mesh inbound — every pod's netns mTLS listener (`defaultInboundPort`, `agent/internal/xds/proxy/ingress.go`) | Istio HBONE is exactly 15008: an ambient ztunnel or sidecar on a shared node/pod binds the same number; every operator and tool that sees :15008 assumes HBONE |
| 15009 | east/west waypoint tunnel — host-netns node port (`DefaultEastWestTunnelPort`, `agent/internal/xds/proxy/edge.go`; chart `agent.eastWestTunnelPort`) | inside the reserved band; host-netns makes node-sharing collisions real |
| 15021 | edge readiness (`DefaultEdgeReadinessPort`; chart `edge.readinessPort`) | Istio's health port is exactly 15021 |

Finishing the move eliminates the shared-node collision class entirely and
stops advertising Istio semantics on non-Istio ports.

## Target numbering

Extend the established aether 18xxx convention, keeping the trailing digits
so the mapping is memorizable:

- 15008 → **18008** (mesh inbound)
- 15009 → **18009** (east/west tunnel)
- 15021 → **18021** (edge readiness)

None are IANA-registered or otherwise conventional; they join 18081/18001/
18054 as "the aether range". A pod's app binding 18008 inside its own netns
would collide with the inbound listener — the same (accepted, undocumented)
risk that exists today at 15008; it moves, it does not grow.

## Migration mechanics

The three ports have three different blast radii; the plan orders them
cheapest-first.

### 15009 → 18009: free, do it immediately

`--east-west-waypoint` is default-OFF and enabled nowhere (talos runs with
the flag off; only the e2e harnesses set the port, explicitly). The port
must match across a clusterset, so changing it AFTER adoption would need a
coordinated multi-cluster dance — changing it NOW costs one constant, one
chart default, and the two e2e harness variables. This is the strongest
argument for doing the whole proposal now rather than later.

### 15021 → 18021: pod-local, atomic per roll

The readiness listener and the kubelet probe are templated from the same
chart value (`edge.readinessPort`), so each edge pod is self-consistent the
moment it starts; a rolling Deployment update is inherently safe. Change the
Go default + chart default + the `ne ... 15021` template conditional
together. Operators who pinned `edge.readinessPort` explicitly are
unaffected (the value stays honored).

### 15008 → 18008: cross-node, needs a dual-bind transition

The inbound port is baked into BOTH sides of every mesh hop from per-node
proxy config: the listener (pod netns bind) and the dialers (peer egress
clusters, the edge's backend dial, the waypoint's local forward, capture's
inline assignments — all `defaultInboundPort` call sites). A naive constant
flip breaks a mixed fleet mid-roll: a new-config proxy dials 18008 on a pod
whose old-config inbound only binds 15008. Hitless requires the listener to
lead the dialers by one full fleet roll:

1. **Phase A (release N) — dual bind.** The inbound listener binds both
   15008 and 18008 (a second identical listener; same filter chains, same
   SDS). Dialers still dial 15008. Roll the fleet; every pod now accepts on
   both ports. Envoy cost: one extra listener per pod, no traffic shift.
2. **Phase B (release N+1) — flip dialers.** `defaultInboundPort` → 18008
   everywhere it is *dialed* (egress, edge, waypoint, capture). Mixed fleet
   is safe in both directions: old dialers hit 15008 (still bound), new
   dialers hit 18008 (bound since Phase A).
3. **Phase C (release N+2) — drop 15008.** Remove the legacy bind. Any
   straggler still dialing 15008 at this point is a proxy two releases
   stale — outside the supported skew.

The port is not part of any identity or routing datum — it appears in no
SAN, no SNI (019's SNI carries the *service* port), and no registry value —
so mTLS, EDS data, and cross-cluster config are untouched. The 019 waypoint
path is per-cluster self-consistent (ew_ingress forwards to its own
cluster's constant) and rides the same three phases.

Each phase is one PR + one chart release, validated by the standard ladder:
unit + `envoy --mode validate`, conformance (GATEWAY-HTTP + MESH-HTTP),
waypoint/replicator e2e harnesses, then a talos roll under the external
prober (100% bar), with the consolidated soak as the Phase C exit check.

## Sequencing

1. **PR 1 (this proposal +) 15009→18009 and 15021→18021** — both safe in a
   single release, no transition machinery.
2. **PR 2 — Phase A** dual-bind inbound (constant `legacyInboundPort =
   15008` retained beside `defaultInboundPort = 18008`... implementation
   detail: keep the *names* stable and add the second bind; the flip in
   Phase B is then a one-line dial-side change).
3. **PR 3 — Phase B** dialers → 18008 (after PR 2's chart is fully rolled
   on every participating cluster).
4. **PR 4 — Phase C** drop the 15008 bind + sweep the ~15 stale `:15008`
   comments and docs (`docs/configuration.md`, proposals are historical
   record — leave them).

Between PR 2 and PR 3, and PR 3 and PR 4, every participating cluster must
complete its roll — in a multi-cluster mesh the *fleet* is the clusterset.
Today that is one cluster (talos-main), so the whole sequence can land in a
week of normal release cadence.

## Alternatives considered

- **Flag-configurable inbound port instead of constants.** More machinery
  (the flag must flow into every dialer AND the listener, per node, and a
  fleet with mixed flag values is exactly the broken state the phases
  avoid). The port is mesh-wide by nature; a constant with a staged flip is
  simpler and safer than pretending it is per-node tunable.
- **Do nothing / document the collision.** The 15009 argument defeats this:
  the east/west port becomes effectively frozen the day the first clusterset
  enables the waypoint. The cheap window is open now and closes on adoption.
- **A different range (e.g. 14xxx).** No advantage over extending the
  existing, already-documented 18xxx convention.
