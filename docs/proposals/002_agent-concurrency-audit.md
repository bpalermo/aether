# Agent Concurrency Audit: Config Propagation, Hot Restart, Pod Lifecycle

**Status:** All findings fixed — R1+R3 in #109, R2+R4-R8 in #110, follow-on predecessor re-probe in #111
**Author:** Bruno Palermo
**Date:** 2026-06-10

## Context

After validating proxy hot restart (Strategies A and B, `docs/proposals/001`) with a
12,000-request zero-drop e2e across three fleet restarts on talos-main, this audit
walks the agent code paths that run concurrently — xDS snapshot generation, the
registry refresher, the SPIRE bridge, the CNI pod lifecycle, the liveness loop, and
the hot-restart supervisor — looking for races. Findings are ordered by severity.
None were observed to fire during the e2e; all are inferred from code inspection.

---

## R1 (HIGH) — Snapshot lost-update race

**Where:** `agent/internal/xds/cache/snapshot.go` (`generateSnapshot`)

`generateSnapshot` is invoked concurrently from CNI `AddPod`/`RemovePod`, the
registry refresher (`LoadClustersFromRegistry`), the SPIRE bridge (every SVID/bundle
rotation via `SetSecrets`), and `SetNodeIdentity`. Each caller mutates its map under
a per-type lock, then: ① computes a version from an atomic counter, ② reads the maps
under sequentially-taken RLocks, ③ calls `SetSnapshot`.

Nothing serializes ①–③ across callers. Interleaving:

```
A: mutate X      → version 10 → read maps (has X)
B: mutate Y      → version 11 → read maps (has X+Y) → SetSnapshot(v11)
A:                                                  → SetSnapshot(v10)   ← stale wins
```

Envoy sees a version *change* (11→10) and applies the **stale** snapshot; B's
mutation (a removed listener, a fresh SVID) is silently dropped until the next
trigger regenerates. With SPIRE rotations the gap self-heals in ~30s, but in a quiet
period a removed pod's listener can stay resurrected or a rotated secret missing
indefinitely.

**Fix:** one `snapshotMu sync.Mutex` held across version-gen + map reads +
`SetSnapshot`. Everything is in-memory; the serialization cost is negligible and it
makes version order equal content order.

## R2 (HIGH, data race) — In-place proto mutation aliased into live snapshots

**Where:** `agent/internal/xds/cache/cluster.go` (`RemoveEndpoint` vs
`clustersEndpointsAndVhosts`)

`clustersEndpointsAndVhosts` clones each *cluster* before injecting mTLS, but
appends `entry.loadAssignment` **un-cloned** into snapshots. `RemoveEndpoint` later
mutates that same proto in place (`entry.loadAssignment.Endpoints = endpoints`)
under `clusterMu` — a lock the xDS server goroutines marshaling the previous
snapshot do not hold. Concurrent proto read/write: torn marshal, `-race` violation.

**Fix:** build a fresh `ClusterLoadAssignment` in `RemoveEndpoint` instead of
mutating, or clone load assignments at snapshot time like the clusters.

## R3 (MEDIUM) — Pod added during startup loses its mTLS identity mapping

**Where:** `agent/internal/xds/cache/listener.go` (`LoadListenersFromStorage`) vs
`agent/internal/cni/server/pod.go` (`AddPod`)

The xDS server's `PreListen` and the CNI server run as concurrent manager
runnables. `LoadListenersFromStorage` builds `local` from a storage `GetAll`, then
**wholesale-replaces** `c.localWorkloads`. An `AddPod` whose `setLocalWorkload`
lands between the `GetAll` and the replacement is wiped from the map: that pod's
outbound connections present the node certificate (transport-socket on-no-match)
instead of its workload SVID until the pod is re-added. Narrow startup-only window.

**Fix:** merge into `localWorkloads` (add/update only) instead of replacing — at
startup the map is empty anyway, so replacement semantics buy nothing.

## R4 (MEDIUM) — Ghost endpoint resurrection: liveness loop vs RemovePod

**Where:** `agent/internal/cni/server/liveness.go` (`reconcileLiveness`) vs
`pod.go` (`RemovePod`)

`RemovePod` unregisters the endpoint **first** and removes the pod from storage
**last**. The 5s liveness tick can `GetAll` in between, observe a health
transition (likely during proxy restarts, when active health checks reset), and
`RegisterEndpoint` — re-registering the just-unregistered endpoint of a deleted
pod. Nothing unregisters it again: a **permanent ghost endpoint** in the registry,
fanned out to every agent. Active HC keeps it out of load balancing, but it bloats
EDS and health-check traffic forever.

**Fix:** remove from storage *before* unregistering in `RemovePod` (CNI DEL is
idempotent), so liveness can no longer see the pod once unregistration starts.

## R5 (LOW, race detector) — `Bridge.client`/`Bridge.ctx` unsynchronized

**Where:** `agent/internal/spire/bridge.go`

`Start` writes `b.ctx` and `b.client` from the manager goroutine;
`SubscribePod`/`UnsubscribePod` read them from CNI goroutines with a bare
`if b.client == nil` check. Practically benign on amd64/arm64, but a genuine data
race `-race` CI would flag. The `Started()` channel (added for SVID resubscription)
already provides the needed happens-before.

**Fix:** gate `SubscribePod`/`UnsubscribePod` on a non-blocking `<-b.started`
select instead of the nil check.

## R6 (LOW) — Resubscribe vs concurrent pod removal: leaked subscription

**Where:** `agent/internal/cni/server/resubscribe.go`

If a pod is removed (CNI DEL: `UnsubscribePod` + storage removal) between the
resubscriber's `GetAll` and its `SubscribePod`, the deleted pod's netns is
re-subscribed and nothing ever unsubscribes it — a leaked SPIRE subscription and a
secret held for the agent's lifetime.

**Fix:** after `SubscribePod`, re-check the pod still exists in storage;
unsubscribe if gone.

## R7 (LOW, hardening) — Rapid hot-restart triggers can kill the proxy pod

**Where:** `agent/internal/proxy/hotrestart/supervisor.go`

The 500ms debounce collapses bursts, but two triggers spaced wider than the
debounce while epoch N is still initializing fork N+1, which exits 1
(`previous envoy process is still initializing`) → newest-epoch death → supervisor
exits non-zero → container restart (brief node data-plane gap).

**Fix:** gate `hotRestart()` on `adminLiveAtEpoch(currentEpoch())`; if the current
epoch is not yet LIVE, re-arm the debounce instead of forking.

## R8 (LOW, narrow) — Epoch state-file write race

**Where:** `agent/internal/proxy/hotrestart/coordination.go` (`writeState`)

`writeState` is read-check-write with no file lock. During a surge overlap the
draining pod's heartbeat (epoch N) can interleave with the successor's first write
(N+1) so the file briefly regresses to N. A third pod starting inside that ~1s
window picks startEpoch N+1 and collides with the live N+1 (base-id bind error →
crashloop until the next heartbeat). Requires a triple pod overlap on one node —
effectively only pathological rolls.

**Fix (optional):** an `O_EXCL` lock file around the read-modify-write, or accept
it given DaemonSet semantics (≤2 pods per node).

---

## Checked and found sound

- Supervisor `readyGate` (written before the goroutine that reads it), `children`/
  `nextEpoch` under `mu`, `childExited`/`done` channel discipline.
- Registry refresher debounce (correct `Stop`/drain/`Reset` pattern).
- `watchConfig` trigger channel (cap-1 + downstream debounce).
- `UnsubscribePod` shared-SPIFFE-ID refcounting (keyed by netns, secret retained
  while referenced).
- Storage fsnotify (internal cache only; no callbacks into the snapshot cache).
- go-control-plane `SnapshotCache.SetSnapshot` itself (internally locked).

## Suggested order of work

R1 and R2 are cheap, high-value fixes (one mutex; one clone) and belong in a single
PR. R3+R4 are small behavioral fixes to the pod lifecycle. R5–R8 are hardening that
can ride along or wait.
