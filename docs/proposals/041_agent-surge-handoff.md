# Proposal 041: Surge roll for the node agent — a standby agent takes the node over

**Status:** Draft 2026-10-02. Not implemented. The cheap half of #1123 shipped
in its PR (startup overlap, image pull policy, cni-install idempotence,
readiness cadence); this is the remaining half.
**Author:** Bruno Palermo
**Date:** 2026-10-02
**Related:** #1123 (the gap), #1129 (a successor proxy's SDS fetch timing out
inside it), #1103 / #1118 (ADS reconnect backoff 0.1-1 s; first serve waits for
the local client certificates), #1085 / #1091 (a successor waits for CDS+LDS),
#1094 / #1099 (first serve waits for the capture projection), 033 (node-taint
lifecycle), 001 (the proxy's own hot-restart handoff, the model this follows).

## Problem

A node agent roll is delete-then-create (`maxSurge: 0, maxUnavailable: 1`). From
the moment the old agent exits until the new one serves xDS, the node's proxy
has no ADS stream and routes on stale config. On talos-main on 2026-10-02 (agent
logs, VictoriaLogs; 10 node restarts across the 03:05Z and 06:53Z rolls) that
window was 7.6-15.8 s:

| term | range | what it is |
|---|---|---|
| old agent exit | 0.02-0.1 s | SIGTERM to `Wait completed`; already prompt |
| pod replacement | 5.6-10.6 s | API delete, DaemonSet controller create, sandbox + volumes, two `Always` image checks (1.44 s by the kubelet's own count), cni-install, container start |
| startup | 2.0-5.1 s | own SVID 0.15-2.0 s, local listeners 0.25-0.98 s, registrar reconnect ~1.05 s, client certificates 0-1.47 s |
| proxy reconnect | 0.06-0.9 s | the 0.1-1 s backoff of #1118 |

The #1123 PR removes what was serial without need from *startup* and ~1.5 s
from *replacement*. On kind (`e2e/agent-restart-gap.sh`, 2 nodes, etcd backend,
proxy-side `control_plane.connected_state`) it took the gap from median 12.0 s /
p99 17.9 s (n=16) to median 5.5 s / p99 8.8 s (n=20), and everything after the
agent's own SVID now takes ~0.1 s. What is left is not the agent's:

- **replacement**, 2.8-3.4 s on kind (images are `Never`-pulled there; talos
  adds its registry checks, now gone, and a slower sandbox). Of that, the
  cni-install init container costs its run (0.1-0.3 s) plus the kubelet
  noticing it exited (0.4-1.4 s, the PLEG relist).
- **the first SVID**, 1.0-5.2 s on kind (0.15-2.0 s on talos): the SPIRE
  agent's Kubernetes attestor cannot match the new container until the kubelet
  reports it, so the agent's Workload API fetch waits on SPIRE and the kubelet,
  not on aether.

Both are paid with nothing serving the proxy. A p99 under 5 s needs the new
agent to be up **before** the old one goes away.

## Design: a standby agent

Roll the agent DaemonSet with `maxSurge: 1, maxUnavailable: 0`. The new pod
starts beside the old one as a **standby**: it builds everything (identity,
registry watch, local listeners from storage, capture projection, the pods'
client certificates; every first-serve gate of #1085/#1099/#1118) but binds no
node socket and writes no node file. It reports Ready only when its first
snapshot is complete. The DaemonSet controller then deletes the old pod; the
old agent exits within ~50 ms; the standby takes the node over and serves.

The handoff signal is a lock, not the API: every agent holds an exclusive
`flock(2)` on `/run/aether/agent.lock` while it owns the node. The standby
blocks on it (in a goroutine, from process start); the kernel releases it the
instant the old process dies, however it dies. On acquiring it the standby:

1. reconciles its listeners with storage: pods the old agent ADDed or DELed
   during the overlap (a diff against the pods it loaded, not a full reload: the
   full load is 0.25-0.98 s on talos);
2. binds `cni.sock` and `xds.sock` (unlink + bind, as today's restart);
3. starts the writers it held back (observed upstreams, QUIC pairs, the mesh-DNS
   snapshot and its heartbeat).

The proxy's stream to the old agent ended when that agent exited; its next
reconnect attempt (fully jittered, first within 0.1 s) lands on the new socket.
Expected gap: the reconnect backoff plus the diff, ~0.1-0.4 s, independent of
pod-start cost.

### What the overlap must not break

| hazard | resolution |
|---|---|
| **hostNetwork ports.** The pod declares `containerPort` 8080 (metrics) and 8082 (health); on a hostNetwork pod those default to hostPorts, so the scheduler's NodePorts filter keeps the surge pod Pending forever, and the process could not bind them anyway. | Drop the `ports:` declarations (informational on hostNetwork). Health moves to a unix socket in the pod's own `/tmp` (emptyDir) with exec probes through a stdlib-only probe binary (the `proxy-ready` pattern, #673); kubelet must never be answered by the *other* agent, which a shared TCP port with `SO_REUSEPORT` would do. Metrics bind after the lock (or move to OTLP only). |
| **Socket ownership.** Go's `UnixListener.Close` unlinks its path by name, so the old agent exiting after a rename would delete the new socket. | With the lock, the standby binds only after the old process is gone: no two listeners on one path ever. Belt and braces: `SetUnlinkOnClose(false)` plus an inode check before unlinking. |
| **CNI ADD during the overlap.** ADDs go to the old agent (it owns `cni.sock`) and are written to storage and pushed to the proxy it serves. The standby must not answer them: the proxy is not its client, so its ACK wait (`envoyAckTimeout`, 2 s) would expire and the pod would start with no listener. | The standby does not bind `cni.sock`; the post-lock diff picks up what the old agent did. In-flight ADDs finish inside the old agent's GracefulStop. |
| **Persisted state, two writers.** The old agent flushes observed upstreams on exit (#701) and writes QUIC pairs (#1033) and the mesh-DNS snapshot. | The standby restores them at start and writes nothing until it holds the lock; the old agent's final flush lands first. Pairs the proxy subscribed to during the overlap are re-stated on the fresh stream (#1036). |
| **Registry writes, drain marks, SPIRE streams.** | Idempotent from two writers: the registry keys an endpoint by service and pod IP (`registry.Registry`), a drain mark is a state, and two Broker subscriptions for one pod are two streams of the same SVID. To verify in implementation: nothing the old agent does on its way out deregisters what the standby relies on. |
| **Node taint (033).** | The controller's guard already asks "any non-terminating Ready agent pod on the node" (`controller/internal/nodetaint/guard.go`), so two pods are fine. The standby's taint remover must still gate on owning the node (it removes only when *its* CNI serves). |
| **Readiness meaning.** The DaemonSet controller deletes the old pod when the new one is Ready. | A `standby-complete` readiness check: first snapshot built and every first-serve gate passed. After takeover the usual checks apply. |
| **Plugin/agent version skew.** cni-install in the surge pod replaces the plugin binary while the old agent still serves. | The CNI gRPC API must stay backward compatible across one version (it already must across a proxy-first roll). |
| **Capacity.** Two agents on the node for the overlap. | `system-node-critical`; the surge pod's requests (200m, memory) must fit, or the roll stalls on that node (visible, not silent). |

## Rollout

Behind `agent.updateStrategy.surge` (default `false`). The kind harness from
#1123 (`e2e/agent-restart-gap.sh`) is the gate: green at `ARG_BOUND=1` across 20
rolls with load running, then a talos soak with the agent rolled under the
standard churn.

## Open questions

1. Diff reconcile: the cache has no "pods I hold" API today;
   `LoadListenersFromStorage` merges and never removes. A DEL during the overlap
   needs a removal path (or the ghost sweep has to cover it promptly).
2. Exec probes cost a fork per period. The probe binary must stay tiny (the
   proxy-ready deps test pattern), and the period stays 2 s.
3. What does the standby do if the old agent never exits (stuck terminating)?
   It stays a standby and Ready; the old pod's terminationGracePeriod (30 s)
   bounds it.
4. Is an operator-initiated `kubectl delete pod` (no surge) still the old
   delete-then-create? Yes: the lock is free at start, so the agent serves
   immediately, as today.

### Resolutions (implementation)

1. **Diff reconcile.** `storage.Reloader` (`CachedLocalStorage.Reload`)
   re-reads the directory under the storage lock and returns
   added/updated/removed records, the removed ones with the value the standby
   built from. `CNIServer.ReconcileStorage` applies it under `lifecycleMu`:
   `RemovePod` + SVID unsubscribe for a DEL, `AddPod` + SVID subscribe for an
   ADD, `AddPod` for a rewritten record (the old agent's termination watch
   marking a pod Terminating). It is a takeover step, so it runs after the
   lock and before either socket binds, and it waits for the xDS server's own
   load from storage (`LocalListenersLoaded`), the view the diff is against.
   An uncontended start (free lock at process start, before storage is read)
   runs no takeover step at all.
2. **Probe cost.** The `agent-ready` exec probe is stdlib-only (`net`, no
   `net/http`), guarded by `//agent/cmd/agent-ready:deps_test` (the linked
   ELF's build info must list no module). Period stays 2 s.
3. **Old agent never exits.** The standby stays a standby, Ready (its
   `standby` readiness check passes once the first snapshot is complete), and
   binds nothing; the old pod's `terminationGracePeriodSeconds` bounds it, and
   the kernel releases the lock when the kubelet kills the process. A standby
   deleted before takeover exits without ever owning the node.
4. **`kubectl delete pod` without surge.** The lock is free at start, so the
   agent owns the node as soon as the manager starts. Nothing waits on the
   lock or runs a takeover step.

Found during implementation:

- **The registrar keyed watch streams by cluster/node.** Two agents on one
  node would replace each other's `WatchEndpoints` subscription (DataLoss,
  full resync) for the whole overlap. `WatchEndpointsRequest.instance`
  (the pod name) is now part of the key; empty keeps the old key, so an
  agent predating it behaves as before.
- **An agent that predates the lock.** The first surge roll onto this version
  replaces an agent that holds no lock. The standby therefore also waits until
  no server answers on `xds.sock`/`cni.sock` (a dial; a stale file from a
  killed agent answers ECONNREFUSED and does not count) before it binds.
- **The CNI conflist re-asserter** writes a node file too. A standby
  observes (readiness reads the chaining state) but repairs only once it owns
  the node, and re-checks at takeover.
- **QUIC pair fetch window** (#1033) restarts at takeover: it measures how
  long the agent has served, and a standby serves nothing.
- **"Complete" means the gates passed, not timed out.** A lone agent's first
  serve proceeds when the registry gate (15 s) or the capture gate (10 s)
  times out, which is right when nobody else serves the node. For a standby,
  proceeding would let the DaemonSet delete a healthy old agent and then
  publish local-only CDS/EDS over the proxy's working config (the #740
  clobber). The `standby` readiness therefore waits for
  `AgentXdsServer.StandbyComplete`: the first snapshot is built, the
  registry has actually loaded (initially or by the background retry), and
  the capture projection has landed. A standby that cannot reach the
  registrar stalls the roll, visibly. The client-certificate gate stays
  lenient: a timeout there costs a pod's QUIC twins, not the node's
  endpoints.
- **Takeover certificate wait.** The bind waits (≤1 s) only for the
  certificates of pods the takeover itself added. It compares the backlog
  with a baseline noted just before the reconcile, so one pod whose SVID
  never comes does not tax every takeover.
- **Known gap cost: an in-flight CNI ADD at SIGTERM.** The old agent's
  GracefulStop lets an in-flight ADD finish. Its best-effort ACK wait
  (`envoyAckTimeout`, 2 s) can then hold the old process, and with it the
  lock, for up to ~2 s, and that time adds to the gap. It is rare (an ADD
  has to be in flight at the moment of the roll's delete) and bounded.
- **Lock-less predecessor check** (`liveServer`) treats any dial error as "no
  server". It only matters for the first surge roll from a pre-lock agent,
  which the two-step rollout (chart 2.3.0 with `surge=false` first) avoids
  entirely.

## Rejected alternatives

- **Socket handoff by rename while both run** (the standby binds a temp path
  and renames it over `xds.sock`): the proxy keeps its stream to the old agent
  anyway until that agent exits, so it buys nothing over binding after the
  lock, and it opens the CNI ADD hazard above for the whole overlap.
- **Shorter pod start only.** Measured: even with no image checks and an
  idempotent init container, sandbox + init + container start is several
  seconds on talos.
