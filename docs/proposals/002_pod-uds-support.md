# Proposal: UDS Support for Pods

**Status:** Draft — Phase 1 (inbound) not started
**Author:** Bruno Palermo
**Date:** 2026-06-11

## Problem Statement

Mesh workloads today interact with the data plane exclusively over TCP loopback
inside their own network namespace:

- **Inbound delivery**: the per-pod `app_<pod>` STATIC cluster forwards
  decrypted traffic to `127.0.0.1:<endpoint.aether.io/port>`, dialed from the
  node proxy with `UpstreamBindConfig.NetworkNamespaceFilepath` set to the
  pod's netns (`agent/internal/xds/proxy/cluster.go`). The health-probe
  cluster (`health_<pod>`) does the same for delegated liveness.
- **Outbound entry**: apps call `http://127.0.0.1:18081` (the netns-bound
  per-pod outbound listener) with the destination service in the `Host`
  header (`agent/internal/xds/proxy/listener.go`).

This excludes two classes of workloads and leaves one sharp edge:

1. **Apps that serve on a Unix domain socket** (gRPC servers on sockets,
   FastCGI-style backends, sidecar-less internal daemons) cannot join the
   mesh at all — `endpoint.aether.io/port` assumes a TCP port.
2. **Apps that want an explicit, capture-free mesh entry** have only TCP
   loopback. gRPC clients support `unix://` targets natively; a socket file
   also carries filesystem permissions, whereas *any* process in the pod
   netns can hit `127.0.0.1:18081`.
3. The only cleartext hop in the mesh (proxy → app on loopback) rides TCP;
   moving it to a UDS narrows who can observe/connect to it.

## The Constraint That Drives Everything

**Pathname Unix sockets live in the mount namespace; the node proxy lives in
the host mount namespace.** Unlike the TCP paths above — which Envoy reaches
via `NetworkNamespaceFilepath` netns binding — a socket *file* inside the
pod's overlayfs is invisible to the proxy.

The bridge is kubelet's pod-volumes directory on the host. An `emptyDir`
volume declared by the pod materializes at:

```
/var/lib/kubelet/pods/<pod-UID>/volumes/kubernetes.io~empty-dir/<volume-name>/
```

A socket created in that volume by the app (Phase 1) or by Envoy (Phase 2) is
reachable from both sides: the pod mounts the volume normally; the proxy
DaemonSet mounts `/var/lib/kubelet/pods` as a `hostPath` (at the **identical
container path**, so no prefix translation is needed when the agent renders
Envoy `Pipe` addresses).

### Rejected alternatives

- **Abstract-namespace sockets** (`@`-prefixed, netns-scoped — no volume
  needed): Envoy can *bind listeners* inside a pod netns via
  `NetworkNamespaceFilepath`, but that knob exists for `SocketAddress` only;
  upstream pipe connects happen in Envoy's own netns with no setns support.
  Dead end for inbound; untested/unsupported for outbound listeners. Not
  pursued.
- **Per-pod `hostPath` mounted into the workload**: requires the *workload*
  spec to mount a hostPath — blocked under PSS `restricted`, and a privilege
  footgun. `emptyDir` keeps workloads unprivileged.
- **Status quo (TCP loopback only)**: excludes UDS-serving apps entirely.

### Why the hostPath is acceptable

`/var/lib/kubelet/pods` into the proxy container exposes all pods' volumes to
the proxy. The proxy DaemonSet is already `privileged: true`, `hostNetwork:
true`, with the host netns dir and `/run/aether` mounted — this adds no
meaningful privilege. The mount uses `mountPropagation: HostToContainer`
(already the chart's pattern for `run-dir`/`netns-dir`) so `emptyDir`
`medium: Memory` volumes — which are tmpfs *mounts*, created after the proxy
starts — propagate. Gate it behind `proxy.udsWorkloads.enabled` (default
`true`) for operators who want it off.

## Shared Plumbing (built once, in Phase 1)

1. **Pod UID on `CNIPod`** (`api/aether/cni/v1/cni.proto`, new field
   `string uid = 11`): required to resolve the kubelet volume path. The CNI
   server already fetches the Pod object on ADD (SPIRE `k8s:pod-uid`
   selectors, topology); persist the UID in storage so resolution survives
   agent restarts. Backward-compatible proto addition.
2. **Annotation → host path resolver** (new `agent/internal/xds/proxy/udspath`
   or `common/udspath`): input `<volume-name>/<socket-file>` from the
   annotation, plus pod UID; output
   `<kubelet-pods-dir>/<uid>/volumes/kubernetes.io~empty-dir/<volume>/<file>`.
   - `--kubelet-pods-dir` agent flag, default `/var/lib/kubelet/pods`
     (distro-dependent; talos uses the default).
   - **Validation is security-critical**: both components must be single,
     clean path segments — reject empty, `/`, `.`, `..`, and anything that
     does not stay inside the pod's own volume dir after `filepath.Clean`.
     A malicious annotation must never address another pod's volume or an
     arbitrary host path.
3. **Chart**: `hostPath /var/lib/kubelet/pods` mount on the proxy DaemonSet
   (same path inside the container, `HostToContainer`), gated by
   `proxy.udsWorkloads.enabled`.

## Phase 1 — Inbound: apps serving on UDS

**Annotation:** `endpoint.aether.io/uds-socket: <volume-name>/<socket-file>`
(presence switches the app delivery to UDS; `endpoint.aether.io/port` is
ignored for delivery and liveness).

What changes (`agent/internal/xds/proxy/`):

- `NewAppCluster` / `NewAppHealthProbeCluster` gain a pipe variant: endpoint
  `Address_Pipe{Path: <resolved host path>}` and **no `UpstreamBindConfig`**
  (UDS is mount-ns-scoped; no netns bind exists or is needed).
- `GenerateListenersFromRegistryPod` picks TCP-vs-pipe per pod from the
  annotation.

What deliberately does **not** change:

- The **inbound mTLS listener** stays a netns-bound `<pod_ip>:15008` TCP
  listener; **registry/EDS endpoints stay `pod_ip:15008`**. Clients — on this
  node, other nodes, other clusters — are completely unaffected; a UDS pod is
  indistinguishable from a TCP pod from outside. No multi-cluster or
  rolling-upgrade compatibility concerns.
- Delegated liveness: the health gateway still probes
  `/healthz/health_<pod>`; the `health_<pod>` cluster just dials the pipe.
  Active HTTP/1.1 health checks over pipe upstreams (with the existing
  `Host: localhost`) are supported Envoy behavior — same machinery as the
  health gateway listener itself (`healthgateway.go` already uses a pipe).
- CNI readiness probe (`/aether/readyz` on the outbound listener), SPIRE/SDS,
  hot restart: untouched.

Failure semantics match TCP: a not-yet-created socket file fails the health
probe exactly like a not-yet-listening port (connection refused → endpoint
stays unpromoted; the existing 15s active-mode warm-up grace applies). If the
app recreates the socket on restart, new connections re-resolve the path.

Permissions: the proxy is `privileged`/root, so connecting is never blocked;
the *app* should create the socket `0600`–`0660` inside its own volume —
document in `workload-requirements.md`.

## Phase 2 — Outbound: apps dialing the mesh via UDS

**Annotation:** `egress.aether.io/socket: <volume-name>/<socket-file>`.

The agent generates one **additional** per-pod listener (`outbound_uds_<pod>`):

- `Address_Pipe{Path: <resolved host path>, Mode: 0666}` (`Pipe.mode` sets
  the socket file permissions so the unprivileged app user can connect).
- Same outbound HTTP filter chain as `outbound_http_<pod>` (Host-header
  service demux, readiness `health_check` filter included) — refactor
  `generateOutboundHTTPListener` to parameterize the address.
- **Additive**: the netns-bound `127.0.0.1:18081` listener remains; apps may
  use either. The CNI readiness probe keeps targeting the TCP listener (a
  pod's data-plane readiness must not depend on the optional UDS path).
- ACK-tracking works unchanged (it keys on listener names).

Apps then call `unix:///<in-pod-volume-mount>/<socket-file>` with the same
`Host`-header convention — for gRPC, a native channel target.

### Phase 2 spike checklist (before committing the implementation)

- [ ] Pipe listener FD inheritance across **hot restart** (in-pod and
      cross-pod): Envoy matches inherited listen sockets by address string —
      verify pipes are passed, and verify stale-socket-file handling on
      non-inherited (crash/SIGKILL) restarts (Envoy unlinks before bind —
      confirm under `readOnlyRootFilesystem` with the hostPath mount).
- [ ] Socket file created by Envoy (root) is connectable by an arbitrary
      app UID with `Mode: 0666`; directory ownership of the emptyDir doesn't
      block traversal for the app.
- [ ] Listener teardown on CNI DEL removes the socket file (or document the
      kubelet GC of the emptyDir as sufficient).

## Implementation Plan (PRs)

| PR | Scope | Contents |
|----|-------|----------|
| 1 | shared plumbing | `CNIPod.uid` proto field + CNI-server population + storage; annotation constants; `udspath` resolver with traversal-rejecting validation + unit tests |
| 2 | Phase 1 | pipe variants of `app_`/`health_` clusters; per-pod selection in `GenerateListenersFromRegistryPod`; proxy DS chart mount + `proxy.udsWorkloads.enabled`; `workload-requirements.md` section |
| 3 | Phase 1 e2e | UDS-serving test workload (small Go HTTP server on a socket) + talos-main validation: join, traffic, delegated-liveness promotion, rolling restart, agent restart (storage replay with UID) |
| 4 | Phase 2 spike | checklist above on talos-main |
| 5 | Phase 2 | `outbound_uds_<pod>` listener + builder refactor; docs; talos e2e with a `unix://` gRPC client |

## Risks / Open Questions

- **kubelet pods dir location** is distro-dependent → flag (default fits
  talos and stock kubeadm).
- **Socket-at-subPath mounts** (`subPath` on the emptyDir in the workload)
  change the host path shape — out of scope; require a plain volume mount.
- **CSI/projected volumes** as socket carriers — out of scope; `emptyDir`
  only (the `kubernetes.io~empty-dir` segment is part of the validated
  contract).
- **Per-request latency**: pipe upstreams skip TCP overhead; no regression
  expected, but capture a before/after in the Phase 1 e2e.
