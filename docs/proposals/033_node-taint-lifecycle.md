# Proposal: Node-taint lifecycle — re-arm the agent-not-ready taint across reboots and agent outages

**Status:** Accepted — 2026-07-19
**Relates:** issue #263 / #261 (the startup taint + one-shot removal), proposal 031
(agent flag-surface reduction — the "no new flags" philosophy this follows).
**Fixes:** #569. **Non-goal:** container-restart waves on a re-armed node (#567).

## Problem

Aether gates a not-yet-ready node with the `aether.io/agent-not-ready:NoSchedule`
startup taint (#263): the operator registers nodes with it
(`kubelet --register-with-taints`), workload pods do **not** tolerate it, and the
agent removes it once its CNI socket is serving — so a pod's fail-closed CNI ADD
never arrives before the agent can handle it. That mechanism only ever fires
**once, at node registration**. The 2026-07-19 incident review found three gaps
where a node ends up schedulable while the agent is *not* serving:

- **G1 — reboot leaves no taint.** The kubelet applies `register-with-taints`
  only when it first creates the Node object, never again for an existing one.
  After a reboot the on-disk CNI config keeps the kubelet reporting `Ready`, so
  the node is schedulable **before** the agent process comes back up. Pods land
  and sit in `ContainerCreating` on fail-closed CNI ADDs.
- **G2 — removal is one-shot.** The old `TaintRemover` was a run-once manager
  `Runnable`: it waited for the CNI socket, removed the taint, and returned. A
  taint (re-)applied to an already-running node — by anything, including this
  proposal's own guard — was **never** removed again.
- **G3 — k8s's own taints don't cover this window.** `node.kubernetes.io/not-ready`
  and `unreachable` track kubelet `NotReady`, not the aether-specific
  "kubelet Ready but agent not serving" window. They can't gate on CNI readiness.

## Design

Two cooperating controllers, split by who is allowed to write which direction of
the taint. No new flags (proposal 031): the grace period is a compiled constant.

### Agent — reconciling remover (removes only)

`agent/internal/node.TaintRemover` becomes a controller-runtime reconciler on the
agent's **own** Node (filtered by node name via a predicate, so each agent watches
exactly one node). On every Node event it removes the taint **iff** the taint is
present **and** the CNI Unix socket is serving (the existing socket gate is kept;
when the taint is present but CNI is down it requeues rather than blocking a
worker — a reconciler can't watch a socket). `NeedLeaderElection=false` (per-node),
best-effort (never fails agent startup). This closes **G2**: a taint the guard
re-arms is dropped again as soon as the agent is serving. The pure `removeTaint`
helper and its unit tests are preserved; the exported `TaintRemover` name is stable
(only its wiring changes from `m.Add(...)` to `.SetupWithManager(m)`).

### Controller — node-taint guard (adds only)

New `controller/internal/nodetaint.Guard`, leader-elected (one writer mesh-wide).
It watches **Nodes** and the **agent DaemonSet pods** (label selector
`app.kubernetes.io/name=aether-agent` in the controller's own — aether-system —
namespace; each pod event maps back to its node). When a node has **no Ready agent
pod** it starts a grace timer; if the node is still without a Ready agent when the
grace elapses, it **adds** the taint. It **never removes** it. This closes **G1**
(a rebooted node with no serving agent gets re-tainted) and, with the agent's
reconciling remover, **G2** end to end.

- **Grace = 30s, a constant (not a flag).** Long enough that a routine agent roll —
  pod deleted, replacement Ready — completes inside the window, so the taint does
  **not** flap on every DaemonSet update (debounced via a per-node first-seen-missing
  timestamp, cleared the moment a Ready agent is seen again).
- **Idempotent.** If the taint is already present the guard does nothing.
- **Control-plane nodes: data-driven, not skipped by assumption.** The guard does
  not special-case control-plane nodes. It re-arms a node **only if** that node has
  no Ready agent pod — and a node has an agent pod only where the agent DaemonSet
  schedules. Since the agent DS carries **no** control-plane toleration and **no**
  nodeSelector (charts/aether/templates/agent-daemonset.yaml), it does not run on
  tainted control-plane nodes, so the guard's pod-presence check naturally skips
  them. If an operator *does* run the agent on control-plane nodes, the guard
  governs them correctly with no code change.

### Effect stays NoSchedule

Kept `NoSchedule`; see rejected alternatives.

## Toleration audit

The taint gates *workload* pods; every aether component must tolerate it (they *are*
the thing that makes a node ready, or must run regardless).

| workload | tolerates? | note |
|---|---|---|
| agent DaemonSet | yes (existing) | removes the taint; `cni-install` is its initContainer, covered |
| aether-proxy DaemonSet | yes (existing) | runs on every node |
| controller / registrar / edge Deployments | yes (existing) | must run to serve the mesh |
| **workload / test / conformance manifests** | **no** (verified) | grep across the repo: only the aether components tolerate it |

No workload or test manifest tolerates the taint (verified by repo-wide grep). The
`cni-install` init container needs no separate toleration — it runs inside the agent
DaemonSet, which already tolerates.

### SPIRE agent DaemonSet — external chart, documented only

When SPIRE is enabled, the SPIRE **agent** DaemonSet (from the external
`spiffe/spire` helm chart, **not** this repo) delivers the SVIDs the aether agent
needs, so it too must tolerate the taint or it will never schedule onto a freshly
re-armed node — deadlocking startup. This chart cannot touch the external
DaemonSet; the toleration must be set in the **spire helm values** under:

```yaml
spire-agent:
  tolerations:
    - key: aether.io/agent-not-ready
      operator: Exists
      effect: NoSchedule
```

(`spire-agent` is the sub-chart of `spiffe/spire` that renders the agent DaemonSet;
this repo's `values.yaml` header calls this out.)

## RBAC

The controller Deployment's ClusterRole gains `nodes: [get,list,watch,patch]` and
`pods: [get,list,watch]` (the agent already has nodes/patch). `patch` on nodes is
add-only in practice — the guard only ever appends the taint.

## Rejected alternatives

- **NoExecute instead of NoSchedule.** `NoExecute` would **evict** every
  non-tolerating pod already running on the node the instant the taint is
  (re-)applied — i.e. on **every** agent roll or transient agent restart, the guard
  would drain the node's mesh workloads. That is catastrophically more disruptive
  than the problem it solves (a re-armed node keeps its running pods; only *new*
  scheduling is blocked until the agent is serving). Rejected. `NoSchedule` gates
  new pods without touching running ones.
- **Agent re-applies its own taint on restart.** An agent can't taint the node it's
  gating *before* it's running (the reboot/crash window is exactly when it is
  absent), and making every agent a node-taint writer multiplies the write path and
  races. A single leader-elected controller writer is simpler and covers the "agent
  process entirely gone" case the agent itself can't.
- **A flag for the grace period.** Proposal 031 retired the agent's flag surface;
  30s is a well-understood roll-completion window. A constant keeps the operator
  contract flat. If tuning is ever needed it can move to MeshConfig, not a flag.
- **Rely on kubelet `register-with-taints` re-applying.** It doesn't (G1); the
  kubelet applies register-taints once per Node object creation. Nothing external
  re-arms on reboot — which is the entire motivation.
- **A DaemonSet-only remover keyed on kubelet readiness.** k8s's not-ready taints
  (G3) track kubelet Ready, not CNI-serving, so they'd remove the gate too early.
```
