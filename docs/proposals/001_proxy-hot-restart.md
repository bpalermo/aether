# Proposal: Hot Restart for the aether-proxy Envoy (Spike)

**Status:** Spike — implementing Strategy A; **target is Strategy B**
**Author:** Bruno Palermo
**Date:** 2026-06-09

## Problem Statement

`aether-proxy` runs Envoy **directly as the container entrypoint** (`charts/agent/templates/proxy.yaml`), bootstrapped from a static ConfigMap (`charts/agent/templates/configmap.yaml`). All listeners, clusters, endpoints, and routes are delivered live over ADS through the shared `/run/aether/xds.sock`. Consequently:

- **Dynamic resources already update without a restart** — that is what xDS gives us. Hot restart buys nothing for them.
- **The gap is bootstrap-level change and Envoy binary/image upgrade without dropping connections.** The static `envoy.yaml` (admin, the `agent_xds` ADS cluster, HTTP/2 keepalive tuning, future stats/tracing sinks) and the Envoy version can only change today via a `RollingUpdate`, which tears down the pod, drops every in-flight connection on the node, and resets all stats.

Envoy's [hot restart](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart) replaces that with an epoch-to-epoch handoff of listen-socket FDs and stats, draining the old process gracefully. This is the Istio `pilot-agent` model: a supervisor owns the Envoy lifecycle and performs epoch-based restarts.

## The Constraint That Drives Everything

Hot restart hands off listen-socket FDs and stats from epoch *N* to *N+1* via:

1. A **shared-memory segment** keyed by `--base-id`, living in `/dev/shm`.
2. An **abstract** unix domain socket, also keyed by `--base-id`, which lives **in a network namespace** (no filesystem write — `readOnlyRootFilesystem: true` survives).

Both processes must therefore reach the same `/dev/shm` **and** the same network namespace at the same time. A default Kubernetes container-image upgrade deletes and recreates the Pod, giving the new process neither — no overlap, no shared memory. **Every strategy below is a different way to arrange that overlap.**

The current chart already helps: `aether-proxy` runs `hostNetwork: true`, so all proxy Pods on a node share the **host** network namespace — the abstract socket is mutually reachable. The remaining requirement is a shared `/dev/shm`.

## Common Building Block: The Supervisor

All three strategies share one component — a Go **hot-restart supervisor** (`agent/internal/proxy/hotrestart`, run via the `agent proxy-supervisor` subcommand) that reimplements Envoy's [`hot-restarter.py`](https://www.envoyproxy.io/docs/envoy/latest/operations/hot_restarter):

| Trigger | Action |
|---------|--------|
| start | fork/exec Envoy `--restart-epoch 0`, fixed `--base-id`, `--drain-time-s`, `--parent-shutdown-time-s` |
| **SIGHUP** / watched config change | hot restart: fork/exec epoch *N+1*; after `--parent-shutdown-time-s`, SIGTERM epoch *N* |
| **SIGTERM / SIGINT** | drain + terminate all children, exit 0 |
| **newest epoch exits unexpectedly** | terminate all, exit non-zero so Kubernetes recreates the pod |
| **SIGUSR1** | forward to current child (log reopen) |

Concurrency must stay constant across epochs (a decrease drops accept-queue connections).

---

## Strategy A — In-place Envoy upgrade (no Pod replacement) — **SPIKE TARGET**

Keep **one long-lived proxy Pod** whose supervisor is the stable entrypoint, and treat the **Envoy binary as data, not image**: the binary lives on a mutable volume. Upgrade = swap the binary + signal → supervisor forks epoch *N+1* **in the same container**, where `/dev/shm` and netns are trivially shared.

- **Pros:** simplest possible — textbook hot restart, no Kubernetes surgery, no surge scheduling.
- **Cons:** upgrades only the **Envoy binary + bootstrap config**, not the proxy container image (base-layer CVEs, supervisor changes still need a real roll).
- **Why we spike this first:** it is the minimal proof that the supervisor + hot-restart handoff actually works (epoch increments, zero dropped connections, stat continuity). The binary-delivery-without-pod-replacement mechanism (a host-local push of a versioned binary that the supervisor watches) is **production-A** and is *not* part of the spike — the spike triggers the handoff via a watched bootstrap-config change / SIGHUP, which exercises the identical code path.

### Spike packaging (A)

Gated behind `proxy.hotRestart.enabled` (off by default; the existing direct-Envoy path is unchanged):

- **The runtime container stays the Envoy image** (it carries Envoy *and its shared libraries*). The Envoy binary is dynamically linked, so copying it alone into a distroless image would not run — we inject the supervisor into the Envoy image instead of the reverse.
- An **initContainer runs the agent image and self-installs** the statically linked supervisor onto a shared `emptyDir` (`agent proxy-supervisor --install-path=/opt/aether/supervisor`).
- The runtime container's command becomes `/opt/aether/supervisor proxy-supervisor --envoy-path=/usr/local/bin/envoy --watch-config ...`; the Envoy service flags are passed through as `--envoy-arg`.
- A shared `emptyDir{medium: Memory}` mounted at `/dev/shm` carries the hot-restart shmem.
- The supervisor watches the bootstrap config dir (fsnotify, debounced); a `kubectl edit`/touch of the ConfigMap (or `kill -SIGHUP`) triggers the in-place hot restart.

The self-install seam is also what becomes "binary as data" in production-A: a host-local push of a new Envoy (or supervisor) onto the shared volume, followed by a watched-trigger restart.

---

## Strategy B — Cross-Pod hot restart (true image upgrade) — **TARGET**

Let the **new and old Pods overlap on the node** and share the hot-restart primitives during that window, using native Kubernetes primitives.

**Requirements:**
- **DaemonSet surge update:** `updateStrategy.rollingUpdate.maxSurge: 1, maxUnavailable: 0` (k8s ≥1.22) — new Pod created before old is torn down.
- **Shared `/dev/shm`:** a host path (host `/dev/shm` or a dedicated tmpfs) mounted at `/dev/shm` in both Pods.
- **Same `--base-id`** in both Pods.
- **hostNetwork: true** — already present; both Pods share the host netns where the abstract socket lives.
- **Epoch coordination across Pods** via a small state file on the shared hostPath (`/run/aether/hotrestart/epoch`).

**Upgrade sequence:**
```
node, before:   [old Pod: supervisor → envoy epoch 5]   serving, listeners bound into pod netns
1. DaemonSet roll (maxSurge=1) schedules [new Pod] on the same node; old keeps serving.
2. new supervisor reads epoch file = 5, starts envoy --restart-epoch 6 (same base-id, shared /dev/shm, host netns).
3. envoy(6) fully initializes (xDS config, health checks).
4. envoy(6) pulls listen-socket FDs + stats from envoy(5), starts listening, signals envoy(5) to drain.
5. new Pod becomes Ready (readiness gate fires only after handoff completes).
6. DaemonSet, seeing new Ready, deletes old Pod; old supervisor SIGTERMs envoy(5) after drain, exits 0.
node, after:    [new Pod: supervisor → envoy epoch 6]   no dropped connection, stats carried over
```

- **Pros:** real container-image upgrade, zero dropped connections, stat continuity, **native rollout, no standing extra cost**.
- **Cons / spike-must-prove:**
  - **Admin port handoff** — both Envoys want `127.0.0.1:9901` in host netns; Envoy passes the admin listener FD via the same transfer, but verify.
  - **Epoch edge case** — if the predecessor is gone (reboot/crash), the file says `5` but no epoch-5 process exists; starting at `6` hangs waiting for a parent. The supervisor must probe for a live predecessor and **reset to epoch 0** when absent.
  - **Readiness gating** — new Pod stays NotReady until handoff succeeds, so `maxUnavailable: 0` keeps the old Pod alive if the new one fails.
  - **maxSurge + hostNetwork co-scheduling** — confirm two proxy Pods co-schedule (admin is `127.0.0.1`, listeners are netns-bound, so no hostPort collision).

---

## Strategy C — Single Pod, dual-Envoy slots (in-place container patch)

One Pod runs `supervisor + envoy-a + envoy-b` as **blue/green slots**, ping-ponging on each upgrade. Patching one container's image makes the kubelet restart **only that container** (the pod sandbox, netns, and shared `/dev/shm` survive); you always patch the **standby** slot, which then hot-restarts from the active.

- **Cleanest hot-restart story:** all containers share the netns automatically, and a pod-level `emptyDir{medium: Memory}` at `/dev/shm` shares the segment — no hostPath, no surge scheduling, no `hostNetwork` requirement for the IPC.
- **But it opts out of the DaemonSet model:** a DaemonSet reconciles whole Pods, so bumping an Envoy image in the template recreates the entire Pod. To patch a single container in place you must run the DS as `updateStrategy: OnDelete` (paused) **and build a custom operator** that patches standby containers node-by-node and orchestrates the handoff. Strategy C is effectively "Strategy A inside the Pod **+ your own per-node upgrade controller**."
- **Lifecycle wrinkles:** Pod `restartPolicy: Always` auto-restarts the drained container (on its old image), so the two slots sit on different versions until each takes its turn; the supervisor must track which slot is active and ensure a freshly-restarted standby comes up *idle*. Native **sidecar containers** (stable ~1.29) help with ordering/lifecycle but add machinery.
- **Standing cost:** **2× Envoy footprint on every node, forever**, for a capability used only at upgrade time.

C is the end state once an Istio-grade proxy-upgrade operator is justified — not the first thing to build.

---

## Comparison

| | A: in-place binary | **B: surge two Pods (target)** | C: dual-Envoy Pod |
|---|---|---|---|
| Upgrades the **container image** | ❌ (Envoy binary only) | ✅ | ✅ |
| Shared `/dev/shm` mechanism | trivial (same container) | hostPath | emptyDir Memory (cleanest) |
| Standing resource cost | 1× | 1× (2× only mid-roll) | 2× always |
| Uses native rollout | ✅ | ✅ (`maxSurge`) | ❌ (custom operator + `OnDelete`) |
| Connection-preserving | ✅ | ✅ | ✅ |
| Spike order | **1st (proves handoff)** | **2nd (the goal)** | future |

## Plan

1. **Spike A** — supervisor as the proxy entrypoint, Envoy from a shared volume, shared `/dev/shm`; trigger via watched bootstrap-config change / SIGHUP. Prove the handoff on talos-main.
2. **Build toward B** — once A is GREEN, add the cross-Pod pieces: `maxSurge` DaemonSet strategy, hostPath `/dev/shm`, the `/run/aether/hotrestart/epoch` coordination file (with the live-predecessor probe → epoch-0 reset), and a readiness gate that fires only after handoff.
3. **C** — revisit only if B's transient surge or per-node behavior proves insufficient and an upgrade operator is warranted.

## Validation (talos-main)

- **Stats proof:** after a trigger, `server.hot_restart_generation` / `server.hot_restarts` increments and counters carry over (`curl :9901/stats`).
- **Zero-drop proof:** hold a long-lived streaming request through the mesh across a triggered restart; assert no reset. Re-run the mesh e2e (200 + XFCC URI SAN) against epoch *N+1*.
- **Binary-upgrade proof (A):** swap the Envoy binary on the shared volume and trigger; new epoch serves, old drains.
- **Image-upgrade proof (B):** roll the DaemonSet image with `maxSurge=1`; confirm overlap + handoff + zero drop.
- **Crash proof:** `kill -9` the child; supervisor exits non-zero and Kubernetes recreates the pod.

## Spike Findings (talos-main, 2026-06-09)

First on-cluster run (rev 29, `proxy.hotRestart.enabled=true` fleet-wide):

- Supervisor deploys cleanly as the proxy entrypoint via the self-install initContainer; Envoy comes up at **epoch 0**, `hot_restart_generation: 1`, 4 listeners, watching `/etc/envoy`.
- A ConfigMap edit propagated (~2 min) and the supervisor performed a hot restart: **epoch 0 → 1**, **`hot_restart_generation` 1 → 2**, listeners/sockets handed over, `server.live` stayed 1 — all in-place (`restartCount` was still 0 at that point).
- **Bug found:** ~70s later the *new* epoch crashed with `hot restart sendmsg() ... errno 111 (connection refused)` → `assert ... Aborted`, and the container restarted (epoch reset to 0). Root cause: the supervisor was externally SIGTERMing the old epoch at `parent-shutdown-time`, racing Envoy's own hot-restart IPC. **Fix:** remove the supervisor's parent-kill; Envoy terminates the old epoch itself via `--parent-shutdown-time-s` and the supervisor only reaps it. (Matches `hot-restarter.py`, which never signals the parent.)

## Risks / Open Questions

- **`/dev/shm` capacity** — container default is 64Mi tmpfs; Envoy's hot-restart shmem holds all gauges/counters. The spike sizes the memory `emptyDir` from measured usage.
- **hostNetwork + privileged FD handoff** — expected fine (Envoy passes the actual fd, no rebind); verify the netns-bound inbound listeners survive the handoff.
- **Distroless Envoy image has no shell** — the initContainer that copies the binary needs a shell-bearing image (non-distroless Envoy), or the binary is baked into a combined image.

## Out of Scope (for the spike)

Production binary-delivery for A, the Strategy C operator, agent→supervisor RPC triggering, coordination with CNI pod add/remove, and supervisor self-observability.

## Exit Criteria → Decision

**A is GREEN** if a triggered hot restart shows an incremented `hot_restart_generation`, zero dropped in-flight connections, and stat continuity on talos-main. That unblocks building **B**, whose own exit criterion is a connection-preserving DaemonSet image roll. Go/no-go on the whole effort: does bootstrap-change + image-upgrade continuity justify the supervisor (A) and the surge/coordination machinery (B)?
