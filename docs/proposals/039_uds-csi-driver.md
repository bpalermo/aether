# Proposal 039: A CSI Driver as the UDS Carrier

**Status:** Draft. Recommends building it, **after Phase 0 settles one question**,
as a **breaking cut-over with no carrier coexistence** (decision 2026-09-27: a release
of dual paths is dearer than migrating the two known UDS workloads in the same upgrade).
Phase 0 has to confirm or refute a symlink confused-deputy in the shipped 034 design
(see *The finding that decides it*). If the attack is confirmed, this proposal is the
structural fix and goes ahead. If it is refuted, the remaining benefits do not justify
a new privileged node component at today's UDS demand, and the proposal is parked. The
bar is written down in advance so the decision is not re-argued once the result is in.
**Author:** Bruno Palermo
**Date:** 2026-09-26
**Related:** 034 (UDS delivery: annotation, `EndpointPolicy`, the `emptyDir`-only
rule, shipped #616–#620), 033 (node-taint lifecycle), 036 (SPIFFE Broker API — the
node agent already leans on a CSI-delivered socket), #583 / #772 (the slim-binary /
own-DaemonSet split pattern this proposal reuses).

## Problem Statement

Proposal 034 delivers inbound traffic to a pod that serves on a Unix socket like this.
The pod declares an `emptyDir`, and the app binds `<volume>/<file>` inside it. The
node proxy is a host-network DaemonSet in the host mount namespace, and it reaches the
socket through kubelet's pod-volumes directory:

```
/var/lib/kubelet/pods/<pod-UID>/volumes/kubernetes.io~empty-dir/<volume>/<file>
```

`common/udspath.Resolve` computes that path. The agent renders it into the
`app_<pod>_<port>` / `health_<pod>` clusters as an Envoy `Pipe` address
(`agent/internal/xds/cache/listener.go`, `udsSocketPathForPod`). The proxy DaemonSet
mounts `/var/lib/kubelet/pods` read-write with `HostToContainer` propagation
(`charts/aether/templates/agent-proxy-daemonset.yaml`, gated by
`proxy.udsWorkloads.enabled`, **default true**).

This works, and it held up under churn in the 2026-08-03 soak. It has five
properties that are worth revisiting:

1. **The proxy can see every volume of every pod on the node.** That includes
   `Secret` and projected-token volumes: they are tmpfs mounts under the same tree,
   and `HostToContainer` propagates them. 034 accepted this because the proxy is
   already privileged. That argument is about *privilege*. The *blast radius* of a
   bug in the proxy's own path handling is a separate question, and the next section
   shows that it is not hypothetical.
2. **The kubelet root is hard-coded in three places, and only one of them has a
   knob.** The agent has `--kubelet-pods-dir`. The chart mounts the literal
   `/var/lib/kubelet/pods` with `type: Directory`, and it has **no value to change
   it**: the agent DaemonSet only ever passes `--kubelet-pods-dir=` to *disable*
   delivery. The `EndpointPolicy` webhook's budget check assumes the default. A
   distro that moved kubelet's root (k0s `/var/lib/k0s/kubelet`, microk8s
   `/var/snap/microk8s/common/var/lib/kubelet`) cannot use UDS delivery without
   editing the chart.
3. **The socket-name budget is about 16 characters for `<volume>/<file>` together.**
   The fixed prefix is 91 of the 107 bytes `sun_path` allows. The budget also depends
   on the kubelet root, so admission can only approximate it (`controller/internal/
   endpointpolicy/` uses a placeholder UID and the default directory).
4. **Every failure is silent.** A missing volume, a `subPath` mount, a CSI or
   projected volume, a wrong kubelet root, or `udsWorkloads.enabled: false` each
   degrade the pod to TCP delivery. Nothing listens on TCP, so the endpoint stays
   unpromoted, and an error appears in one node's agent log. The pod itself is
   Running and Ready.
5. **aether owns neither the directory nor the filesystem the socket lives on.**
   The kubelet creates the `emptyDir`. For the default medium it is a plain directory
   on the kubelet root filesystem. For `medium: Memory` it is a kubelet-created tmpfs.
   aether cannot choose its mount flags, its size, or its ownership.

Property 5 is what makes a CSI driver more than tidying. The only way for aether to
own the filesystem a workload socket lives on, without the workload declaring a
`hostPath` (which `baseline` forbids), is for aether to be the volume plugin.

## The finding that decides it

> **Unverified.** This finding is mechanically reasoned from kernel and code
> behaviour, not reproduced. Phase 0 exists to reproduce it on kind. It should be
> filed as its own issue, not folded into this proposal's PRs.

`connect(2)` on an `AF_UNIX` pathname follows symlinks: `unix_find_other` resolves
`sun_path` with `LOOKUP_FOLLOW`. The resolution happens in the **caller's** mount
namespace, and the caller here is Envoy in the proxy container. The last path
component, `<file>`, is created by the app, so it is **app-controlled**. An app that
replaces `app.sock` with a symlink makes the proxy connect, as root, to whatever the
link names *in the proxy container's view*. That view includes:

| Path in the proxy container | What answers | Why it matters |
|---|---|---|
| `/run/aether/cni.sock` | the agent's CNI gRPC server (`AddPod`/`RemovePod`/…), no caller authentication (protovalidate only) | a tenant could deregister other pods on its node |
| `/run/aether/xds.sock` | the agent's xDS server (`os.Chmod(…, ModePerm)`, `common/xds/server.go`) | read the node's full Envoy config, including every local pod's clusters |
| `/run/secrets/workload-spiffe-uds/socket` | the SPIRE Workload API (csi.spiffe.io) | attests the *connecting PID*, which is Envoy, so this could hand the attacker the proxy pod's SVID |
| `/var/lib/kubelet/pods/<other-uid>/…/<sock>` | another UDS workload | bypasses the victim's inbound mTLS/RBAC; traffic arrives as trusted local delivery |

The attack needs nothing beyond what any pod author in a mesh namespace already has:
the `endpoint.aether.io/uds-socket` annotation (or an `EndpointPolicy`, which does
not help because the *file* is still the app's), plus `endpoint.aether.io/protocol:
grpc` so that `NewAppCluster` speaks h2c. The attacker then calls its **own** service
through the mesh. Its inbound listener forwards the request down its own app cluster,
and the pipe connect follows the link. grpc-go accepts prior-knowledge h2c. The
headers Envoy adds (XFCC, `x-request-id`) are metadata that the servers above ignore.

**No check in the agent closes this.** An `lstat` before rendering is a TOCTOU race,
and the window is unbounded because Envoy re-connects on every new upstream
connection. Envoy's `Pipe` has no no-follow option. `/proc/<pid>/root/…` does not
confine anything: absolute symlinks and `..` escape to the caller's root, which is
the whole reason `openat2(RESOLVE_IN_ROOT)` exists.

**What does close it is a mount flag.** `MS_NOSYMFOLLOW` (Linux ≥ 5.10; talos runs
6.18) makes path resolution return `ELOOP` for any symlink that *resides on* that
mount, and that includes the final component of a `connect(2)`. It is a property of
the filesystem the socket lives on, and today aether does not own that filesystem.

A **partial, CSI-free interim** exists, and Phase 0 should ship it if the repro
lands. The proxy supervisor is privileged and PID 1 of the proxy container. It can
apply `mount_setattr(…, AT_RECURSIVE, MOUNT_ATTR_NOSYMFOLLOW)` to its own
`/var/lib/kubelet/pods` view at startup. The flag is per-mount and the container's
view is an rslave, so nothing leaks to the host. That covers default-medium
`emptyDir`s, which are plain directories on the covered mount. It does **not** cover
tmpfs mounts that propagate in *after* the call: `medium: Memory` `emptyDir`s, and
also the secrets tree, which is not a delivery target. A propagated mount is a clone
of the host's mount, carrying the host's flags. So the interim also needs the agent
to refuse UDS delivery for `medium: Memory` volumes and fall back to TCP. That is a
behaviour change for 034 users, and it is why the interim is only a stopgap.

## Requirements

- **R1 — mesh-owned socket filesystem.** aether picks the mount flags
  (`nosymfollow,nodev,nosuid,noexec`) and the size of the filesystem that carries
  workload sockets.
- **R2 — the proxy stops mounting kubelet's pod-volumes tree.** It reaches workload
  sockets only through a directory that holds nothing but workload sockets.
- **R3 — one knob for the kubelet root, in one component,** following the precedent
  the spiffe-csi-driver chart already follows (`kubeletPath`).
- **R4 — a fixed, generous socket budget,** independent of the volume name and the
  kubelet root, so admission can check it exactly.
- **R5 — loud failure.** A pod that asks for a mesh socket on a node where the mesh
  cannot provide one fails where the operator looks (`FailedMount` on the pod), not in
  an agent log.
- **R6 — the workload stays `baseline`- and `restricted`-clean.** No `hostPath`, no
  privilege, no capabilities.
- **R7 — no change to the user contract that selects UDS delivery.** The
  `endpoint.aether.io/uds-socket` annotation, `EndpointPolicy`, and the
  annotation-wins precedence all stay as they are. What changes is the *volume kind*
  the pod declares.

Non-goals: cross-node sockets; per-port sockets (still one socket per pod, as in 034);
and scheduler awareness of the driver (see *What CSI does not buy*).

## Precedent: how csi.spiffe.io does it (read from talos-main)

`kubectl --context talos-main` (read-only) shows the driver that already sits next to
aether and feeds both the agent and the proxy:

- **CSIDriver** `csi.spiffe.io`: `attachRequired: false`, `podInfoOnMount: true`,
  `volumeLifecycleModes: [Ephemeral]`, `fsGroupPolicy: None`,
  `requiresRepublish: false`.
- **DaemonSet** `spire-spiffe-csi-driver` in `spire-system` (a `privileged` PSA
  namespace), `priorityClassName: system-node-critical`, `maxSurge: 0 /
  maxUnavailable: 1`. It already **tolerates `aether.io/agent-not-ready`**. It has two
  containers:
  - `spiffe-csi-driver` (`privileged: true`, `drop: [ALL]`,
    `readOnlyRootFilesystem`). It mounts `/var/lib/kubelet/pods` with
    **`Bidirectional`** propagation, the socket-source directory read-only, and its
    own CSI socket directory at `/var/lib/kubelet/plugins/csi.spiffe.io`.
  - `node-driver-registrar` (`registry.k8s.io/sig-storage/csi-node-driver-registrar:
    v2.15.0`). It registers `/var/lib/kubelet/plugins/csi.spiffe.io/csi.sock`
    through `/var/lib/kubelet/plugins_registry`.
- `NodePublishVolume` bind-mounts the host socket directory read-only onto the
  kubelet-supplied `target_path`. Nothing more happens: there is no state and no API
  access.

So the pattern demonstrably works on talos: read-only rootfs, `/var/lib/kubelet` on
EPHEMERAL, and Bidirectional propagation from a privileged container. The workload
side (`volumes: [{csi: {driver: csi.spiffe.io, readOnly: true}}]`) is already in
aether's own proxy and agent pods.

**PodSecurity.** The `baseline` profile restricts only `hostPath` among volume types.
`restricted` allow-lists `configMap, csi, downwardAPI, emptyDir, ephemeral,
persistentVolumeClaim, projected, secret`. An inline `csi:` volume is therefore
admissible in every mesh namespace (R6). The node plugin lives in `aether-system`,
which is labelled `pod-security.kubernetes.io/enforce=privileged`.

## Design

### Shape, end to end

```yaml
# workload pod (the only user-visible change vs 034: the volume source)
metadata:
  labels:     { aether.io/managed: "true" }
  annotations:
    endpoint.aether.io/port: "8080"
    endpoint.aether.io/uds-socket: "uds/app.sock"   # unchanged contract
spec:
  containers:
    - name: app
      volumeMounts: [{ name: uds, mountPath: /run/app }]
  volumes:
    - name: uds
      csi: { driver: csi.aether.io }                  # was: emptyDir: {}
```

On the node:

```
/run/aether/uds/<pod-uid>/            per-pod tmpfs, mounted by the node plugin with
                                      nosymfollow,nodev,nosuid,noexec,size=…,mode=…
   └── app.sock                       bound by the app (via /run/app/app.sock)

/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/uds/mount
                                      ← bind of /run/aether/uds/<pod-uid>
```

The proxy already mounts `/run/aether` from the host with `HostToContainer`. The
per-pod tmpfs mounts propagate into it, so it dials
`/run/aether/uds/<uid>/app.sock`. **It needs no new mount**, and its
`/var/lib/kubelet/pods` mount goes away once the `emptyDir` kind is removed (R2).

The path budget is `len("/run/aether/uds/") + 36 + 1 = 53`. That leaves **54 bytes
for the file name**, independent of the volume name and the kubelet root (R4).
Admission can check it exactly.

### The node plugin: `csi.aether.io`

**Binary and packaging.** The node plugin is a new slim binary,
`agent/cmd/uds-csi`, with its logic in `agent/internal/udscsi/`. It gets its own
image (`aether-uds-csi`) and its own DaemonSet (`aether-uds-csi`, template
`charts/aether/templates/uds-csi-daemonset.yaml`) with a `node-driver-registrar`
sidecar. This follows the mesh-dns (#583) and proxy-supervisor (#772) splits, for
the same reasons:

- Rolling the agent must not make UDS pods starting at that moment eat
  `FailedMount` backoff.
- This code changes almost never.
- A `deps_test` (the `//agent/cmd/proxy-supervisor:deps_test` pattern) keeps its
  dependencies to the CSI spec protos, grpc, and `x/sys/unix`. No client-go, no
  controller-runtime: the plugin makes no API calls, so it needs no RBAC.

Hosting it inside the agent pod was rejected. The agent container is deliberately
not privileged (`allowPrivilegeEscalation: false`, root only), and `Bidirectional`
propagation requires a privileged container. Adding a privileged container to the
agent pod couples the rolls and widens the agent pod's surface for no gain.

**CSI services** (`container-storage-interface/spec` v1, a new direct require):

- **Identity:** `GetPluginInfo` (`csi.aether.io`, version), `GetPluginCapabilities`
  (none), `Probe`.
- **Node:** `NodeGetInfo` (`node_id` = node name), `NodeGetCapabilities` (possibly
  `VOLUME_MOUNT_GROUP`, see open question Q2), `NodePublishVolume`, and
  `NodeUnpublishVolume`. There is no Controller service and no staging.

**`NodePublishVolume`** takes `volume_context["csi.storage.k8s.io/pod.uid"]`, which
`podInfoOnMount` supplies, and does the following:

1. It validates the request, rejecting with `InvalidArgument` so the rejection shows
   on the pod as `FailedMount`. Rejected: a request that is not
   `csi.storage.k8s.io/ephemeral: "true"`, `readonly: true` (the app must `bind`),
   an unknown `volumeAttributes` key, a UID that is not a clean single segment
   (reusing `udspath.validateSegment`), and a second, *different* `volume_id` for a
   UID that already has one. That last rule enforces one mesh socket volume per pod.
2. It `mkdir`s `/run/aether/uds/<uid>` (0700 root) and mounts a fresh tmpfs there:
   `nosymfollow,nodev,nosuid,noexec,size=<chart value, default 1Mi>,
   nr_inodes=<small>,mode=0777`. The mode matches the kubelet's `emptyDir` default,
   so apps that work on an `emptyDir` today work unchanged; `fsGroup` tightening is
   Q2.
3. It bind-mounts `/run/aether/uds/<uid>` onto `target_path`. The bind inherits the
   source mount's flags, so `nosymfollow` holds on both the app's view and the
   proxy's view.
4. It is idempotent. If `target_path` is already our bind, it returns OK. This path
   is exercised by kubelet's volume reconstruction after a kubelet restart, and by
   node reboot, where `/run` is tmpfs and the kubelet republishes onto an empty
   `/run/aether/uds`.

**`NodeUnpublishVolume`** runs `umount(target_path)`, then
`umount2(/run/aether/uds/<uid>, MNT_DETACH)`, then `rmdir`. It is idempotent on
"not mounted" and "does not exist". `MNT_DETACH` is used because the app's containers
are dead by now, but a straggling fd must not fail the unpublish.

**Why a per-pod tmpfs rather than one shared tmpfs with per-pod directories:**
size isolation. On a shared tmpfs, one pod writing junk files would starve every
other UDS pod's `bind(2)` with `ENOSPC`. Pages are charged to the memory cgroup of
whoever writes them, which is the app, so a per-pod tmpfs is bounded twice.

**Driver-level hardening that is not about symlinks:**

- `nodev` stops a pod with the default `CAP_MKNOD` from planting a device node for
  the proxy to open. Today `connect` would fail `ENOTSOCK` anyway, but the flag costs
  nothing.
- Hard links cannot cross into the tmpfs from anything the app can see. The tmpfs is
  its own superblock and nothing else is bind-mounted into the app's view of it, so
  the "link another pod's socket in" variant is closed too.

### DaemonSet (the costs, concretely)

| Item | Value |
|---|---|
| namespace | `aether-system` (already `enforce=privileged`) |
| containers | `uds-csi` (`privileged: true`, `capabilities.drop: [ALL]`, `readOnlyRootFilesystem`, `runAsUser: 0`) + `node-driver-registrar` |
| host mounts | `<kubeletRoot>/pods` (**Bidirectional**, `type: Directory`); `<kubeletRoot>/plugins/csi.aether.io` (CSI socket); `<kubeletRoot>/plugins_registry` (registration); `/run/aether/uds` (**Bidirectional**, `DirectoryOrCreate`) |
| chart values | `udsCsi.enabled`, `udsCsi.kubeletRootDir` (default `/var/lib/kubelet`, the **only** place the kubelet root appears for the CSI path), `udsCsi.tmpfsSize`, image, resources |
| scheduling | `priorityClassName: system-node-critical`; tolerates `aether.io/agent-not-ready` (as the spiffe driver already does) and every `NoSchedule` taint the agent DaemonSet tolerates |
| rollout | `maxSurge: 0, maxUnavailable: 1`. Two plugin instances cannot register the same driver name on one node. |
| cluster-scoped | `CSIDriver csi.aether.io` (`attachRequired: false`, `podInfoOnMount: true`, `volumeLifecycleModes: [Ephemeral]`, `fsGroupPolicy: None`, or `File` if Q2 says yes, `requiresRepublish: false`) |
| RBAC | none for the plugin; the registrar sidecar needs none either |
| new external image | `csi-node-driver-registrar` (same tag family SPIRE pins; its own Renovate lane) |
| Chart.yaml | bump (CI-enforced) |

### Agent side

- **Learn the volume kind at CNI ADD.** `enhanceCNIPod`
  (`agent/internal/cni/server/pod.go`) already `Get`s the Pod. It would also record
  the name of the pod's `csi.aether.io` volume, if any, in a new `CNIPod` field
  (`string uds_csi_volume = 12`). The field is persisted for the same reason `uid`
  was in 034: storage replay must resolve paths without the API server.
- **Resolver.** `common/udspath` gains `ResolveCSI(udsRoot, podUID, annotation)`. It
  splits `<volume>/<file>`, requires `volume == uds_csi_volume`, validates `<file>`
  as a single segment, checks the 107-byte limit, and returns
  `<udsRoot>/<uid>/<file>`. The agent flag is `--uds-csi-root`, default
  `/run/aether/uds`. `udsSocketPathForPod` picks `ResolveCSI` when the named volume
  is the CSI volume, and the legacy `Resolve` otherwise. The annotation,
  `EndpointPolicy`, the precedence rules, the failure semantics, and the
  `app_<pod>_<port>`/`health_<pod>` pipe variants are **untouched** (R7).
- **A new capability from the pod spec.** The agent now *knows* whether the volume
  named by the annotation or policy exists. That turns 034's "drift caveat" (a policy
  naming a volume the pods do not mount) from a silent unpromoted endpoint into a
  counted, attributable reason
  (`aether.agent.uds.resolve_failures{reason="volume_not_declared"}`). This is not
  CSI-specific, and it could ship on its own.

### Ordering: NodePublish precedes CNI ADD

The ordering in the kubelet's `SyncPod` is:

1. `volumeManager.WaitForAttachAndMount`, which calls `NodePublishVolume` for every
   volume.
2. `containerRuntime.SyncPod`, which creates the sandbox and triggers **CNI ADD**.
3. The containers start.

Teardown runs the other way. `SyncTerminatingPod` stops the containers and then the
sandbox (**CNI DEL**). The volume manager unmounts (**NodeUnpublish**) only after the
pod is terminated.

So by the time the agent sees ADD, `/run/aether/uds/<uid>` already exists. By the
time Unpublish tears it down, the agent has already removed the pod's clusters. The
agent therefore **does not need to learn anything from NodePublish**. The directory
is a pure function of the UID and the file name comes from the existing contract, so
the plugin stays stateless and no plugin→agent channel exists. That is deliberate:
a shared state file or an RPC from the plugin would re-couple two components that
the ordering already decouples. (#796's "DEL with the agent absent" does not change
this. It only delays cluster removal, and a stale pipe cluster pointing at an
unmounted directory fails `ENOENT` exactly like a dead app.)

### Both directions

- **Inbound (app listens, proxy dials). This is 034 Phase 1, the only shipped
  direction.** It is covered above.
- **Outbound (proxy listens, app dials). This is 034 Phase 2, still deferred.** CSI
  makes Phase 2 *easier*: the directory is mesh-owned and exists before the app's
  containers start, so the agent can program `outbound_uds_<pod>` at ADD time with a
  `Pipe{path: /run/aether/uds/<uid>/mesh.sock, mode: 0666}`. `nosymfollow` protects
  Envoy's `bind` side too. A pre-planted `mesh.sock` symlink makes `bind` fail
  `EADDRINUSE` rather than creating a socket elsewhere, and Envoy's pre-bind unlink
  removes the link, not its target. 034's Phase 2 spike checklist (hot-restart
  inheritance of pipe listeners) still applies unchanged. **Phase 2 should be built
  only on the CSI kind.** Building it on `emptyDir` would mean Envoy, as root,
  creating files inside a kubelet-owned tree that the app can race.

### What CSI does not buy (corrections to the brief)

- **No scheduler awareness.** Inline ephemeral CSI volumes get no topology
  treatment: `allowedTopologies` belongs to StorageClasses, CSINode is consulted for
  attach limits of PVs, and the scheduler does not filter nodes on driver presence
  for an inline volume. A pod lands anyway and sits in `ContainerCreating` with
  `FailedMount: driver name csi.aether.io not found in the list of registered CSI
  drivers`, retried with the kubelet's backoff. That is louder than today (R5), but it
  is not an admission failure. The scheduling gate aether already has is the 033
  taint. See Q3 for whether to join it.
- **The kubelet-root dependency moves; it does not disappear.** The node plugin must
  mount `<kubeletRoot>/pods` and `plugins_registry` like every CSI driver. It shrinks
  from three uncoordinated places (flag, hard-coded chart mount, webhook assumption)
  to one chart value in one component, and it leaves the data path: the proxy and the
  agent never see a kubelet path again.
- **No survival past pod lifetime.** An `emptyDir` already survives container
  restarts, so CSI gains nothing there. Surviving *pod* restarts is a non-goal: a new
  pod is a new UID.
- **Permissions are a wash on the inbound side.** The proxy is root, so no mode ever
  stops it. The app owns its socket's mode either way. The ownership story matters
  only for Phase 2, where the app must traverse a root-created directory, and
  `mode=0777` on the tmpfs, or `fsGroup` via Q2, covers that.

## What it buys, what it costs

**For** (strongest first):

1. **It is the only structural fix for the symlink confused deputy (if Phase 0
   confirms it).** A mesh-owned tmpfs with `nosymfollow` closes the whole class,
   including `Memory`-medium volumes. Neither the agent nor Envoy can close it, and
   the CSI-free interim cannot cover every case.
2. **The proxy loses its view of every pod's volumes, secrets included.** What
   replaces it is a directory that contains nothing but sockets that workloads chose
   to expose to the mesh. That is a real blast-radius cut for a component that parses
   untrusted traffic.
3. **Exact, generous, kubelet-independent budgets and loud failures.** 54 bytes for
   the file name, checkable at admission. The kubelet root becomes one chart value.
   A pod on a node without the driver fails `FailedMount` instead of running
   silently undelivered. It is also the substrate that 034 Phase 2 needs anyway.

**Against** (strongest first):

1. **A new privileged node component, and pod start and termination now depend on
   it.** If the plugin is down, a UDS pod cannot start (`FailedMount` backoff) and a
   deleted UDS pod stays `Terminating`. The kubelet does not finalize the pod until
   its volumes are unmounted, and Unpublish needs the plugin. An `emptyDir` has
   neither dependency. `system-node-critical`, `maxUnavailable: 1`, and a
   rarely-changing slim binary mitigate this, but the dependency is real, and it
   lands in exactly the reboot and power-blip windows that the incident history says
   are where aether hurts.
2. **Demand is close to zero today.** The known UDS workloads are the e2e `udsecho`
   and the talos soak's echo. A CSI driver, a registrar sidecar image, a CSIDriver
   object, a new CNIPod field, a migration, and a deprecation window is a lot of
   machinery for that. It is justified by fixing a security class, not by features.
3. **It is a breaking change for today's UDS workloads** (decision 2026-09-27: no
   coexistence — the CSI volume becomes the only carrier in the release that ships
   it). The known UDS workloads are two (the e2e `udsecho` and the soak's echo), both
   ours, and each migrates by changing one volume source. That is cheaper than a
   release of dual paths in the resolver, the webhook, docs and e2e, and it removes
   the proxy's kubelet-pods mount in the same step rather than one release later.

## Alternatives

- **Status quo plus the CSI-free interim** (supervisor `nosymfollow` remount, plus
  refusing `Memory`-medium volumes). This is cheap and should ship first regardless.
  It leaves the proxy seeing every pod's volumes, it leaves the ~16-character budget
  and the kubelet-root sprawl, and it has a *coverage rule* ("not tmpfs") that is
  easy to break by accident. It is the fallback if Phase 0 refutes the attack.
- **Flip `proxy.udsWorkloads.enabled` to `false` by default.** With default `true`,
  every cluster is exposed even with zero UDS users, because any pod author can opt
  in. If Phase 0 confirms the attack, this should ship **together with the interim**
  as a behaviour change in the same chart release. It is not an alternative to the
  fix, but it is honest about the audience.
- **Workload `hostPath`.** Blocked by `baseline` (and `restricted`), and it hands a
  host path to the workload.
- **A per-node tmpfs injected into pods by the mutating webhook
  (`controller/internal/podmutate`).** The only way to inject a host directory into a
  pod is a `hostPath` volume, which fails the same PSA check whoever authored it.
  Dead end.
- **`emptyDir` `medium: Memory` (today's option).** It is a kubelet-owned tmpfs, so
  aether still cannot choose its flags. `sizeLimit` is only evicted-on-exceed, not
  enforced as a mount size.
- **Abstract-namespace sockets (`@name`).** Abstract `AF_UNIX` names are scoped to
  the **network namespace**. The app is in the pod netns and the proxy is
  host-network, so the proxy cannot see the app's abstract socket at all. Envoy's
  `NetworkNamespaceFilepath` exists for `SocketAddress` binds only, and pipe
  upstreams connect in Envoy's own netns (034 already rejected this). There is no
  filesystem, so there are no flags to set, but also no rendezvous.
- **An agent-side `lstat` / `O_PATH` check before rendering the pipe.** A TOCTOU
  race with an unbounded window, since the app can swap the file after every check.
  Not a fix.
- **The socket file named in the CSI `volumeAttributes`** (`socket: app.sock`)
  instead of the annotation. Rejected for now: it would be a third place to declare
  delivery, with its own precedence against the annotation and `EndpointPolicy`
  (R7). Revisit only if the annotation is ever retired.
- **A generic ephemeral volume (`ephemeral:` + a StorageClass on the same driver).**
  It gives StorageClass `allowedTopologies`, but it creates a PVC per pod, needs a
  Controller service, and makes the plugin a provisioner. That is far more machinery
  for scheduling awareness the 033 taint already approximates.

## Plan

| Phase | PR(s) | Contents | Gate |
|---|---|---|---|
| **0 — settle the finding** | 0a (issue + kind repro); 0b (interim) | 0a: `e2e/uds.sh` gains a `hostile` leg, where a `udsecho` variant swaps `app.sock` for a symlink to `/run/aether/cni.sock` (and, SPIRE on, to the workload socket) and calls itself with `protocol: grpc`. The assertion is that the gRPC reaches the agent. The leg must be seen **failing** (attack works) before any fix, so it is not a vacuous gate. 0b, only if 0a confirms: supervisor `mount_setattr(AT_RECURSIVE, NOSYMFOLLOW)` on its kubelet-pods view, the agent refuses `medium: Memory` UDS volumes, and `udsWorkloads.enabled` defaults to `false` (chart bump, release note). The 0a leg then asserts `ELOOP`/no delivery. | 0a result decides the proposal's fate (see Status) |
| **1 — the driver** | 1a, 1b | 1a: `agent/cmd/uds-csi` + `agent/internal/udscsi/` (Identity/Node services, per-pod tmpfs + bind, idempotency, unit tests against a fake mounter, and a root-only mount test in the kernel-gate CI lane the TPROXY work added). 1b: chart (`uds-csi-daemonset.yaml`, `CSIDriver`, values, Chart.yaml bump), image, `deps_test`. Inert until a pod declares the volume. | driver registers on kind; `FailedMount` on bad requests |
| **2 — the CSI volume becomes the only carrier (BREAKING)** | 2 | `CNIPod.uds_csi_volume = 12` via `enhanceCNIPod`; `udspath.ResolveCSI` **replaces** the `kubernetes.io~empty-dir` resolver (the emptyDir shape, `--kubelet-pods-dir`, the proxy's `/var/lib/kubelet/pods` mount and `proxy.udsWorkloads` are deleted in the same PR — chart major bump, release note); `--uds-csi-root`; `resolve_failures{reason}` counter (seeded at 0 so it is never "no series"); the `EndpointPolicy` webhook computes the 54-byte CSI budget (for a policy the webhook cannot see the pod, so it keeps a conservative budget and the agent stays fail-closed, as today); podmutate **rejects** an `emptyDir` carrier with a message naming the CSI volume; `e2e/udsecho` and `e2e/soak/udsecho` switch their volume source in the same PR. | `e2e/uds.sh` runs every existing leg on the CSI carrier; the hostile leg passes with no kubelet-pods mount on the proxy at all; an `emptyDir` carrier is rejected at admission (seen red on the old build) |
| **3 — talos** | — | Migrate the soak's UDS echo to the CSI volume in the same `helm upgrade` (the old carrier stops resolving the moment the agent rolls, so the workload and the chart move together); validate across an **agent roll, a uds-csi roll, a proxy roll and a node reboot**: mounts persist across a plugin roll, republish is idempotent after reboot, Terminating pods drain while the plugin rolls. One 8 h soak with the UDS leg on the CSI carrier. 034 Phase 2 is built on the CSI carrier only. | soak grade |

No coexistence phase and no deprecation window (decision 2026-09-27): a UDS workload
that does not switch its volume source in the upgrade that ships Phase 2 stops being
delivered — loudly, at admission for new pods and as `resolve_failures{reason="emptydir"}`
for running ones — which is the same "breaking change, no compatibility flags" rule the
TPROXY cut-over used. Callers see nothing, exactly as in 034: endpoints stay
`pod_ip:18008`, and multi-cluster, E/W waypoint, and delegated liveness are all
unaffected.

## Open questions

- **Q1 — Does Phase 0a reproduce?** The candidate blockers to check are grpc-go
  rejecting h2c with Envoy's added pseudo-headers (unlikely), the CNI server's
  protovalidate rejecting a plausible request (it validates shape, not caller), and
  SPIRE's attestation of Envoy's PID mapping to *no* registration entry (then that
  row degrades to "denied", but the CNI/xDS rows stand). **Any one confirmed row is a
  confirmation.**
- **Q2 — `fsGroup` delegation.** Should the CSIDriver declare `fsGroupPolicy: File`
  with the `VOLUME_MOUNT_GROUP` capability, so the plugin gets
  `volume_mount_group` and mounts the tmpfs `gid=<fsGroup>,mode=2770` instead of
  `0777`? It is tighter, and it matters for Phase 2 where a *third* container in the
  pod should not reach the mesh socket. The cost is a behaviour difference from
  `emptyDir` that could break apps that are migrated blindly.
- **Q3 — Join the 033 taint gate?** Should the agent's taint removal also wait for
  `csi.aether.io` to appear in the node's `CSINode`? This gives UDS pods scheduling
  protection at the cost of one more gate on *every* node, most of which host no UDS
  workloads. The leaning answer is **no**: `FailedMount` backoff already covers it,
  and the gate should not grow for a niche feature. It should be re-evaluated after
  the Phase 3 reboot test.
- **Q4 — Terminating-while-plugin-down.** How long does a UDS pod stay Terminating
  across a plugin roll on talos, and does anything in aether (the ghost sweep, the
  #641 eviction path) misread a long Terminating as a stale pod? This should be
  measured in Phase 3, not assumed.
- **Q5 — Registrar sidecar supply chain.** Should aether reuse the exact
  `csi-node-driver-registrar` tag the SPIRE chart pins, so talos pulls one image
  instead of two, or pin its own? The recommendation is to pin its own and let
  Renovate converge them.
