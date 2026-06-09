# Proposal: Hot Restart for the aether-proxy Envoy (Spike)

**Status:** Spike (Proposed)
**Author:** Bruno Palermo
**Date:** 2026-06-09

## Problem Statement

`aether-proxy` runs Envoy **directly as the container entrypoint** (`charts/agent/templates/proxy.yaml`), bootstrapped from a static ConfigMap (`charts/agent/templates/configmap.yaml`). All listeners, clusters, endpoints, and routes are delivered live over ADS through the shared `/run/aether/xds.sock`. Consequently:

- **Dynamic resources already update without a restart** — that is what xDS gives us. Hot restart buys nothing for them.
- **The gap is bootstrap-level change and Envoy binary upgrade without dropping connections.** The static `envoy.yaml` (admin, the `agent_xds` ADS cluster, HTTP/2 keepalive tuning, future stats/tracing sinks) and the Envoy version can only change today via a `RollingUpdate`, which tears down the pod, drops every in-flight connection on the node, and resets all stats.

Envoy's [hot restart](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart) replaces that with an epoch-to-epoch handoff of listen-socket FDs and stats, draining the old process gracefully. This is the Istio `pilot-agent` model: a supervisor owns the Envoy lifecycle and performs epoch-based restarts.

## The Constraint That Drives the Design

Hot restart hands off listen-socket FDs and stats from epoch *N* to *N+1* via:

1. A **shared-memory segment** keyed by `--base-id`, living in `/dev/shm`.
2. An **abstract** unix domain socket, also keyed by base-id (no filesystem write — `readOnlyRootFilesystem: true` survives).

Both processes must therefore share `/dev/shm` and the IPC/PID namespace. The clean guarantee: **both Envoy epochs are children of one supervisor in one container.** Envoy is PID 1 today with no supervisor, so there is nothing to fork epoch *N+1*. **Introducing that supervisor is the central change.**

## Proposed Solution (Spike)

A Go hot-restart **supervisor** becomes the proxy container's entrypoint; Envoy becomes its managed child. We reimplement the logic of Envoy's [`hot-restarter.py`](https://www.envoyproxy.io/docs/envoy/latest/operations/hot_restarter) in Go, shipped as a new `agent proxy-supervisor` subcommand (reuses the existing build/image/release plumbing, logr setup, and the already-vendored `fsnotify`).

```
aether-proxy entrypoint:  agent proxy-supervisor --config /etc/envoy/envoy.yaml ...
                                   │
                                   ├─ fork/exec  envoy --restart-epoch 0 --base-id B ...   (epoch 0)
                                   └─ on trigger fork/exec envoy --restart-epoch 1 --base-id B ... (epoch 1)
                                          Envoy's own IPC transfers sockets+stats; epoch 0 drains; supervisor reaps it
```

The proxy keeps running as its **own DaemonSet/pod** — the agent pod is untouched; only the proxy container's `command` changes.

### Supervision model (replicating hot-restarter.py)

| Trigger | Action |
|---------|--------|
| start | fork/exec child Envoy with `--restart-epoch 0`, fixed `--base-id`, `--drain-time-s`, `--parent-shutdown-time-s` |
| **SIGHUP** | hot restart: fork/exec epoch *N+1*; after `--parent-shutdown-time-s`, SIGTERM epoch *N* |
| **SIGTERM / SIGINT** | drain + terminate all children, exit 0 |
| **child exits unexpectedly** (newest epoch) | terminate all, exit non-zero so k8s recreates the pod (the SIGCHLD-fatal behavior) |
| **SIGUSR1** | forward to the current child (log reopen) |

In Go we use a `cmd.Wait()` goroutine per child reporting to a channel rather than raw SIGCHLD reaping; the newest epoch dying is fatal, an old epoch finishing its drain is expected.

### Restart trigger

fsnotify watch on the mounted bootstrap file: a ConfigMap update → kubelet rewrites the mount symlink → supervisor self-sends SIGHUP. External SIGHUP is also honored (manual / future agent-driven trigger).

### Chart changes

- proxy container `command` → `agent proxy-supervisor ...` (Envoy flags moved into the supervisor's invocation).
- `/dev/shm` sizing — see Risks; likely an `emptyDir{medium: Memory, sizeLimit}` at `/dev/shm`.
- confirm abstract-socket IPC keeps `readOnlyRootFilesystem: true`.

## What the Spike Builds (~3–5 days, throwaway-quality acceptable)

1. `agent/internal/proxy/hotrestart` — the supervisor (started in this branch as a scaffold).
2. `agent proxy-supervisor` cobra subcommand wiring it into the binary.
3. Envoy invocation with `--restart-epoch`, `--base-id`, `--drain-time-s`, `--parent-shutdown-time-s`.
4. fsnotify config-file trigger + external SIGHUP.
5. Chart wiring (`/dev/shm`, command change).

## Validation (talos-main)

- **Stats proof:** after a trigger, `server.hot_restart_generation` / `server.hot_restarts` increments and counters carry over (`curl :9901/stats`).
- **Zero-drop proof:** hold a long-lived streaming request through the mesh across a triggered restart; assert no reset. Re-run the mesh e2e (200 + XFCC URI SAN) against epoch *N+1*.
- **Binary-upgrade proof:** swap the Envoy binary/layer and trigger; new epoch serves, old drains.
- **Crash proof:** `kill -9` the child; supervisor exits non-zero and k8s recreates the pod.

## Risks / Open Questions

- **`/dev/shm` capacity** — container default is 64Mi tmpfs; Envoy's hot-restart shmem holds all gauges/counters. Spike measures actual usage; add a sized memory `emptyDir` if needed.
- **hostNetwork + privileged FD handoff** — expected fine (Envoy passes the actual fd, no rebind); verify the netns-bound inbound listeners survive the handoff.
- **Concurrency/accept-queue** — decreasing `--concurrency` across epochs can drop queued accepts; keep concurrency constant across restarts.
- **Value vs. k8s rolling update** — an image upgrade *is* a pod replacement in k8s, so the realistic standalone trigger is a bootstrap-ConfigMap change. The spike must honestly assess whether that payoff justifies a supervised two-process container.

## Out of Scope (for the spike)

Production hardening, the cross-container/shared-namespace variant, agent→supervisor RPC triggering, coordination with CNI pod add/remove, and supervisor self-observability.

## Exit Criteria → Decision

**GREEN** if a triggered hot restart shows an incremented `hot_restart_generation`, zero dropped in-flight connections, and stat continuity on talos-main. Go/no-go: does bootstrap-change + binary-upgrade continuity justify making the proxy a supervised two-process container? If green, the follow-up is a real `pilot-agent`-style proxy manager.
