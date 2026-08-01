# Proposal 034: UDS Support for Pods

**Status:** Draft — Phase 1 (inbound) not started
**Author:** Bruno Palermo
**Date:** 2026-06-11, revised 2026-08-01 against current main
**History:** originally numbered 002; renumbered (002 is taken by the merged
agent-concurrency audit). The 2026-08-01 revision re-grounds every code
reference after two months of drift — port migration (030), multi-port
routing (005), redirect-all capture (022), the E/W waypoint (019), and the
extension-filter plumbing (025/#470) all landed since the first draft.

## Problem Statement

Mesh workloads today interact with the data plane exclusively over TCP
loopback inside their own network namespace:

- **Inbound delivery**: the per-pod, per-port `app_<pod>_<port>` STATIC
  cluster forwards decrypted traffic to `127.0.0.1:<port>`, dialed from the
  node proxy with `UpstreamBindConfig.SourceAddress.NetworkNamespaceFilepath`
  set to the pod's netns (`agent/internal/xds/proxy/cluster.go`,
  `NewAppCluster`). The health-probe cluster (`health_<pod>`, one per pod)
  does the same for delegated liveness.
- **Outbound entry**: managed pods are transparently captured by default
  (redirect-all, proposal 022) and may also address the explicit netns-bound
  outbound listener at `http://127.0.0.1:18081` (`mesh.ProxyOutboundPort`)
  with the destination in the `Host` header (`docs/workload-requirements.md`).

This excludes one class of workloads and leaves two sharp edges:

1. **Apps that serve on a Unix domain socket** (gRPC servers on sockets,
   FastCGI-style backends, sidecar-less internal daemons) cannot join the
   mesh at all — `endpoint.aether.io/port(s)` assumes TCP delivery.
2. **Apps that want an explicit, capture-free mesh entry** have only TCP
   loopback. gRPC clients support `unix://` targets natively; a socket file
   also carries filesystem permissions, whereas *any* process in the pod
   netns can hit `127.0.0.1:18081`. This matters most for pods that opt out
   of redirect-all (`capture.aether.io/redirect-all: "false"`).
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
Envoy `Pipe` addresses). Nothing in the tree touches `/var/lib/kubelet`
today — this is greenfield.

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
- **Sharing an emptyDir with the proxy** (the authz-sidecar pattern, #499):
  only works intra-pod. The proxy is a separate DaemonSet pod; it cannot
  mount an arbitrary workload's emptyDir. The chart's own rationale comment
  on the authz socket (`agent-proxy-daemonset.yaml`) documents the
  emptyDir-vs-hostPath tradeoff under `hostNetwork` — for cross-pod, the
  kubelet-volumes hostPath is the only bridge.
- **Status quo (TCP loopback only)**: excludes UDS-serving apps entirely.

### Why the hostPath is acceptable

`/var/lib/kubelet/pods` into the proxy container exposes all pods' volumes to
the proxy. The proxy DaemonSet is already `privileged: true`, `hostNetwork:
true`, `runAsUser: 0`, with the host netns dir (`/var/run/netns`) and
`/run/aether` mounted — this adds no meaningful privilege. The mount uses
`mountPropagation: HostToContainer` (already the chart's pattern for
`run-dir`/`netns-dir`) so `emptyDir` `medium: Memory` volumes — which are
tmpfs *mounts*, created after the proxy starts — propagate.

The mount must **not** be `readOnly`: `connect(2)` on an `AF_UNIX` socket
requires write permission on the socket inode, and a read-only mount fails
the connect with `EROFS` regardless of the caller being root. (The proxy's
`readOnlyRootFilesystem: true` is unaffected — that governs the container
rootfs, not this volume.)

Gate it behind `proxy.udsWorkloads.enabled` (default `true`) for operators
who want it off. Like `proxy.authzSidecar.enabled`, the value threads two
ways: the proxy DaemonSet mount and the agent's flags (the agent must know
whether rendering pipe addresses is allowed).

## Shared Plumbing (built once, in Phase 1)

1. **Pod UID on `CNIPod`** (`api/aether/cni/v1/cni.proto`, new field
   `string uid = 11` — verified still the next free number): required to
   resolve the kubelet volume path. The CNI server already fetches the Pod
   object on ADD and returns its UID transiently for SPIRE `k8s:pod-uid`
   selectors (`agent/internal/cni/server/pod.go`, `enhanceCNIPod`); today it
   is deliberately **not persisted**, which is why the SPIRE re-subscribe
   path re-`Get`s the Pod on agent restart
   (`agent/internal/cni/server/resubscribe.go`). Persisting it makes
   storage replay (`LoadListenersFromStorage`) self-sufficient for pipe-path
   resolution — delivery must not depend on API-server availability at boot.
   Backward-compatible proto addition; storage is protojson
   (`agent/storage/local.go`), old files simply lack the field.
2. **Annotation → host path resolver** (new `common/udspath` or
   `agent/internal/xds/proxy/udspath`): input `<volume-name>/<socket-file>`
   from the annotation, plus pod UID; output
   `<kubelet-pods-dir>/<uid>/volumes/kubernetes.io~empty-dir/<volume>/<file>`.
   - `--kubelet-pods-dir` agent flag, default `/var/lib/kubelet/pods`
     (distro-dependent; talos uses the default).
   - **Validation is security-critical**: both components must be single,
     clean path segments — reject empty, `/`, `.`, `..`, and anything that
     does not stay inside the pod's own volume dir after `filepath.Clean`.
     A malicious annotation must never address another pod's volume or an
     arbitrary host path.
3. **Chart** (`charts/aether`, umbrella): `hostPath /var/lib/kubelet/pods`
   mount on the proxy DaemonSet (same path inside the container,
   `HostToContainer`, not readOnly), gated by `proxy.udsWorkloads.enabled`;
   the same value adds the agent's `--kubelet-pods-dir`/enable flag.
   Chart.yaml version bump (CI-enforced).

## Phase 1 — Inbound: apps serving on UDS

**Annotation:** `endpoint.aether.io/uds-socket: <volume-name>/<socket-file>`
(constant in `common/constants/annotations`, next to `endpoint.aether.io/port`).

Presence switches app **delivery** to the socket. Unlike the first draft,
`endpoint.aether.io/port(s)` is **still required and still meaningful**:
since multi-port routing (005), the inbound listener demuxes per port and
delivery rides per-(pod, port) `app_<pod>_<port>` clusters, and registry/EDS
endpoints advertise `pod_ip:18008` per service port. The port keeps naming
the *service* port clients dial; the annotation only changes what the app
cluster dials. A UDS pod declares one socket; every declared port's
`app_<pod>_<port>` cluster dials that same pipe (protocol multiplexing on
one socket is the app's affair — normal for gRPC).

What changes (`agent/internal/xds/proxy/`):

- `NewAppCluster` / `NewAppHealthProbeCluster` gain a pipe variant: endpoint
  `Address_Pipe{Path: <resolved host path>}` and **no `UpstreamBindConfig`**
  (UDS is mount-ns-scoped; no netns bind exists or is needed).
- `GenerateListenersFromRegistryPod` (which now returns a *slice* of app
  clusters, one per port) picks TCP-vs-pipe per pod from the annotation.

What deliberately does **not** change:

- The **inbound mTLS listener** stays a netns-bound TCP listener on
  `0.0.0.0:18008` (`defaultInboundPort`, post-030); **registry/EDS endpoints
  stay `pod_ip:18008`**. Clients — on this node, other nodes, other clusters
  — are completely unaffected; a UDS pod is indistinguishable from a TCP pod
  from outside. No multi-cluster or rolling-upgrade compatibility concerns.
- The **E/W waypoint** (019) is covered for free: `ew_ingress_<fqdn>`
  clusters forward raw mTLS bytes to local pods at `:18008`, i.e. delivery
  still happens strictly behind the pod's own inbound listener and its
  `app_<pod>_<port>` clusters. Same for the SPIRE-off cleartext inbound
  variant (#421) — the delivery cluster is orthogonal to the inbound
  transport.
- Delegated liveness: the health gateway (itself already a `Pipe` listener
  on `/run/aether/health.sock`) still probes `/healthz/health_<pod>`; the
  `health_<pod>` cluster just dials the pipe. Active HTTP/1.1 health checks
  over pipe upstreams (existing `Host: localhost`) are supported Envoy
  behavior. For `endpoint.aether.io/protocol: tcp` pods the probe is already
  connect-only (`NewAppHealthProbeCluster`'s `tcp` variant) — on a pipe that
  degrades to "socket exists and accepts", which is the right analogue.
- CNI readiness probe (`/aether/readyz` on `127.0.0.1:18081`,
  `cni/internal/plugin/readyprobe.go`), SPIRE/SDS, hot restart: untouched.
- Unmanaged pods: the agent answers `RESULT_IGNORED` for pods without the
  `aether.io/managed` label and no listeners are generated (#484) — the
  annotation is inert there, like every `endpoint.aether.io/*` annotation.

Failure semantics match TCP: a not-yet-created socket file fails the health
probe exactly like a not-yet-listening port (connection refused → endpoint
stays unpromoted; the 15s warm-up grace in the CNI liveness loop applies to
`health-check-mode: active` pods — EDS-mode pods, the default, register
UNHEALTHY and need no grace).

Permissions: the proxy is `privileged`/root, so connecting is never blocked;
the *app* should create the socket `0600`–`0660` inside its own volume —
document in `workload-requirements.md`.

## Phase 1b — Service-scoped CRD attachment (GAMMA policy pattern)

Phase 1 delivers UDS through a pod annotation, which puts the whole contract in
the workload's `podTemplate`. Phase 1b adds a second, **service-scoped** way to
declare the same thing — a namespaced `EndpointPolicy` CR attached to a Service
with the Gateway API policy-attachment shape (GEP-713 direct attachment), the
same rails `HTTPFilter` rides for the proxy-extension escape hatch (025 M3).

### Why a CRD as well

- **Admission-time validation.** The `<volume>/<file>` value is subject to
  segment rules *and* a ~16-character budget (the `sun_path` cap minus kubelet's
  fixed prefix). As an annotation, a violation is discovered by the agent at
  listener-generation time and reported as an error log on one node, with the
  pod silently degraded to TCP. As a CR field, the controller's admission
  webhook rejects it at `kubectl apply`, where the author is standing.
- **Ownership split.** The annotation lives in the workload spec (app team);
  the CR is a separate namespaced object a platform owner can author, RBAC, and
  review independently of the Deployment — the same argument that made
  `HTTPFilter` a CR rather than a route annotation.
- **One object per service, not per workload.** A service with several
  Deployments (canary, per-region) declares delivery once.

### Shape

```yaml
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata:
  name: echo-uds
  namespace: aether-test
spec:
  targetRef:              # same-namespace, core group, kind Service only
    kind: Service
    name: echo
  udsSocket: s/app.sock   # same <volume>/<file> value as the annotation
```

`targetRef` is the shared `PolicyTargetRef` message `HTTPFilter` already uses.
Attachment is same-namespace **by construction** (the message carries no
namespace), matching `ServiceChainFilter`/`ServiceInboundFilter`. The target
name is matched against the mesh service key `<ns>/<svc>` (020 Part 1), whose
name component is the workload's ServiceAccount — the same resolution every
Service-attached policy in the tree uses.

### Precedence

The **pod annotation wins**. The CR is the service-level default; a pod that
carries `endpoint.aether.io/uds-socket` uses its own value and ignores the
policy. This is the "most specific wins / local wins" ethos the tree already
applies to imported-vs-local routes (026) and per-route-vs-chain filters (025).
Removing the CR reverts the affected pods to TCP delivery on the next
regeneration; removing the annotation falls back to the CR, if any.

At most one policy per service. Two `EndpointPolicy` objects targeting one
Service are a config error, not a merge: the lexicographically smallest policy
name wins and the conflict is logged, mirroring the deterministic tie-break in
`serviceScopedFilter`. Determinism matters because the losing value would
otherwise flip with map iteration order and churn the snapshot.

### The drift caveat

The socket carrier is **pod-spec data**: the `emptyDir` volume and the file the
app creates in it. A CR can name a volume the target service's pods do not
mount, and nothing in the CRD or the webhook can see that — the policy object
and the pod template are edited independently, and admission has no pod to
inspect. This degrades exactly like a bad annotation and no worse: the pipe
address resolves fine (it is a pure string computation over the pod UID), Envoy
dials a socket that does not exist, the delegated-liveness probe fails, and the
endpoint **stays unpromoted**. No traffic is sent anywhere it cannot be served;
there is no failure mode where a mis-scoped policy blackholes a healthy service.
Operators diagnose it the same way as the annotation: the health cluster never
promotes, and the agent log names the socket.

### Not exported cross-cluster

`EndpointPolicy` is **not** a class-1 payload for the 026 config plane and the
registrar's config-export controller never projects it. UDS delivery is
node-local by definition: the pipe address is only meaningful to the agent whose
proxy shares the kubelet pod-volumes hostPath with the pod. A peer cluster's
pods are reached over mTLS at `pod_ip:18008` and their delivery is their own
cluster's business — exactly the argument that keeps `SCOPE_INBOUND` filters
local (027 M3). Nothing about this CR is visible outside the cluster that owns
the pods.

### Implementation

| Piece | Where |
|---|---|
| `EndpointPolicySpec` proto (edition 2024, IMPLICIT presence, buf-validate rules) | `api/aether/config/v1/endpoint_policy.proto` |
| CRD Go type + deepcopy + protojson shim | `common/apis/config/v1/endpointpolicy_*.go` |
| CRD manifest (typed OpenAPI schema mirroring the proto rules) | `charts/crds/templates/endpointpolicy.yaml` |
| Admission validator (segment rules + `sun_path` budget via `udspath.Resolve` with a worst-case 36-byte UID placeholder; targetRef checks) | `controller/internal/endpointpolicy/` |
| CRD-presence-gated agent reconciler → `map["<ns>/<svc>"]socket` | `agent/internal/endpointpolicy/` |
| `SetUDSServicePolicies` + annotation-first resolution + per-pod delivery-cluster regeneration | `agent/internal/xds/cache/` |

The webhook's budget check assumes the default `--kubelet-pods-dir`; a
nonstandard directory shifts the budget in either direction, so the agent still
fail-closes at resolution time (falling back to TCP) rather than trusting
admission.

## Phase 2 — Outbound: apps dialing the mesh via UDS

**Annotation:** `egress.aether.io/socket: <volume-name>/<socket-file>` (a new
annotation prefix; `capture.aether.io/` is the precedent for a
behavior-scoped one).

Honest re-assessment of value since the first draft: with redirect-all ON by
default (022) and mesh DNS answering `<svc>.<ns>.aether.internal`, most apps
never address the data plane explicitly, so this phase's audience narrowed
to (a) apps that opt out of capture and want a permissioned explicit entry,
and (b) `unix://`-native gRPC clients. Phase 2 stays in the proposal but its
priority is lower than in June; ship Phase 1, then re-evaluate demand before
building it.

The agent generates one **additional** per-pod listener (`outbound_uds_<pod>`):

- `Address_Pipe{Path: <resolved host path>, Mode: 0666}` (`Pipe.mode` sets
  the socket file permissions so the unprivileged app user can connect).
- Same outbound HTTP filter chain as `outbound_http_<pod>` (Host-header
  service demux, readiness `health_check` filter, and the extension-filter
  union threading from 025/#470) — parameterize the address in the
  now-exported `GenerateOutboundHTTPListener`. Callers pass the
  once-per-loop extension union (#615 hoisted it; keep that shape).
- **Additive**: the netns-bound `127.0.0.1:18081` listener remains; apps may
  use either. The CNI readiness probe keeps targeting the TCP listener (a
  pod's data-plane readiness must not depend on the optional UDS path).
- ACK-tracking works unchanged (it keys on listener names).

Apps then call `unix:///<in-pod-volume-mount>/<socket-file>` with the same
`Host`-header convention — for gRPC, a native channel target.

### Phase 2 spike checklist (before committing the implementation)

- [ ] Pipe listener FD inheritance across **hot restart** (in-pod and
      cross-pod under the supervisor): Envoy matches inherited listen
      sockets by address string — verify pipes are passed, and verify
      stale-socket-file handling on non-inherited (crash/SIGKILL) restarts
      (Envoy unlinks before bind — confirm with the hostPath mount; the
      proxy's `readOnlyRootFilesystem` does not cover the volume).
- [ ] Socket file created by Envoy (root) is connectable by an arbitrary
      app UID with `Mode: 0666`; directory ownership of the emptyDir doesn't
      block traversal for the app.
- [ ] Listener teardown on CNI DEL removes the socket file (or document the
      kubelet GC of the emptyDir as sufficient).

## Implementation Plan (PRs)

| PR | Scope | Contents |
|----|-------|----------|
| 1 | shared plumbing | `CNIPod.uid = 11` + CNI-server population + storage persistence; annotation constants; `udspath` resolver with traversal-rejecting validation + unit tests |
| 2 | Phase 1 | pipe variants of `app_<pod>_<port>`/`health_<pod>` clusters; per-pod selection in `GenerateListenersFromRegistryPod`; proxy DS chart mount + `proxy.udsWorkloads.enabled` + agent flag + Chart.yaml bump; `workload-requirements.md` section |
| 2b | Phase 1b | `EndpointPolicy` proto + CRD + admission webhook; CRD-gated agent reconciler; annotation-over-CR precedence in the cache; chart RBAC/webhook rules + Chart.yaml bumps |
| 3 | Phase 1 e2e | UDS-serving test workload (small Go HTTP server on a socket) + talos-main validation: join, traffic (incl. name-based path and cross-node), delegated-liveness promotion, rolling restart, agent restart (storage replay with persisted UID, API server unreachable) |
| 4 | Phase 2 spike | checklist above on talos-main |
| 5 | Phase 2 | `outbound_uds_<pod>` listener + `GenerateOutboundHTTPListener` address parameterization; docs; talos e2e with a `unix://` gRPC client |

## Risks / Open Questions

- **kubelet pods dir location** is distro-dependent → flag (default fits
  talos and stock kubeadm).
- **Multi-port UDS pods**: all declared ports dial the same socket. If a
  real workload needs per-port sockets, the annotation grows a map form
  (`<port>:<vol>/<file>,...`) — deferred until demanded.
- **Socket-at-subPath mounts** (`subPath` on the emptyDir in the workload)
  change the host path shape — out of scope; require a plain volume mount.
- **CSI/projected volumes** as socket carriers — out of scope; `emptyDir`
  only (the `kubernetes.io~empty-dir` segment is part of the validated
  contract).
- **Per-request latency**: pipe upstreams skip TCP overhead; no regression
  expected, but capture a before/after in the Phase 1 e2e.
