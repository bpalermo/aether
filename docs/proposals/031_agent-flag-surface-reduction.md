# 031 — Reduce the agent's feature-flag surface

Status: Accepted (assessment 2026-07-12)

## Motivation

The agent accumulated one chart boolean per shipped proposal. Several of them
no longer encode an operator decision — they encode "this feature was new
once." Every vestigial flag is a support-matrix multiplier: `false` values
produce configurations that are never tested together (three other defaults
silently assume `transparentCapture=true`), and the redirect-all pair encodes
one three-state setting across two booleans where one implies the other.

## Assessment

| Value | Verdict | Reason |
|---|---|---|
| `agent.importConfig` | **keep** (default off) | A *trust* declaration: accepting peer-written routing config must be explicit. Meaningless single-cluster. |
| `agent.eastWestWaypoint` | **keep** (default off) | A *topology* fact: only correct when pod IPs are NOT cross-cluster routable but node IPs are. Flat-network clustersets must keep it off. Not auto-detectable. |
| `agent.gamma` | **keep as kill switch, default → true** | The real precondition was the Gateway API CRDs (a watch on a missing CRD wedges the manager). With CRD auto-detection (this proposal) the reconciler degrades gracefully, so present-CRDs = on is the right default. |
| `agent.l4Routes` | **retire** | Same story: the only guard it provided was "cluster may lack the experimental-channel CRDs" (TCPRoute/UDPRoute are v1alpha2). Per-type CRD detection replaces it. |
| `agent.transparentCapture` | **retire (hardcode on)** | Capture is the product identity: `meshDns`, `l4Routes`, `captureRedirectAllDefault`, and GAMMA-on-capture all silently degrade without it. Flipping it mid-fleet changes the addressing model under running clients — a poor kill switch. Per-pod `capture.aether.io/*` annotations remain the opt-out. |
| `agent.captureRedirectAll` | **retire** | The dormant ORIGINAL_DST passthrough chain it gates is inert and nearly free; always render it. `captureRedirectAllDefault` becomes the single redirect-all knob: `true` = capture-everything with per-pod opt-out, `false` = per-pod opt-in via annotation. Same three behaviors, one boolean, no implication chain. |
| `agent.eastWestTunnelPort` | **retire (constant 18009)** | Every other data-plane port is a constant (see 030); a per-cluster-settable port that must match clusterset-wide with no cross-cluster validation is a footgun with no matching benefit. |

End state: **four** agent switches — `gamma` (kill switch, default true),
`importConfig`, `eastWestWaypoint`, `captureRedirectAllDefault` — each
encoding something only the operator can know.

## CRD auto-detection (the enabling work)

`common/crdcheck.Present(mapper, gvk)` extends the pattern the gamma
reconciler already used for the `HTTPFilter` CRD to every optional type:

- **gamma**: `HTTPRoute` is the floor — absent ⇒ GAMMA skipped entirely with
  a warning; `GRPCRoute`, `ReferenceGrant`, `HTTPFilter` degrade
  individually (watch AND the matching `List` in Reconcile are gated).
- **l4route**: `TCPRoute`/`TLSRoute`/`UDPRoute` gate per type (they live in
  different Gateway API channels); all absent ⇒ reconciler skipped;
  `ReferenceGrant` gated like gamma. The controller builder picks the first
  present type as its `For()` anchor.

Detection is setup-time (RESTMapper): installing a CRD later requires an
agent restart, same trade the HTTPFilter gate already made.

## Phasing

1. **PR 1 (this doc)** — `common/crdcheck` + detection in the gamma and
   l4route reconcilers. Pure hardening; no flag semantics change.
2. **PR 2** — the surface reduction: chart `agent.gamma` default `true`;
   delete `agent.transparentCapture` / `agent.l4Routes` /
   `agent.captureRedirectAll` / `agent.eastWestTunnelPort` (and the agent
   flags `--transparent-capture`, `--l4-routes`, `--capture-redirect-all`,
   `--east-west-tunnel-port`); capture + the dormant passthrough chain +
   L4 routing become unconditional (CRD-gated); tunnel port becomes the
   constant 18009. RBAC that was gated on the deleted values becomes
   unconditional. Docs updated. Chart minor bump.

Charts digest-pin their images, so a chart release and its binaries move
together — deleting a Go flag the previous chart passed is safe (the previous
chart runs the previous image).

## Compatibility

- `helm --set agent.l4Routes=...` etc. against the new chart is silently
  ignored (helm merges unknown keys into `.Values`; nothing reads them).
  Release notes call the deletions out.
- Behavior changes only for operators who had explicitly set
  `transparentCapture=false` or `l4Routes=false` (unsupported/degraded
  configurations) or relied on `captureRedirectAll=true` alone (per-pod
  opt-in still works — the annotation semantics are unchanged; only the
  machinery flag is gone).
- The e2e harnesses and conformance flows all run the retained defaults.
