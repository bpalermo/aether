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

## Round 2 — binary-flag retirements (2026-07-12, chart 0.77.0)

PR 2 cleaned the chart surface; a follow-up assessment applied the same test
("does the non-default value produce a configuration anyone runs or tests?")
to the remaining **binary** flags across all commands.

| Flag | Verdict | Reason |
|---|---|---|
| agent `--proxy-id` | **retired** | Always set identical to `--node-name` (chart: both `$(NODE_NAME)`; edge: both from `POD_NAME`), while the snapshot cache was keyed by node-name and the xDS server by proxy-id — divergence was a latent bug, not a feature. `--node-name` is now the single identity; `queryNodeMetadata` fetched the Node object by proxy-id, which only worked because they were equal. |
| agent `--remove-startup-taint` | **retired** | The chart never passed it, the nodes RBAC is unconditional, and the remover is a no-op when the taint is absent. No configuration where `false` helps. |
| agent `--gamma` Go default | **flipped to `true`** | PR 2 flipped only the chart default; the binary default stayed `false`, contradicting the end state. The chart now passes the explicit boolean form (`--gamma={{ .Values.agent.gamma }}`) so `agent.gamma=false` still works. |
| edge `--edge-readiness-port` | **retired (constant 18021)** | 030's "data-plane ports are constants" rationale; the edge isn't hostNetwork, so there is no conflict story. |
| edge `--edge-per-gateway-addressing` | **retired (unconditional)** | 021 Phase 2 has been the default, conformance-hard-gated, and soak-validated since June. Phase 1 (shared `SetVirtualHosts`) remains only as the `EdgeServiceName`-empty / port-allocation-failure fallback, not as a configuration. The shared edge Service is always ClusterIP. |
| edge `--mounted-registry-dir` | **retired (fixed pod-local path)** | Existed only to point at an always-empty dir, propped up by the `edgeStorageDir` default-detection hack. |
| registrar `--generate-mesh-services` | **retired (unconditional)** | The `transparentCapture` argument one hop upstream: unconditional capture and default-on mesh DNS resolve against the VIPs it generates; a registrar without it silently degrades the whole capture path. Service CRUD RBAC becomes unconditional. |
| registrar `--spire-trust-domain` | **retired (derived)** | Agent and edge resolve the trust domain from their own SVID; the registrar alone took it as a flag (default: the ROOTCA any-peer sentinel) that could silently disagree with what SPIRE issues — the exact failure class of the historical trust-domain-mismatch bug. Now derived via `TrustDomainFromSource`, tightening the pre-flag ROOTCA default to same-trust-domain peers (the mesh is one trust domain by design). |
| cni-install `--transparent-capture` | **retired** | The direct PR 2 leftover: the agent side went unconditional but the netconf gate survived with the chart hardwiring the flag true. The netconf `transparent_capture_enabled` field and the plugin gate are gone; the scoped capture redirect is unconditional for managed pods (per-pod `capture.aether.io/*` annotations remain the opt-out). The writer-less legacy `capture_redirect_all_enabled` netconf field went with it. |
| controller `--pod-ndots` | **replaced by `--mesh-domain`** | ndots *is* the mesh domain's label count; a separately-configured copy can drift from the domain it describes. Derived in the binary; the chart passes the domain it already knows. |

Assessed and **kept**:

- agent `--mesh-dns` / `--mesh-dns-upstream` (+ cni-install `--mesh-dns` /
  `--host-ip`) — the one addressing-model flag left standing. By the
  `transparentCapture` logic it would go, but it installs a per-pod :53 DNAT
  to a node-local resolver, which can genuinely conflict with node-local DNS
  caches / nonstandard DNS setups — a topology fact, kept as a kill switch
  (like `gamma`). Recorded here so it stops looking like an oversight.
- Everything in PR 2's keep column (`importConfig`, `eastWestWaypoint`,
  `controlCluster`, `captureRedirectAllDefault`), the multi-cluster opt-ins
  (`--enable-mcs`, `--peer-etcd`, `--region`, `--registry-backend`), edge
  public ports / geoip / TLS, supervisor tuning, and the manager/telemetry
  and socket-path plumbing.

Same compatibility argument as PR 2: charts digest-pin their images, so the
chart release and its binaries move together. `TestRetiredFlagsGone` (and its
edge/registrar/cni/controller siblings) pin every retirement.
