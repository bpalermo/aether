# Configuration Reference

A structured reference for configuring Aether: the **`aether` Helm chart values**,
the **CRDs**, the **CLI flags** of each binary, and the **labels/annotations**
workloads use. Values and defaults are sourced from
[`charts/aether/values.yaml`](../charts/aether/values.yaml),
[`common/constants/`](../common/constants), and each binary's Cobra flags — those
files are authoritative if anything here drifts.

Configuration has **two layers**:

1. **Deploy-time system config** — `aether` chart values, set once and inherited by
   every component (agent, proxy, registrar, controller, edge).
2. **Runtime proxy observability** — the namespaced `MeshConfig` CR, which
   overrides *only* the proxy data plane's access-log / tracing / stats policy
   without a redeploy.

---

## 1. Chart values (`charts/aether`)

### Top-level

| Key | Default | Purpose |
|---|---|---|
| `nameOverride` / `fullnameOverride` | `""` | Override the chart name / fully-qualified resource name. |
| `namespace.create` | `true` | Create the release namespace with privileged pod-security labels (the agent needs `hostNetwork` + `NET_ADMIN`). With `true`, do **not** pass `--create-namespace` (a first install fails with `namespaces "…" already exists`); the namespace must exist beforehand carrying Helm's ownership metadata, see [Getting started](./getting-started.md#install). With `false`, pass `--create-namespace` or create the namespace yourself, and label it `pod-security.kubernetes.io/enforce: privileged` where Pod Security admission is enforced: nothing else will (#1384). |
| `namespace.name` | `""` | Namespace all resources deploy into (defaults to the release namespace). |
| `clusterName` | `talos-main` | Cluster name passed to agent + registrar (`--cluster-name`); used in registry keys. |
| `controlCluster` | `""` | Cross-cluster config authority (026 EM3). Set to a cluster name → only that cluster's registrar exports GAMMA config and everyone imports only from it. Empty = federated (any peer, highest-version wins). |
| `debug` | `true` | Verbose logging on all components (`--debug`). |
| `meshDomain` | `aether.internal` | DNS-style domain services are addressed under (`<service>.<meshDomain>`); also the ODCDS catch-all suffix. |

### Resources — removing a default request or limit

Every container's `resources` value (`agent`, `agent.meshDnsDaemon`, `cniInstall`, `udsCsi`, `proxy`, `proxy.supervisor`, `proxy.authzSidecar`, `edge`, `registrar`, `controller`) has defaults in `values.yaml`, and Helm **merges** maps: an override is laid over the defaults key by key, so an empty map removes nothing. `--set-json 'controller.resources={}'`, `--set-json 'controller.resources.limits={}'` and a values file with `resources: {}` or `limits: {}` all render the unchanged defaults. To remove one quantity, set that quantity to null or to the empty string. Both work on every container since chart 2.4.14 (#1355: before it, an empty string rendered `cpu: ""` on the agent, proxy, authz sidecar, edge, registrar and controller, which the apiserver rejects). Removing a limit: `--set controller.resources.limits.cpu=null`, `--set controller.resources.limits.cpu=`, or `limits: {cpu: null}` / `limits: {cpu: ""}` in a values file. Removing a request is the same on the other map: `--set controller.resources.requests.cpu=null`, `--set controller.resources.requests.cpu=`, or `requests: {cpu: null}` / `requests: {cpu: ""}`. A map left with no entries is not rendered, and a whole map can be dropped at once with `--set controller.resources.limits=null` (or `requests=null`, or `requests: ~` in a values file). Removing `limits.memory` also removes `GOMEMLIMIT` where the chart derives it. Removing `limits.cpu` from the `registrar` or the `controller` leaves their `GOMAXPROCS` (`resourceFieldRef: limits.cpu`) following the node's core count, because the kubelet substitutes node allocatable for a missing limit; their defaults keep the limit for that reason. Every spelling above was rendered with `helm template` (Helm 3.18) against chart 2.4.14; none was applied to a cluster. Do not combine them with `--reuse-values`: read the release's values back (`helm get values -o yaml`) and pass them with `-f`.

### `otel` — system-wide telemetry

Enable once; every component inherits it. The proxy may override its own
access-log/tracing policy via the MeshConfig CR.

| Key | Default | Purpose |
|---|---|---|
| `otel.enabled` | `false` | Enable the OTel MeterProvider + push telemetry everywhere. |
| `otel.endpoint` | `""` | OTLP gRPC collector `host:port` (insecure). Empty disables OTLP + the proxy/CNI stat sink. Deploy-time value baked into the CNI plugin and Envoy bootstrap (never read from a runtime ConfigMap). |
| `otel.logs` | `false` | Export component logs over OTLP (also tee'd to stderr). |
| `otel.traceSampleRate` | `0.1` | Head-sampling ratio (0.0–1.0); bounds exported spans only. |
| `otel.traceExport` | `false` | Export spans over OTLP (needs a collector traces pipeline). |

### `spire` — system-wide mTLS

`enabled` is the mesh-wide mTLS switch; the rest is per-component plumbing.

| Key | Default | Purpose |
|---|---|---|
| `spire.enabled` | `true` | Mesh-wide mTLS switch (agent, registrar, controller webhook cert). |
| `spire.workloadSocketPath` | `/run/secrets/workload-spiffe-uds/socket` | Workload API socket (the `csi.spiffe.io` mount). |
| `spire.brokerSocket.hostPath` | `/run/spire/agent/sockets/csi.spiffe.io/broker` | SPIRE agent **SPIFFE Broker Endpoint** socket directory on the node — where the `spiffe/spire` chart puts it with `spire-agent.sockets.broker.mountOnHost=true`. The agent brokers an X.509-SVID for every pod on its node through it (proposal 036). |
| `spire.brokerSocket.mountPath` | `/run/spire/broker-sockets` | Where that directory is mounted in the agent container. |
| `spire.brokerSocket.socketName` | `broker.sock` | Broker socket filename. |
| `spire.waitWarnAfter` | `2m` | How long a component may wait for its first SVID before the waiting log line escalates from INFO to WARN (`--spire-wait-warn-after` on agent, registrar, controller and edge). On the **agent** it is also the dwell before the `spire-svid` readiness check reports NotReady — which arms the node taint, so it must not fire on a brief SPIRE hiccup (#740). The Deployments take no dwell: they go NotReady the instant they are known to lack an identity, which just removes one replica from one endpoint set. |

**Workload SVID DNS SANs (mesh-wide, required by east-west QUIC).** East-west QUIC
(038 Phase 4b) is unconditional — there is no value or flag for it — so every
workload SVID must carry the DNS SANs `<sa>.<ns>.<meshDomain>` and
`*.<sa>.<ns>.<meshDomain>` (`dnsNameTemplates` on the workloads' `ClusterSPIFFEID`).
Envoy's QUIC client checks the SNI `<port>.<sa>.<ns>.<meshDomain>` against them after
the SPIFFE pin (aether#957); the requirement stands until envoyproxy/envoy#47740 is in
a plain proxy pin (today's pin carries it as a patch, #972). This is SPIRE
configuration, not an aether chart value; see [`runbook.md`](./runbook.md)
§ *East-west QUIC* for the spiffe/spire snippet, the rollout order and the budget.

### `meshConfig` — proxy MeshConfig seeding

| Key | Default | Purpose |
|---|---|---|
| `meshConfig.createDefault` | `true` | Seed the singleton `MeshConfig` (`default`) on first install only — never overwritten on upgrade (operators own it via kubectl). |
| `meshConfig.proxy` | `{}` | The `spec.proxy` seeded into that CR (protojson field names). Empty = proxy inherits everything from system config. |

### `agent`

| Key | Default | Purpose |
|---|---|---|
| `agent.gamma` | `true` | GAMMA east-west L7 routing (018): watch `HTTPRoute`s **and `GRPCRoute`s** parented to a Service (plus `ReferenceGrant`s and `HTTPFilter` attachments) and apply them on **both** the explicit outbound path and the transparent-capture path. MESH-HTTP Core conformance-green. Safe without the Gateway API CRDs (CRD-detected, degrades with a warning); `false` is a kill switch. |
| `agent.cniConflistReassert` | `true` | Keep aether chained in the node's active CNI conflist (#645): the agent watches `/etc/cni/net.d` (read-write mount) and re-appends the `aether-cni` entry whenever a competing writer strips it — kube-flannel `cp -f`s its ConfigMap template over `10-flannel.conflist` on every flannel pod recreation, which a Talos bootstrap-manifest re-sync triggers, silently unmeshing every pod started afterwards. Never creates a conflist of its own; `false` is a kill switch. |
| `agent.updateStrategy.surge` | `false` | How the agent DaemonSet rolls (proposal 041). `false`: delete-then-create (`maxSurge: 0, maxUnavailable: 1`); the node's proxy has no ADS stream for the whole pod replacement + startup. `true`: `maxSurge: 1, maxUnavailable: 0`; the new agent starts as a **standby** (it finds `/run/aether/agent.lock` held), builds its whole first snapshot but binds no node socket and writes no node file, reports Ready, and takes the node over the instant the old agent exits. The gap becomes the reconnect backoff plus a storage diff (~0.1-0.4 s). Needs room on every node for a second `agent.resources` request during the overlap. Roll out in two steps: upgrade to 2.3.0 with `false` first, then set `true` (runbook explains why). In both modes the agent pod declares no `ports:` and probes over a pod-local socket (`/agent-ready`). See [`runbook.md`](./runbook.md) § *Agent surge roll (proposal 041)*. |
| `agent.importConfig` | `false` | Cross-cluster config import (026): poll the registrar for peer-exported GAMMA projections and materialize them (merged with local; local wins). Pairs with `registrar.registryBackend=etcd`. |
| `agent.eastWestWaypoint` | `false` | East/west waypoint (019): dial **cross-cluster** endpoints at their node's routable IP + the fixed tunnel port `18009` instead of the (unroutable) pod IP; this node's host-network proxy SNI-forwards inbound tunnel traffic to local pods. Intra-cluster stays direct pod-to-pod. Needs cross-cluster endpoint visibility (shared or replicated etcd) + a shared SPIRE trust domain. |
| `agent.captureRedirectAllDefault` | `true` | Redirect-all as the DEFAULT for managed pods (022 Step 4); opt out per-pod with `capture.aether.io/redirect-all="false"`. `false` = per-pod opt-in via the same annotation set to `"true"`. (Transparent capture itself and the passthrough chain are unconditional since proposal 031.) |
| `agent.meshDns` | `true` | Per-pod mesh DNS (018). Gates BOTH halves: the agent's in-process resolver (which writes the record snapshot) and the separate `aether-mesh-dns` DaemonSet that serves pods from it. |
| `agent.meshDnsUpstream` | `[]` | Upstream resolver(s) for non-mesh queries, passed to the **mesh-dns daemon** (`--mesh-dns-upstream`), not the agent. Empty = the daemon's own resolv.conf (kube-dns). |
| `agent.meshDnsDaemon.image.*` | repo+digest placeholders | The slim `mesh-dns` image, pinned separately from the agent's (#583) — the DaemonSet ships only the `/mesh-dns` binary. Rendered when `agent.meshDns` is true. |
| `agent.meshDnsDaemon.resources.{requests,limits}` | requests cpu `50m` / mem `32Mi`; limits mem `64Mi`, **no CPU limit** | No CPU limit by design (#1253, chart 2.4.8; was `25m` request / `100m` limit). The daemon is every managed pod's resolver, so its tail latency is every pod's DNS latency: at `25m`/`100m` it averaged ~24.5m yet was CFS-throttled in 8.7 % of 100 ms periods, because its CPU comes in bursts and a throttled period parks every query in flight until the next one. The `50m` request is its scheduling reservation and its CFS weight against the proxy under contention. QoS was and stays Burstable. Set `limits.cpu` to put a cap back (and see `goMaxProcs`); an empty `limits.cpu` renders no limit. No `GOMEMLIMIT` is set for this container. How to measure throttling: [`runbook.md`](./runbook.md) § *mesh-dns CPU throttling*. |
| `agent.meshDnsDaemon.goMaxProcs` | `2` | Rendered as the mesh-dns container's `GOMAXPROCS` env var (#1253). The Go runtime derives it from the CPU limit with a floor of 2 — so 2 is what the old `100m` limit gave — and without a limit it would scale to the node's core count (and the GC's background workers with it). `0` (or empty) omits the env var and lets the Go runtime choose. |
| `agent.meshDnsDaemon.forwardPoolSize` | `null` | Pooled, already-connected UDP sockets per upstream on the forward path (`--forward-pool-size`; #674 — dialling per query was 20.97% of this daemon's CPU). Unset emits no flag and takes the built-in default of `8`; `0` disables pooling and restores dial-per-query. An escape hatch, not a tuning knob: reuse trades per-query source-port randomisation for the socket's lifetime, which is safe only because the socket is *connected* (the kernel drops any datagram not from the upstream's exact address). |
| `agent.meshDnsDaemon.lameDuckMax` | `10s` | Ceiling on the post-SIGTERM window in which the resolver stops reporting ready but keeps answering (`--lame-duck-max`; #729). It closes as soon as it observes a *different* instance answering on the same address, so the ceiling only bites when no successor appears (scale-down, node drain, failed surge). The DaemonSet's `terminationGracePeriodSeconds` is derived from this (+5s). `"0s"` restores the pre-#729 close-on-SIGTERM behaviour, which dropped one queued datagram on every roll. |
| `agent.meshDnsDaemon.debug` | `false` | TRACE logging for this daemon only (#684). The top-level `debug` deliberately no longer reaches mesh-dns — it is the one component on every managed pod's `:53` path, and everything it logs is also fanned out to the OTLP log exporter. |
| `agent.eastWestQuicIdleTimeout` | `8s` | Idle timeout of every east-west QUIC (`quic:`) twin's upstream pool (`--east-west-quic-idle-timeout`, #1054); h1/h2 pools keep 30 s. An idle source h3 connection gets no GOAWAY when the destination proxy hot-restarts, so it must close before the parent exits or its next packet draws a stateless reset. The chart **refuses to render** unless `eastWestQuicIdleTimeout + 5s < proxy.hotRestart.parentShutdownTime` (and it must be `> 0`); lowering the parent-shutdown time needs this lowered with it. Every twin also carries a fixed QUIC transport `idle_network_timeout` of 8 s and a 1 s keep-alive PING while a request is open (#1087: a dead destination fails an in-flight request in at most about 10 s instead of hanging to the 15 s route timeout; 8 s, not less, because of the #1093 worker stalls). The transport idle also closes a twin connection with no open stream after 8 s, so values of this flag above 8 s no longer lengthen reuse. Cost: a pair idle between this and 30 s pays one extra QUIC handshake. |
| `agent.image.*` | repo+digest placeholders, `pullPolicy: Always` | Digest-pinned image; mirror by overriding `repository` alone. |
| `agent.resources.{requests,limits}` | requests cpu `200m` / mem `96Mi`; limits mem `128Mi`, **no CPU limit** | No CPU limit by design (#1116): snapshot builds are 5–25 ms CPU bursts on the ODCDS/EDS path, and a 200m CFS quota turned them into 100 ms steps and could throttle a goroutine while it held the snapshot-cache mutex (#1105/#1114). The pod is therefore QoS **Burstable, not Guaranteed**. To get Guaranteed, set `limits.cpu` equal to `requests.cpu` (raise both to at least `1000m`) and `requests.memory` equal to `limits.memory`. Memory was `64Mi`/`64Mi` before chart 2.4.2; the 2026-10-04 soak OOMKilled agents at that limit (Go runtime 44–61 MiB, plus the `/agent-ready` exec probe processes, ~3 MiB RSS each, charged to the same cgroup). `GOMEMLIMIT` is rendered at **90 %** of `limits.memory` (115.2 MiB at the default) and omitted when no memory limit is set. A surge roll needs a second `200m`/`96Mi` request per node. See [`runbook.md`](./runbook.md) § *The node agent is OOMKilled*. |
| `agent.goMaxProcs` | `2` | Rendered as the agent container's `GOMAXPROCS` env var (#1116). Since Go 1.25 the runtime derives `GOMAXPROCS` from the cgroup CPU limit, so without a limit it would scale to the node's core count. `0` (or empty) omits the env var and lets the Go runtime choose, e.g. when you set an explicit CPU limit. |

> The agent reaches the registry only through the registrar (gRPC) — it never
> talks to an external registry backend and carries no backend credentials.

### `proxy` — per-node Envoy DaemonSet

| Key | Default | Purpose |
|---|---|---|
| `proxy.enabled` | `true` | Deploy the per-node Envoy. Disable to run only the agent. |
| `proxy.image.repository` | `quay.io/aethermesh/proxy` | External image built by the `//proxy` workspace, digest-pinned by the proxy release's bump-chart PR. Every pin reader accepts exactly this repository (`image_reference("proxy")`). |
| `proxy.image.tag` | (commit SHA) | The publishing commit. |
| `proxy.image.digest` | (index digest) | Multi-arch index digest of that commit's image; the `aether.image` helper prefers it over `tag`. |
| `proxy.logLevel` | `info` | Envoy log level. |
| `proxy.jsonLogs` | `true` | Envoy application logs as one JSON object per line. |
| `proxy.hotRestart.baseId` | `0` | Envoy hot-restart tunables (mechanism is not optional; see proposal 001). |
| `proxy.hotRestart.drainTime` | `10s` | Graceful connection-close window for the draining epoch. |
| `proxy.hotRestart.parentShutdownTime` | `15s` | When the previous epoch is terminated (must exceed drainTime). Also the supervisor's admin re-verify budget: the epoch-identity probe re-confirms on a fresh connection every `parentShutdownTime/3` (floor 2s, ceiling 15s), so a cross-pod takeover is diagnosed while the draining parent still lives. Below ~6s the floor takes over and the supervisor logs the lost margin at startup; raising it also delays successor-pod readiness by the same amount. |
| `proxy.hotRestart.handoffDeadline` / `adminUnresponsiveDeadline` | `0` | Supervisor watchdogs (0 = built-in defaults). |
| `proxy.hotRestart.shmHostPath` | `/run/aether/shm` | Shared-memory hostPath for cross-pod hot restart. |
| `proxy.hotRestart.drainStrategy` | `gradual` | Envoy `--drain-strategy` for the hot-restart parent and every other drain (LDS listener removal, the pod-termination drain): `gradual` or `immediate`. `immediate` is an opt-in only: on talos-main it made the #1054 stateless resets worse (#1068); the carried patches (#1064/#1066/#1069) fix the exit window under `gradual`. See [`runbook.md`](./runbook.md) § *Source h3 requests die on a stateless reset at a destination's roll (#1054)*. |
| `proxy.hotRestart.skipParentStats` | `false` | Pass Envoy `--skip-hot-restart-parent-stats` (#1050). An off-by-default **emergency switch**: the hot-restart main-thread deadlock it worked around is fixed by the carried patch #1060. Turning it on costs the parent's gauges and its last ≤5 s of counter deltas in the child. |
| `proxy.hotRestart.hotRestartOnConcurrencyChange` | `false` | Supervisor `--hot-restart-on-concurrency-change` (#1136). Off: a roll that changes the Envoy worker count (`proxy.concurrency`) drains the predecessor and starts a fresh Envoy instead of hot-restarting. On: hot-restart anyway, which resets about half of the predecessor's live HTTP/3 connections on every node. An operator override, not a tuning knob. |
| `proxy.concurrency` | `0` | Envoy worker thread count (#1093). `0` passes no flag, so Envoy picks its own default: the smallest of the node's online CPUs, the CPUs the container's affinity mask allows, and the container's cgroup CPU limit (`proxy.resources.limits.cpu`) rounded **down** to a whole CPU, never less than 1. That is one worker per core only when the container has no CPU limit and no restricted mask, as with the chart's defaults; with a `1500m` limit it is one worker, with `2` or `2500m` two. `N > 0` passes `--concurrency N` to every Envoy epoch through the supervisor, and Envoy then runs exactly `N` whatever the limit or the mask. Exists because on a 4-core node a hot-restart handoff runs two Envoys (8 workers + 2 main threads on 4 cores). **Changing it is a drain + fresh start, not a hot restart** (#1136): a hot restart between different worker counts re-steers the parent's QUIC connections by the child's count (the reuse-port steering program belongs to the whole socket group) and resets about half of the live HTTP/3 connections on each node. So when the new pod's supervisor finds a live predecessor whose `/server_info` reports another `concurrency`, it logs one WARN naming both counts, drains the predecessor (`POST /drain_listeners?graceful`, then `hotRestart.drainTime`), stops it (`/quitquitquit`) and starts its own Envoy fresh at epoch 0; `aether_supervisor_handoff_mode_total{mode="fresh_after_drain"}` counts it. The cost, per node in turn, is the drain window plus a gap with no listeners while the fresh Envoy initializes (on kind: about 2 s, bridged by SYN retransmits, 0 failed requests; on a busy node expect the successor's init time, 3–15 s on talos-main). Every restart after that rollout is same-to-same and hot again. With `0` the supervisor has no flag to compare, so it computes the count the way Envoy does, from its own container's CPUs, affinity and cgroup limit (#1442; until then it assumed one worker per online CPU, so a proxy with a CPU limit or a restricted mask took the drain + fresh start on **every** roll). A rollout that changes `proxy.resources.limits.cpu` across a whole-CPU boundary while `proxy.concurrency` is `0` therefore changes the worker count, and is a drain + fresh start like any other count change. `proxy.hotRestart.hotRestartOnConcurrencyChange` forces the old hot restart. A negative or non-integer value fails the render. **Do not change it on a live mesh until both prerequisites are deployed**: the #1126 successor crash is fixed by the carried Envoy patch `envoy-aether1126-forwarded-udp-worker-index.patch` in proxy images built from it or later, and the #1127 supervisor fix is the other prerequisite; see [`runbook.md`](./runbook.md) § *Sizing nodes for a proxy hot restart*. |
| `proxy.terminationGracePeriodSeconds` | `180` | The proxy pod's `terminationGracePeriodSeconds`, also passed to the supervisor as `--termination-grace`. The deleted pod keeps its Envoy alive as the successor's hot-restart parent, so it is deliberately generous. Where no successor can appear (node shutdown, `kubectl delete daemonset`, a replacement stuck Pending) the supervisor drains Envoy itself at `terminationGrace − (hotRestart.drainTime + 15s)` instead of being SIGKILLed (#771); keep it well above `drainTime + 15s`. |
| `proxy.overload.enabled` | `true` | Envoy overload-manager graceful-degradation ladder. |
| `proxy.overload.maxHeapSizeBytes` | `402653184` (384Mi) | Keep at ~75% of `resources.limits.memory`. |
| `proxy.resources.{requests,limits}` | cpu `500m`, mem `512Mi` | No CPU limit on purpose. The CPU request buys the proxy cgroup weight on a contended node, not capacity; a hot-restart handoff needs about a core of node headroom on top of it. See [`runbook.md`](./runbook.md) § *Sizing nodes for a proxy hot restart*. |
| `proxy.supervisor.resources.{requests,limits}` | requests cpu `100m` / mem `32Mi`; limits mem `64Mi`, **no CPU limit** | Resources of the `install-supervisor` init container (#1344, chart 2.4.13; it rendered no `resources` before). It runs on every proxy pod start and copies the supervisor and the `proxy-ready` prober onto the pod's shared volume: about 50 ms of CPU and a 14 MB peak RSS, measured on a workstation build. The request is `cniInstall`'s; it gives the container a CFS weight on a busy node and puts it in request accounting, and reserves nothing extra (the `proxy` container's `500m` / `512Mi` is the pod's effective request). No CPU limit by design (a quota is what slowed `cni-install`, #1335). To add a cap set `limits.cpu`; an empty value renders no limit rather than an invalid quantity. |

#### `proxy.authzSidecar` — external authorization (proposal 027)

| Key | Default | Purpose |
|---|---|---|
| `proxy.authzSidecar.enabled` | `false` | Add a node-local authz gRPC sidecar (UDS) + a DISABLED ext_authz filter entry; zero effect until an `HTTPFilter` (extAuthz) opts a route/service in. |
| `proxy.authzSidecar.opa.enabled` | `false` | Built-in OPA preset (opt-in). |
| `proxy.authzSidecar.opa.image` | `openpolicyagent/opa:1.21.1-envoy-static` | OPA image. |
| `proxy.authzSidecar.opa.policy` | `""` | Rego policy (ConfigMap-mounted); required when `opa.enabled`. OPA reads it once, at start, so a changed policy **rolls the proxy DaemonSet** (a hot restart on every node): the pod template carries `checksum/opa-policy`, the sha256 of this text (chart 2.4.15, #1363). Before 2.4.15 a policy changed without a chart version change was not applied until the next roll. |
| `proxy.authzSidecar.image.{repository,tag,args}` | `""` / `[]` | Bring-your-own authz container (serves `envoy.service.auth.v3.Authorization` on `unix:///run/aether/authz/authz.sock`). |
| `proxy.authzSidecar.timeout` | `200ms` | Per-check gRPC timeout. |
| `proxy.authzSidecar.failureMode` | `DENY` | `DENY` (fail-closed, 403 when unreachable) or `ALLOW` (fail-open). |
| `proxy.authzSidecar.resources` | `10m` / `32Mi` requests, `128Mi` memory limit | Sidecar resources (OPA preset and bring-your-own). No CPU limit on purpose: it is on the request path, and throttling becomes ext_authz timeouts — 403s under `DENY`. |
| `proxy.authzSidecar.startupProbe.enabled` | `true` | Render the sidecar's startupProbe. The kubelet starts the `proxy` container only after it passes (#1275). Off: the sidecar counts as started as soon as its process runs. |
| `proxy.authzSidecar.startupProbe.{periodSeconds,timeoutSeconds,failureThreshold}` | `1` / `1` / `120` | Timings of the default probe, which execs the staged `proxy-ready --unix-socket=/run/aether/authz/authz.sock` inside the sidecar container: it passes once a `connect(2)` to the authz socket succeeds. Works for the OPA preset and for a bring-your-own image, since both must serve on that socket. |
| `proxy.authzSidecar.startupProbe.override` | `{}` | A full Kubernetes probe, used verbatim instead of the default socket check. The pod is `hostNetwork`, so a port probe answers for whichever pod on the node holds the port. |
| `proxy.authzSidecar.{livenessProbe,readinessProbe}` | `{}` | Optional probes, rendered verbatim. A native sidecar's readinessProbe counts toward pod Ready (and so gates a proxy roll); a failed livenessProbe restarts only the sidecar, and checks fail per `failureMode` while it is down. |

**The sidecar is a native sidecar (chart 2.4.9, #1275).** `authz` is an init container
with `restartPolicy: Always`, placed after `install-supervisor` (which stages the probe
binary) and before the `proxy` container. The kubelet does not start `proxy` until the
sidecar's startupProbe passes, so a new Envoy never takes the node's listeners with no
authz behind it. On deletion the kubelet stops the sidecar only after `proxy` has exited,
so an Envoy that is still serving (waiting for a successor, or draining) keeps its authz.
It used to be a second regular container, started after `proxy`. When its image had to be
pulled, the new Envoy served for 7–13 s with no authz, and every check failed (a 403 under
`DENY`). See [`runbook.md`](./runbook.md) § *The ext_authz sidecar across a proxy roll*.
Native sidecars need **Kubernetes >= 1.29** (on by default from 1.29, GA in 1.33). With
the sidecar enabled the chart refuses to render for an older cluster. An older apiserver
drops `restartPolicy` from an init container, and the sidecar would then be an init
container that never exits, with every proxy pod stuck in `Init`. `helm template` with no
cluster uses Helm's built-in Kubernetes version, so pass `--kube-version` there. With the
sidecar disabled nothing changes and no minimum applies.

**OPA 1.21 YAML change (chart 2.4.6, #1222).** The preset image moved from OPA 1.20.2
to 1.21.1, which parses YAML under the 1.2 core schema everywhere OPA reads it
(`--data`, bundles, config files, the `yaml.unmarshal` builtin): the bare words
`yes`/`no`/`on`/`off`/`y`/`n` are now **strings**, not booleans (`true`/`false` are
unchanged), and a YAML document with unreachable content is rejected. The preset
itself hands OPA no YAML — only command-line flags and the `opa.policy` Rego file — so
the chart needs no change. If your policy calls `yaml.unmarshal`, or you run your own
OPA config/bundle/data YAML through a bring-your-own image on 1.21+, write booleans as
`true`/`false` (or quote the word if you meant the string). The same release also types
empty literals (`{}`, `[]`) as empty, so a policy that selects a key out of one, or
compares a non-empty object/array to `{}`/`[]`, now fails to compile — use
`count(x) == 0`. A policy that does not compile keeps the sidecar from serving, which
under `failureMode: DENY` is a 403 on every opted-in route, so check it with
`opa check` against the new image before upgrading.

Envoy exports **no** `ext_authz` statistics until a route actually uses the filter
(its OTLP stats sink only flushes counters that have been used). The prober chart's
`authzCanary` gives a cluster one such route and asserts an allow and a deny
decision every cycle — see `charts/prober/values.yaml`.

### `cniInstall` — CNI installer init container

| Key | Default | Purpose |
|---|---|---|
| `cniInstall.image.*` | repo+digest placeholders, `pullPolicy: IfNotPresent` | Digest-pinned image. |
| `cniInstall.resources.{requests,limits}` | requests cpu `100m` / mem `32Mi`; limits mem `32Mi`, **no CPU limit** | No CPU limit by design (#1335, chart 2.4.12; was a `100m` limit). The init container runs on every agent pod start, and its work is one short CPU burst (Go start-up plus a byte compare of the plugin on the host against the image's) that a `100m` quota — 10 ms per 100 ms period — stretched to 500–800 ms on all 40 agent starts measured on talos-main, always a multiple of ~100 ms, for at most 50–80 ms of CPU. The request costs the node nothing (the agent container's `200m` is the pod's effective request). To restore a cap set `limits.cpu`; an empty value (`--set cniInstall.resources.limits.cpu=`) renders no limit rather than an invalid quantity. See `docs/runbook.md`, "cni-install CPU limit". |

`cniInstall.otlpEndpoint` and `cniInstall.pinOTLPEndpoint` were removed in chart
`2.4.0` (#1166): the CNI plugin binary exports no telemetry of its own. It reports
its timings and its capture-divert outcome to the agent over the CNI gRPC socket,
and the agent exports `aether_cni_operations_total{aether_cni_operation="capture_divert"}`
and the span attributes with its own telemetry (`otel.*`). A values file that still
sets the two keys is accepted and ignored.

### `udsCsi` — the `csi.aether.io` CSI node plugin (proposal 039)

The **only** carrier for UDS delivery (034) since 039 Phase 2 (chart `2.0.0`),
and **on** by default: `udsCsi.enabled` gates all of UDS delivery — this
DaemonSet and the `CSIDriver`, the agent's `--uds-csi-root` (rendered from
`udsCsi.root`), the proxy's read-only `HostToContainer` mount of `udsCsi.root`
(its only view of workload sockets; the old `/var/lib/kubelet/pods` mount and
`proxy.udsWorkloads` are gone), and the agent's `EndpointPolicy` watch. On a
node without UDS workloads it is an idle 5m/16Mi DaemonSet. A pod declaring
`securityContext.fsGroup: <gid>` and `volumes: [{name: s, csi: {driver: csi.aether.io}}]`
gets a per-pod tmpfs at the volume's `mountPath`, mounted by the plugin at
`<root>/<pod-uid>` on the host: `nosuid,nodev,noexec,nosymfollow`, capped at
`size`, `mode=2770,uid=0,gid=<fsGroup>`. A pod **without** an `fsGroup` is refused
with a `FailedMount` event naming the fix (and, if mesh-managed, denied earlier by the
controller's pod webhook). Renders the `aether-uds-csi` DaemonSet
(privileged; `Bidirectional` propagation on `<kubeletRoot>/pods` and on `root`)
and the cluster-scoped `CSIDriver` (`attachRequired: false`, `podInfoOnMount:
true`, `volumeLifecycleModes: [Ephemeral]`, `fsGroupPolicy: File`). The plugin
serves the kubelet's plugin-registration API itself (no `node-driver-registrar`
sidecar), makes no API calls (no RBAC, no token) and does not involve SPIRE.

| Key | Default | Purpose |
|---|---|---|
| `udsCsi.enabled` | `true` | UDS delivery on the `csi.aether.io` carrier: the DaemonSet, the `CSIDriver`, the agent's `--uds-csi-root`, the proxy's read-only mount of `root`, and the agent's `EndpointPolicy` RBAC/watch. `false` renders none of them and passes `--uds-csi-root=`: pods asking for a socket fall back to TCP, counted as `aether.agent.uds.resolve_failures{reason="disabled"}`. |
| `udsCsi.image.*` | repo+digest placeholders, `pullPolicy: Always` | The slim `uds-csi` image (`quay.io/aethermesh/uds-csi`). |
| `udsCsi.kubeletRoot` | `/var/lib/kubelet` | The kubelet's `--root-dir` — the **only** place it appears on the CSI path: `--kubelet-root`, the CSI socket (`<kubeletRoot>/plugins/csi.aether.io/csi.sock`), the registration socket (`<kubeletRoot>/plugins_registry/csi.aether.io-reg.sock`) and the `pods` dir every target path lives under all derive from it, with hostPath == mountPath. Right for kubeadm, kind and **Talos** (default root); k0s is `/var/lib/k0s/kubelet`, microk8s `/var/snap/microk8s/common/var/lib/kubelet`. Must be absolute (the chart refuses otherwise). |
| `udsCsi.root` | `/run/aether/uds` | Host directory holding the per-pod tmpfs mounts (`<root>/<pod-uid>`). Under `/run` so a reboot starts it empty (the kubelet republishes). The single source of truth for the carrier path: the plugin's `--root`, the agent's `--uds-csi-root` and the proxy's mount all render from it. Sets the socket-file budget: `107 − len("<root>/") − 36 − 1` = **54 bytes** at the default, which is what admission checks. |
| `udsCsi.size` | `1Mi` | Size cap of each per-pod tmpfs (bytes or `Ki`/`Mi`/`Gi`, at most `1Gi`). Pages are charged to the writing app's memory cgroup. |
| `udsCsi.inodes` | `64` | Inode cap (`nr_inodes`) of each per-pod tmpfs: files, sockets and directories, its root directory included (#1107). Without it the kernel default is half the node's RAM pages' worth, far past anything `size` bounds. At least `8`; the chart refuses less. |
| `udsCsi.debug` | `false` | Debug logging for this daemon only; the global `debug` does not reach it. |
| `udsCsi.nodeSelector` / `udsCsi.tolerations` | `{}` / `[]` | Extra scheduling constraints. The `aether.io/agent-not-ready` toleration is always rendered. |
| `udsCsi.resources.{requests,limits}` | requests cpu `5m` / mem `16Mi`; limits mem `32Mi`, **no CPU limit** | No CPU limit by design (#1321, chart 2.4.10; was a `100m` limit). The plugin serves `NodePublishVolume`/`NodeUnpublishVolume`, on the start and termination path of every UDS pod: at `5m`/`100m` it averaged under 1m yet was CFS-throttled in 57.6 % of the 100 ms periods it ran in, because its CPU comes in bursts (a publish, and the 30 s exec liveness probe, a second Go process in the same cgroup). The `5m` request is unchanged: it is several times the measured average and is reserved on every node. QoS was and stays Burstable. Set `limits.cpu` to put a cap back (and see `goMaxProcs`); an empty `limits.cpu` renders no limit. No `GOMEMLIMIT` is set for this container. How to measure throttling: [`runbook.md`](./runbook.md) § *uds-csi CPU throttling*. |
| `udsCsi.goMaxProcs` | `2` | Rendered as the uds-csi container's `GOMAXPROCS` env var (#1321); the exec liveness probe inherits it. The Go runtime derives it from the CPU limit with a floor of 2 — so 2 is what the old `100m` limit gave — and without a limit it would scale to the node's core count. `0` (or empty) omits the env var and lets the Go runtime choose. |

### `registrar`

| Key | Default | Purpose |
|---|---|---|
| `registrar.registryBackend` | `kubernetes` | Backend (`--registry-backend`): `kubernetes` or `etcd`. |
| `registrar.replicaCount` | `2` | Always 2 (exercises the multi-replica write-behind topology). |
| `registrar.topologySpreadConstraints` | `[]` | Empty = chart default: a soft hostname spread across the replicas (`maxSkew: 1`, `ScheduleAnyway`; stacking both on one node was the #628/#629 hot spot). Set to replace the default entirely. |
| `registrar.enableMCS` | `false` | Multi-Cluster Services phase 1 (018 + 006): export `ServiceExport`s and materialize `ServiceImport`s + clusterset VIPs. Requires the etcd backend + the MCS-API CRDs. |
| `registrar.region` | `local` | Region owning this registrar's etcd partition (006); keys are `/aether/v1/regions/<region>/clusters/<clusterName>/…`. One region = one etcd. |
| `registrar.etcd.endpoints` | `[]` | etcd client endpoints (etcd backend). |
| `registrar.peerEtcd` | `[]` | Cross-region replication (006 Phase 2), one entry per peer region: `"<region>=<endpoint>[,<endpoint>...]"`. The leader registrar mirrors this region's own registry subtree verbatim into each peer's etcd under an **origin-heartbeat lease** (TTL ~30s): if this region dies, its mirror expires on the peers — whole-region failover cleanup with no peer-side GC. Requires the etcd backend + a non-default `region`. |
| `registrar.service.{port,targetPort}` | `443` / `8443` | gRPC service ports. |
| `registrar.image.*` / `registrar.resources.*` | placeholders / cpu `100m`, mem `64Mi` (requests = limits) | The CPU limit is kept on purpose (#1321): 1.5 % of CFS periods throttled on the 2026-10-06 soak, not on a request path, and `requests == limits` keeps the pod QoS Guaranteed. `GOMAXPROCS` is derived from `limits.cpu`. |

### `controller`

| Key | Default | Purpose |
|---|---|---|
| `controller.replicaCount` | `2` | Reconcilers and the node-taint guard are leader-elected, but every replica serves the admission webhooks, which are `failurePolicy: Ignore`: with none answering, a pod in an `aether.io/managed` namespace is admitted **unmeshed**. One replica measured a 49 s gap on a leader delete. |
| `controller.injectPodNdots` | `true` | Pod-mutating webhook injects `dnsConfig` ndots into managed pods so mesh FQDNs resolve absolute-first (musl/Alpine safety). Pairs with mesh DNS. |
| `controller.namespaceInjection` | `true` | Namespace auto-injection: a pod in a namespace labeled `aether.io/managed=true` is given the pod label automatically (opt out with `aether.io/managed=false`). |
| `controller.webhook.spire` | `false` | Webhook serving cert source — decoupled from mesh SPIRE. `false` = Helm self-signed CA and certificate (works out of the box), generated once, valid ten years, and reused by every upgrade (the chart looks the Secret up in the cluster). `true` = serve with the controller's SPIRE SVID + inject the trust bundle; no Secret is rendered, and the controller's ClusterRole gains `update` on its webhook configurations: the validating one always, the pod-mutating one whenever it renders (`namespaceInjection` or `injectPodNdots`; `docs/runbook.md`, "The pod-mutating webhook's caBundle is empty"). With `false`, `helm template` has no cluster to look in and generates a new pair on every render: see `docs/runbook.md`, "Rendering the chart reproducibly". |
| `controller.webhook.certRotation` | `""` (never rotate) | Only with `spire=false` (#1364, chart 2.4.16). Set it to any new value (a date works) to generate a new CA and serving certificate on the next upgrade. The value is stamped on the Secret (`aether.io/webhook-cert-rotation`), so later upgrades with the same value reuse the new pair; clearing it rotates nothing and leaves the stamp, so restoring the same value later does not rotate again. After the rotating upgrade the webhooks' `caBundle` holds the new CA and the old one; the controller loads the new certificate from its mounted Secret without a restart, and the next upgrade renders the new CA alone. Not gap-free: the Secret is patched before the webhook configurations, so an upgrade that fails in between leaves the webhooks failing open until it completes (runbook, "Rendering the chart reproducibly"). |
| `controller.webhook.clusterSpiffeID.create` | `true` | When `spire=true`, create the controller's `ClusterSPIFFEID` with the webhook Service DNS SANs. |
| `controller.webhook.clusterSpiffeID.className` | `""` | spire-controller-manager class name; REQUIRED when `create=true`. |
| `controller.webhook.identityGate.enabled` | `true` | Egress identity gate (#1053): the pod-mutating webhook injects the `aether-identity-ready` init container (first in line) into every mesh pod it admits; it holds the app containers until SPIRE has issued the pod's X.509 SVID, so no request leaves before the pod has a client certificate (otherwise `503 UF` for the first seconds). Asks the Workload API over a `csi.spiffe.io` volume mounted into the init container only. Never rendered with `spire.enabled=false`; rides the `/mutate` webhook, so inert unless `namespaceInjection` or `injectPodNdots` is on. Opt a pod out with `aether.io/identity-gate: "false"`. |
| `controller.webhook.identityGate.image.*` | empty = `agent.image` | Image running `/identity-ready` (an extra layer of the agent image, already on every node). |
| `controller.webhook.identityGate.pullPolicy` | `IfNotPresent` | The agent image is digest-pinned and already pulled by the agent DaemonSet. |
| `controller.webhook.identityGate.timeout` | `""` (wait forever) | Go duration after which the init container gives up (exit 1; the kubelet retries with backoff). Empty = fail closed: the pod stays in `Init` until the SVID exists. |
| `controller.webhook.identityGate.resources` | req cpu `5m` mem `16Mi`, limit mem `64Mi` | Init container resources; an empty value leaves that entry unset. |
| `controller.image.*` / `controller.resources.*` | placeholders / requests cpu `50m`, mem `64Mi`; limits cpu `100m`, mem `64Mi` | The CPU limit is kept on purpose (#1321): 1.0 % of CFS periods throttled on the 2026-10-06 soak, and a throttled period adds under 100 ms to an admission call with a 5 s timeout. `GOMAXPROCS` is derived from `limits.cpu`. |

### `edge` — north-south ingress gateway (proposals 003/018/021/028)

An unprivileged Deployment (Envoy + `agent edge`) that dials mesh pods directly
over mTLS and routes external traffic via the Gateway API. Disabled by default.

| Key | Default | Purpose |
|---|---|---|
| `edge.enabled` | `false` | Deploy the edge. |
| `edge.namespace` | `aether-ingress` | The edge runs in its own namespace, isolated from the control plane. |
| `edge.namespaceCreate` | `true` | Let the chart create it (baseline PSA). |
| `edge.replicaCount` | `2` | Gateway replicas (standard RollingUpdate + readiness gate; no hot-restart supervisor). |
| `edge.topologySpreadConstraints` | `[]` | Empty = chart default: a soft hostname spread across the gateway replicas (`maxSkew: 1`, `ScheduleAnyway`). Set to replace the default entirely. |
| `edge.rollingUpdate` | `{}` | Empty = chart default by `replicaCount` (#812): ≥ 2 replicas `maxSurge: 0, maxUnavailable: 1`; exactly 1 `maxSurge: 1, maxUnavailable: 0`. Set to replace it entirely. |
| `edge.gatewayClassName` | `aether` | The `GatewayClass` whose Gateways this edge serves (controller `gateway.aether.io/edge`). Requires the Gateway API CRDs. |
| `edge.gateway.create` | `true` | Chart-manage a `Gateway` of that class (HTTP + optional HTTPS listeners). |
| `edge.gateway.tlsSecretName` / `tlsSecretNamespace` | `""` | The `kubernetes.io/tls` Secret for the downstream cert; REQUIRED when `tls.enabled` + `gateway.create`. |
| `edge.gateway.address` | `""` | Pin the Gateway's LoadBalancer IP (021 Phase 2, via MetalLB). Empty = auto-assign. |
| `edge.gateway.hostname` | `""` | Constrain the chart-managed Gateway's listeners (e.g. `"*.example.com"`). |
| `edge.gateway.httpRoutes` | `[]` | Declaratively managed `HTTPRoute`s parented to the chart Gateway (the supported replacement for hand-applied manifests). |
| `edge.tls.enabled` | `false` | Downstream TLS: HTTPS listener (certs per Gateway listener via SDS) + HTTP→HTTPS redirect. The edge→pod hop stays mTLS. |
| `edge.geoip.enabled` | `false` | Emit `x-geo-*` request headers from a MaxMind DB (028). The `x-geo-*` namespace is always stripped from client requests. |
| `edge.geoip.headers` | `[country]` | Which headers to emit: `country`, `city`. |
| `edge.geoip.database.secretName` / `fileName` | `""` / `GeoLite2-City.mmdb` | The bring-your-own mmdb Secret + key. |
| `edge.xffNumTrustedHops` | `0` | Trusted proxies in front of the edge (feeds HCM client-address + geoip XFF). |
| `edge.httpPort` / `httpsPort` | `80` / `443` | Public listener ports (privileged ports via `NET_BIND_SERVICE`; pod stays unprivileged). |
| `edge.routeNamespace` | `""` | Namespace the edge watches Gateways/HTTPRoutes in. Empty = its own namespace. |
| `edge.service.port` | `80` | HTTP port of the shared edge Service. That Service is always `ClusterIP`: each Gateway gets its own LoadBalancer Service (021 Phase 2), so there is no `type` value. |
| `edge.service.httpsPort` | `443` | HTTPS port of the shared Service; rendered only with `edge.tls.enabled`. |
| `edge.service.extraPorts` | `[]` | Extra Service ports for Gateway TCP/TLS listeners (`TCPRoute`/`TLSRoute`), e.g. `[{name: postgres, port: 5432, protocol: TCP}]`; `protocol` defaults to `TCP`. |
| `edge.drain.preStopSeconds` | `10` | preStop sleep holding off SIGTERM during drain (matches `proxy.hotRestart.drainTime`). 0 disables. |
| `edge.drain.terminationGracePeriodSeconds` | `30` | Must exceed preStop + Envoy drain. |
| `edge.admin.enabled` | `false` | Envoy admin endpoint, bound to `127.0.0.1` only and never exposed via a Service (an unauthenticated control surface). Reach it with `kubectl port-forward`. |
| `edge.admin.port` | `9901` | Loopback admin port. |
| `edge.overload.enabled` | `true` | Envoy overload manager on the `fixed_heap` monitor (shrink heap, then stop accepting requests, before the container memory limit OOM-kills Envoy). |
| `edge.overload.maxHeapSizeBytes` | `201326592` (192Mi) | Keep at ~75% of `edge.resources.limits.memory`. |
| `edge.spire.clusterSpiffeID.create` | `true` | Create the edge's `ClusterSPIFFEID` (when `spire.enabled`) so SPIRE issues the edge pod its SVID. |
| `edge.spire.clusterSpiffeID.className` | `""` | spire-controller-manager class name; required when `create=true` (empty = not rendered; manage the `ClusterSPIFFEID` yourself). |
| `edge.resources.{requests,limits}` | requests cpu `200m` / mem `128Mi`; limits mem `256Mi`, no CPU limit | Applied to **both** containers of the edge pod (`agent` and `envoy`). |
| `edge.goMaxProcs` | `2` | Rendered as the edge `agent` container's `GOMAXPROCS` env var (#1335), the same shape as `agent.goMaxProcs` (#1116). The container has no CPU limit, so the Go runtime would otherwise size itself to the node's core count; it averages under 2m of CPU (4.4m over its busiest 5 minutes on talos-main), so 2 is not a constraint. The `envoy` container is not a Go process. `0` (or empty) omits the env var and lets the Go runtime choose, e.g. when you set an explicit CPU limit. |
| `edge.concurrency` | `2` | Envoy worker thread count for the edge pod's `envoy` container (#1344, chart 2.4.13), the same shape as `proxy.concurrency`: `N > 0` passes `--concurrency N` on the container's command line (the edge runs Envoy directly, with no supervisor), `0` passes no flag and Envoy picks its own default, which is what earlier charts did: the smallest of the node's online CPUs, the CPUs the container's affinity mask allows, and the container's cgroup CPU limit (`edge.resources.limits.cpu`) rounded down to a whole CPU, never less than 1 — one worker per node core only when the container has no CPU limit and no restricted mask. The default is the count the node proxies run with on talos-main; the edge Envoy averages about 12m of CPU there. Raise it for an edge that carries real traffic volume. Changing it is an ordinary rolling update: the edge has no hot restart, so none of `proxy.concurrency`'s caveats apply. A negative or non-integer value fails the render. |

#### `edge.config` — the fleet-default `EdgeConfig` (proposal 029)

This block renders one `EdgeConfig` CR (`edge-defaultconfig.yaml`) that the
chart's `GatewayClass` points at through its `parametersRef`, so **every** Gateway
of that class inherits it. Override per-Gateway with your own `EdgeConfig` via
`Gateway.spec.infrastructure.parametersRef` (override-wins merge). Every field is
optional in the CRD and has a compiled best-practice default — the values below
just surface those defaults for discoverability and tuning.

`edge.xffNumTrustedHops` (above) also feeds this CR's `xffNumTrustedHops`; it
lives outside the block because it is a topology fact shared with the geoip
filter.

| Key | Default | Purpose |
|---|---|---|
| `edge.config.name` | `""` | Name of the generated `EdgeConfig`; empty means `aether-edge-defaults`. |
| `edge.config.useRemoteAddress` | `true` | The edge treats the immediate downstream connection address as the client and manages XFF from it. Correct for an internet-facing edge; XFF is forgeable otherwise. |
| `edge.config.headersWithUnderscoresAction` | `HEADERS_WITH_UNDERSCORES_ACTION_REJECT_REQUEST` | Header-smuggling defence. Canonical enum names (buf `ENUM_VALUE_PREFIX`): `…_ALLOW`, `…_REJECT_REQUEST`, `…_DROP_HEADER`. |
| `edge.config.streamIdleTimeout` | `300s` | Bound on an idle stream (slowloris). |
| `edge.config.requestTimeout` | `300s` | Bound on the whole request; `0s` disables. |
| `edge.config.idleTimeout` | `3600s` | Downstream connection idle timeout. |
| `edge.config.perConnectionBufferLimitBytes` | `32768` (32 KiB) | Listener + edge-cluster buffer cap. |
| `edge.config.http2.maxConcurrentStreams` | `100` | Downstream HTTP/2 concurrent-stream cap (malicious-client protection). |
| `edge.config.http2.initialStreamWindowSize` | `65536` (64 KiB) | Downstream HTTP/2 per-stream flow-control window. |
| `edge.config.http2.initialConnectionWindowSize` | `1048576` (1 MiB) | Downstream HTTP/2 per-connection flow-control window. |
| `edge.config.http3.enabled` | `false` | Add the QUIC/HTTP3 UDP listener on the HTTPS port plus `alt-svc` advertisement (029 M3). |

---

## 2. CRDs (`charts/crds`)

All are `config.aether.io/v1`, **Namespaced**, structural-but-permissive
(`x-kubernetes-preserve-unknown-fields`); authoritative validation is the
controller's protovalidate webhook, not OpenAPI. All carry
`helm.sh/resource-policy: keep`.

| CRD | Kind (short) | Purpose |
|---|---|---|
| `meshconfigs.config.aether.io` | `MeshConfig` (`mc`) | Per-namespace proxy observability overrides (access logs, tracing, per-pod stats). A namespace inherits the control-plane namespace's `MeshConfig` field-by-field unless it sets its own (proposal 015). |
| `httpfilters.config.aether.io` | `HTTPFilter` (`htf`) | The proxy-extension escape hatch (proposal 025): attach a supported Envoy HTTP filter (ext_authz, RBAC, header-to-metadata) at a chosen scope. |
| `edgeconfigs.config.aether.io` | `EdgeConfig` | Edge Envoy tuning (proposal 029): best-practices hardening defaults, HTTP/3 (QUIC, ALPN `h3`), timeouts/limits. Attached natively via Gateway API `parametersRef` on the `GatewayClass` (fleet default) or per-`Gateway` (override-wins merge). |
| `endpointpolicies.config.aether.io` | `EndpointPolicy` | Service-scoped UDS delivery (proposal 034 Phase 1b): `spec.targetRef` (kind=Service, same namespace) + `spec.udsSocket` (`<volume>/<file>`) declares socket delivery for every pod of a service; `<volume>` must be the pods' `csi.aether.io` volume (039), the file at most 54 bytes. The per-pod `endpoint.aether.io/uds-socket` annotation wins; one policy per Service (lexicographically smallest name wins). Read by the agent only when `udsCsi.enabled` (default). |

**`HTTPFilter` scopes** (`spec.scope`, plus the `target_refs` attachment):

| Scope | Attachment | Applies to |
|---|---|---|
| `SCOPE_ROUTE` (default) | Gateway API **ExtensionRef** on an HTTPRoute/GRPCRoute rule | that single route |
| (targetRef) | `spec.targetRefs` (kind=Service) | every route of the Service |
| `SCOPE_CHAIN` | Service targetRef, always-on | the service's capture vhost (one per service; rides the 026 cross-cluster channel) |
| `SCOPE_INBOUND` | Service targetRef, destination-side | the target service's own pods' inbound listeners (not propagated cross-cluster) |

---

## 3. CLI flags

Every binary also gets the shared **manager** flags
(`common/manager/flags.go`): `--debug`, `--metrics-enabled`,
`--metrics-bind-address`, `--otel-enabled`, `--otlp-endpoint`, `--logs-enabled`,
`--trace-sample-rate`, `--trace-export`. (The chart sets these from the values
above.)

### `agent` (node agent — root command)

Identity/registrar/SPIRE: `--mesh-config` (`/etc/aether/mesh-config.yaml`),
`--mesh-domain` (`aether.internal`), `--spire-enabled` (`true`), `--node-name`
(required; doubles as the xDS node identity — the old `--proxy-id` was retired),
`--cluster-name` (required),
`--registrar-address` (`aether-registrar.aether-system.svc:443`),
`--spire-workload-socket`, `--spire-wait-warn-after` (`2m`).

`--spire-wait-warn-after` (also on `registrar`, `controller` and `agent edge`;
chart key `spire.waitWarnAfter`) is how long the wait for this workload's first
SVID stays at INFO before the waiting log line escalates to WARN. Since #740 no
component dies on a missing SPIRE — it retries in the background — so this is a
*logging* threshold everywhere except the node agent, where it doubles as the
dwell before the `spire-svid` readiness check reports NotReady. See
[`runbook.md`](./runbook.md) § *The agent is stuck waiting for SPIRE*.

Node-agent-specific:

| Flag | Default | Purpose |
|---|---|---|
| `--mounted-registry-dir` | `/host/var/lib/aether/registry` | Local pod-data dir for the CNI plugin. |
| `--node-lock` | `/run/aether/agent.lock` | Node-ownership lock (proposal 041). The agent that owns the node holds an exclusive `flock` on it for its life. An agent that finds it taken (a surge roll) is a standby: it builds its first snapshot but binds neither `xds.sock` nor `cni.sock` and writes no node file (observed upstreams, mesh-DNS snapshot, conflist repair, storage) until the lock is released. It then applies the CNI ADD/DEL served meanwhile and binds. Must be on a host path every agent pod on the node shares. Empty disables the lock. |
| `--health-socket` | `""` | Serve `/healthz` and `/readyz` on this Unix socket for the `agent-ready` exec probe (041). Must be in the pod's own filesystem (the chart uses `/tmp/aether-agent-health.sock` in the `tmp` emptyDir), so the kubelet is never answered by a surge peer on the same hostNetwork node. Empty serves no socket. |
| `--health-probe-bind-address` | `:8082` | TCP `/healthz` and `/readyz`. `0` disables it; the chart passes `0` and probes the socket. Metrics (`--metrics-bind-address`, `:8080`) bind only once the agent owns its node. |
| `--uds-csi-root` | `/run/aether/uds` | Host directory under which the `csi.aether.io` plugin mounts each UDS pod's tmpfs, mounted into the proxy at the identical path; the proxy dials `<root>/<pod-uid>/<file>` (034/039). Must match the plugin's `--root` (the chart renders both from `udsCsi.root`). Empty disables UDS delivery: pods requesting a socket fall back to TCP loopback (`resolve_failures{reason="disabled"}`). Replaced `--kubelet-pods-dir` (039 Phase 2). |
| `--spire-broker-socket` | `/run/spire/broker-sockets/broker.sock` | SPIRE agent's SPIFFE Broker Endpoint socket, over which the agent brokers an X.509-SVID for every pod on its node (036). Replaced `--spire-admin-socket`; requires SPIRE >= 1.15.2 with its experimental broker enabled. |
| `--gamma` | `true` | GAMMA east-west routing (018); default-on kill switch (031). CRD-detected. |
| `--cni-conflist-reassert` | `true` | Re-assert the chained `aether-cni` entry in the node's active CNI conflist whenever a competing writer strips it (#645). Watches `--mounted-cni-net-dir` (fsnotify) plus a 60s re-check; only ever appends to an existing, valid conflist that still carries a primary CNI plugin. |
| `--mounted-cni-net-dir` | `/host/etc/cni/net.d` | Host CNI config dir as mounted into the agent (read-write) for the re-assert loop. |
| `--import-config` | `false` | Enable cross-cluster config import (026). |
| `--control-cluster` | `""` | Trust imported config ONLY from this origin (026 EM3). Empty = federated. |
| `--east-west-waypoint` | `false` | Per-node east/west waypoint for cross-cluster traffic (019); tunnel port is the fixed constant 18009. |
| `--east-west-quic-pair-fetch-window` | `1h` | How long after the agent starts a **persisted** (source ServiceAccount, destination) QUIC pair with **no evidence of use** is kept before the pair and its twin are pruned (issues #1033, #1073). Evidence of use is an on-demand fetch of the `quic:` twin, an on-demand subscription for it, or the proxy re-stating the twin as held on a fresh xDS stream, in this or any earlier agent process (`demand_confirmed` in the persisted set). A pair with any of it is never pruned by this window. A pruned pair that still carries traffic is re-fetched on its next request (one ODCDS round trip). `0` disables the prune. Not exposed in the chart; the default applies. See [`runbook.md`](./runbook.md) § *East-west QUIC (proposal 038 Phase 4)*, the "Post-start prune of pairs with no evidence of use" paragraph. |
| `--east-west-quic-idle-timeout` | `8s` | Idle timeout of each `quic:` twin's upstream connection pool (#1054); h1/h2 keep 30 s. Must be `> 0`. Keep it at least 5 s below the proxy's `--parent-shutdown-time`: the agent cannot check that (it never sees the proxy DaemonSet), so the chart does — `agent.eastWestQuicIdleTimeout + 5s < proxy.hotRestart.parentShutdownTime` or the chart fails to render. The twin's QUIC transport idle (8 s, #1087) closes a connection with no open stream after 8 s anyway, so values above 8 s no longer lengthen reuse. |
| `--mesh-dns` | `false` | Per-pod mesh DNS (018): answer `<svc>.<ns>.<mesh-domain>` from the generated mesh Services. Upstream forwarding belongs to the `mesh-dns` daemon, not the agent. |
| `--mesh-dns-snapshot-path` | `/host/var/lib/aether/registry/mesh-dns/records.json` | Host-persistent record table the in-process resolver writes and warm-loads at boot (and the `mesh-dns` daemon watches). Under the CNI registry hostPath so it survives a rolling restart; empty disables persistence. |
| `--authz-sidecar` | `false` | Node-local ext_authz sidecar entry (027). |
| `--authz-sidecar-timeout` | `200ms` | Per-check gRPC timeout. |
| `--authz-sidecar-failure-mode-allow` | `false` | Fail-open (default: fail-closed). |

> The chart's booleans (`agent.gamma`, `agent.meshDns`,
> `agent.captureRedirectAllDefault`, …) map to these flags. Transparent
> capture, the redirect-all passthrough chain, L4 route types, and the
> startup-taint removal are unconditional since proposal 031 (no flags).

### `agent edge` (subcommand)

Inherits the manager + identity flags (but `--node-name` is relaxed, derived
from `POD_NAME`). Adds: `--edge-http-port` (`80`), `--edge-https-port`
(`443`), `--edge-tls` (`false`), `--gateway-class` (`aether`),
`--route-namespace` (`""` — the default namespace for Gateway TLS Secrets;
watching is cluster-wide), `--edge-service-name` (`""`), `--geoip-city-db`
(`""`), `--geoip-headers` (`[country]`), `--xff-num-trusted-hops` (`0`).
The readiness listener is the fixed port 18021 (030 constant), per-Gateway
addressing is unconditional (021 Phase 2), and the empty local store lives at
the fixed pod-local path.

### `proxy-supervisor` (standalone binary — the `aether-proxy` container's PID 1)

The Envoy hot-restart supervisor (proposal 001). It was `agent proxy-supervisor`
until #772: as a subcommand it made the proxy pod stage and run the whole 65MiB
agent binary — controller-runtime, client-go, go-control-plane, SPIRE, Gateway
API, miekg/dns — to fork a child process. It is now its own binary
(`//agent/cmd/proxy-supervisor`, 15MiB / 24 modules) in its own image
(`quay.io/aethermesh/proxy-supervisor`), which also means the proxy
DaemonSet no longer depends on the agent image at all. The `agent
proxy-supervisor` alias that bridged the split for one release has been removed,
along with the `/proxy-ready` copy in the agent image that only it read.

Flags (every name and default unchanged by the split — the chart passes them
literally): `--envoy-path`
(`/usr/local/bin/envoy`), `--config` (`/etc/envoy/envoy.yaml`), `--base-id` (`0`),
`--drain-time` (`45s`), `--parent-shutdown-time` (`60s`), `--watch-config`
(`true`), `--state-dir` (`/run/aether/hotrestart`), `--ready-marker`, `--envoy-arg`
(repeatable), `--handoff-deadline`/`--admin-unresponsive-deadline` (`0` = defaults),
`--termination-grace` (`0`), `--shutdown-drain-immediately` (`false`),
`--admin-address` (`127.0.0.1:9901`), `--install-path`,
`--install-readiness-path`, `--otlp-endpoint`.

`--envoy-arg` cannot repeat an Envoy flag the supervisor passes itself: `-c` /
`--config-path` (set from `--config`), `--base-id` (from `--base-id`, chart
`proxy.hotRestart.baseId`), `--drain-time-s` (from `--drain-time`, chart
`proxy.hotRestart.drainTime`), `--parent-shutdown-time-s` (from
`--parent-shutdown-time`, chart `proxy.hotRestart.parentShutdownTime`),
`--restart-epoch`, `--admin-address-path` and `--mode`. Envoy refuses a flag
given twice (`Argument already set!`), so each of these is a startup error that
names the flag instead of a failure of every fork.

The supervisor checks `--envoy-arg` once, before the first fork, against what the
pinned Envoy was measured to do. Besides the flags above it refuses:

- **A flag and its value in one item.** The pinned Envoy accepts one spelling
  only, the flag in one argument and its value in the next. `--concurrency=2`,
  `--service-node=n1`, `-l=info` and `-linfo` all answer `Couldn't find match for
  argument`. Write `--envoy-arg=--concurrency --envoy-arg=2`, as the chart does.
  The error shows the two items for the argument it refused. A flag that takes no
  value goes alone: `--skip-hot-restart-parent-stats=true` is refused too, and the
  fix is the single item `--envoy-arg=--skip-hot-restart-parent-stats` (a second
  item `true` would be refused by Envoy). The error says so for the pinned Envoy's
  valueless flags.
- **Flags that break a handoff or stop Envoy from serving.** The supervisor does
  not pass these, so they are not repeats, and Envoy would start:

  | Flag | Why it is refused |
  |---|---|
  | `--use-dynamic-base-id` | Envoy ignores the fixed `--base-id` and picks a random one, so no successor finds its shared memory and hot-restart socket. Envoy also refuses the flag at any restart epoch above 0, which is every hot restart. |
  | `--disable-hot-restart` | A successor never contacts its predecessor: the old Envoy is neither drained nor stopped, and no socket is handed over. |
  | `--socket-path` | The two Envoys of a handoff meet on Envoy's default hot-restart socket, an abstract one in the host network namespace. If they disagree on the path the successor loops on `connection refused`, and a path on a pod's own filesystem is not shared with the next pod. |
  | `--hot-restart-version`, `--version`, `-h` / `--help` | Envoy prints and exits 0 without serving, on every fork. |
  | `--` / `--ignore_rest` | Envoy ignores every argument after it, while the supervisor still reads them (a `--concurrency` behind it would be compared with a predecessor's worker count and never applied). |

- **A flag the chart passes, given twice.** `--concurrency`, `-l` / `--log-level`,
  `--service-cluster`, `--service-node`, `--service-zone`, `--drain-strategy` and
  `--skip-hot-restart-parent-stats` are allowed once: the chart passes each of
  them (`--concurrency` for `proxy.concurrency`, `--skip-hot-restart-parent-stats`
  for `proxy.hotRestart.skipParentStats`), and Envoy refuses any flag given twice
  except `--stats-tag`.
- **A `--concurrency` whose value Envoy cannot use.** The value is the next item,
  whatever it looks like. A missing value, a non-number (`x`, `1.5`, `0x2`), a
  number above 4294967295 and another flag standing in its place are refused, as
  Envoy refuses them. Four kinds of value Envoy accepts are refused too: an empty
  one (Envoy runs its default worker count as if the flag were absent), a
  negative one (Envoy reads `-1` as 4294967295 workers), one with a sign or a
  space (`+2`, ` 2`) and one above 2147483647 (no Envoy starts that many
  workers). `--concurrency 0` is accepted: Envoy runs **one** worker for it,
  not its default count, and the supervisor counts it as 1 when it compares worker
  counts at a handoff. (The chart never renders it: `proxy.concurrency: 0` passes
  no flag.)

Other hot-restart flags are passed through. `--base-id-path` only writes the base
id Envoy uses to a file (with the fixed `--base-id` the file holds that number at
every epoch, and a handoff completes). `--skip-hot-restart-on-no-parent` only
changes what a child does when its parent is already gone. `--socket-mode` is not
read while the socket is the abstract default. `--cpuset-threads` does nothing in
the pinned Envoy.

Limits of the check. It does not carry Envoy's flag table, so it does not know
which flags take a value, and it compares every item. A value that is itself
spelled like a refused argument (a `--service-node` named `-c`, a `--log-format`
that is `--x=y` or starts with `-l`) is refused although Envoy would take it. A
flag in none of the lists above is passed through unchecked: a misspelled one, or
one given twice, still fails at the fork.

`--termination-grace` is the pod's own `terminationGracePeriodSeconds` (the chart
passes the same value it sets on the pod spec; `180s` as deployed). The supervisor
cannot read it from the API, and it is the only non-arbitrary bound on how long a
SIGTERM may wait for a successor before the kubelet's SIGKILL settles the matter.
The wait budget is `terminationGrace − (drainTime + 5s) − 10s`, i.e. 155s at the
deployed values; `0` means "unknown", which keeps the wait unbounded (the
pre-#771 behaviour, and what every chart predating the flag gets).

`--shutdown-drain-immediately` is the escape hatch for a deployment where no
replacement pod can overlap this one — a DaemonSet without `maxSurge`, or a
single-instance proxy. **Leave it off wherever a surge replacement exists.** On
SIGTERM with Envoy still serving at our own epoch, the default is to keep serving
and wait (bounded as above) for the replacement's Envoy to hot-restart ours; that
wait is the entire reason `kubectl delete pod` — and therefore node drain,
eviction and preemption — costs the node's data plane nothing. Turning it on
trades that for a prompt, still-graceful drain: `POST /drain_listeners?graceful`,
wait `--drain-time`, then SIGTERM. It never restores the bare SIGTERM that #795
was filed for. The chart does not set it.

`--install-path` and `--install-readiness-path` are the initContainer's staging
mode: the first self-copies this binary to the shared volume as the supervisor,
the second copies the bundled `/proxy-ready` prober out of the same image. A
requested `--install-readiness-path` against an image that does not carry one is
a hard failure, so image skew surfaces in the initContainer rather than as a pod
that can never become Ready. Since #772 both binaries ship in the
`proxy-supervisor` image and are built from one commit, so that skew is no longer
reachable through the supported path; the check stays as a guard.

The supervisor is not its own readiness probe. The pre-#673 `--readiness-check`
exec-probe mode (re-execing a supervisor binary every 2s per pod spent >=31% of
the supervisor container's CPU on Go package init alone — measured when that
binary was the agent's 67MB one) was deprecated by #673 and has been removed;
the chart execs the standalone `proxy-ready` binary below.

### `proxy-ready` (standalone binary — bundled in the proxy-supervisor image, not run from it)

The `aether-proxy` pod's exec readiness probe (#673). One flag: `--ready-marker`
(`/var/run/aether-proxy/ready`); exit 0 iff that path stats. It is deliberately
stdlib-only (~1.7MB vs the agent's 67MB) — it imports nothing but
`common/readymarker`, and `//agent/cmd/proxy-ready:deps_test` fails the build if
that ever changes. It ships as an extra layer in the `proxy-supervisor` image (no
second pull: the `install-supervisor` initContainer, which already runs that
image, copies it onto the proxy pod's shared volume at
`/opt/aether/proxy-ready`). It is not in the agent image: the copy there
served only the removed `agent proxy-supervisor` alias.

The probe stays an **exec** probe on the pod-local marker rather than an
`httpGet`/`tcpSocket`: the proxy DaemonSet is `hostNetwork: true` with
`maxSurge: 1`, so predecessor and successor share the host netns for the whole
handoff and no port-based check is provably pod-local (the reason #582 was closed
for `mesh-dns`). Envoy's admin endpoint is not an equivalent target either — a
draining hot-restart parent answers `LIVE` at its old epoch for the entire
`--parent-shutdown-time-s` window (proposal 001, lesson 6), and the supervisor
deliberately holds readiness while it is still the serving parent.

### `mesh-dns` (standalone daemon — its own binary and image)

The `aether-mesh-dns` DaemonSet (#578, #583). It answers `<svc>.<ns>.<mesh-domain>`
from the snapshot file the agent writes and forwards everything else upstream, so
agent rolls never gap pod DNS. It does **not** share the agent's flag set:
`--snapshot-path` (`/host/var/lib/aether/registry/mesh-dns/records.json`),
`--mesh-domain` (`aether.internal`), `--mesh-dns-upstream` (repeatable,
`host[:port]`; empty = `/etc/resolv.conf`), `--ready-marker`
(`/run/aether/mesh-dns.ready`),
`--forward-pool-size` (`8`), `--lame-duck-max` (`10s`), `--otlp-endpoint`,
`--debug`.
It binds UDP+TCP on the host at port 18054, which the CNI DNATs each managed
pod's `:53` to.

`--lame-duck-max` (#729) is the longest this resolver keeps **serving** after
SIGTERM before closing its `SO_REUSEPORT` listeners. It stops reporting ready
immediately and closes as soon as it observes a successor answering on the same
address (a TXT identity stamp carrying someone else's value proves the successor
is both in the reuseport group and serving), so the ceiling only applies when no
successor appears — a scale-down, a node drain, or a failed surge. The chart
derives the DaemonSet's `terminationGracePeriodSeconds` as this value + 5s, so
raising it cannot silently leave the kubelet SIGKILLing mid-window. `0` disables
the window and closes on SIGTERM, which dropped one queued datagram on every
roll before #729. Grade a roll on
`aether_mesh_dns_lame_duck_exits_total{reason}` — a healthy roll is `successor`
on every node (see [`runbook.md`](./runbook.md) § *Grading the mesh-DNS lame-duck
handoff across a roll*).

The daemon is not its own readiness probe. The pre-#683 `--readiness-check`
exec-probe mode (re-execing this 16.9MB daemon every 15s per pod spent ~10
core-seconds per 25 minutes fleet-wide, ~3-4% of the container's CPU, on container
exec and Go package init alone) was deprecated by #683 and has been removed; the
chart execs the standalone `mesh-dns-ready` binary below.

`--debug` only raises the log level (Info to Trace); it gates no feature and no
data-path behaviour. The mesh-DNS forward path logs **nothing per query** at any
level — the resolver's only Debug-level call site is the snapshot reload. The
chart nonetheless defaults it **off** for this daemon (#684) via its own
`agent.meshDnsDaemon.debug` key: the global `debug: true` deliberately no longer
reaches mesh-dns, because every record it emits is also fanned out to the OTLP
log exporter and this is the one component on every managed pod's `:53` path.
Turn it on for a diagnosis with `--set agent.meshDnsDaemon.debug=true`.

### `mesh-dns-ready` (standalone binary — bundled in the mesh-dns image)

The `aether-mesh-dns` pod's exec readiness probe (#683). One flag:
`--ready-marker` (`/run/aether/mesh-dns.ready`); exit 0 iff that path stats. It
is deliberately stdlib-only (~1.7MB vs the daemon's 16.9MB) — it imports nothing
but `common/readymarker`, and `//agent/cmd/mesh-dns-ready:deps_test` fails the
build if that ever changes. It ships as an extra layer in the mesh-dns image, the
image the DaemonSet already runs, so there is no second pull and **no chart/image
skew is possible**: the prober and the daemon that writes the marker are the same
artifact.

Like `proxy-ready`, it stays an **exec** probe on the pod-local marker rather
than an `httpGet`/`tcpSocket`: this DaemonSet is `hostNetwork: true` with
`maxSurge: 1`, so predecessor and successor share the host netns for the whole
handoff and a port-based check could be answered by the peer pod's SO_REUSEPORT
socket. That is precisely what #582 proposed and why it was closed abandoned.

### `agent-ready` (standalone binary — bundled in the agent image, the agent pod's exec probe)

The `aether-agent` container's liveness and readiness probe (proposal 041). It
dials the agent's `--health-socket`, sends one `GET <path>?verbose`, and exits 0
iff the agent answers 200. On failure it prints the response body, which names
the failing check (the kubelet records it in the probe event). Stdlib-only and
without `net/http`. `//agent/cmd/agent-ready:deps_test` asserts that the linked
binary's build info lists no module at all: it is exec'd every 2 s on every node.

| Flag | Default | Purpose |
|---|---|---|
| `--socket` | `/tmp/aether-agent-health.sock` | The agent's health socket. |
| `--path` | `/readyz` | `/readyz` (readiness) or `/healthz` (liveness). |
| `--timeout` | `1s` | Bound on the whole exchange. |

### `identity-ready` (standalone binary — bundled in the agent image, run as an injected init container)

The egress identity gate (#1053): the controller's `/mutate` webhook injects it as
the `aether-identity-ready` init container (first in line) into every mesh pod it
admits (#1055, `--identity-gate`). It asks the SPIRE agent's Workload API — over a
`csi.spiffe.io` volume mounted into this init container only — for the pod's own
X.509 SVID and exits 0 once SPIRE has issued it, so the app containers never start
before the node proxy has a client certificate for the pod. It ships as an extra
layer (`/identity-ready`) in the agent image, already on every node; it links gRPC,
protobuf and go-spiffe's generated Workload API client and nothing heavier, and
`//agent/cmd/identity-ready:deps_test` keeps it that way.

| Flag | Default | Purpose |
|---|---|---|
| `--spire-workload-socket` | `/run/secrets/workload-spiffe-uds/socket` | Workload API socket (the `csi.spiffe.io` mount). |
| `--timeout` | `0` | Exit non-zero after this long without an SVID; `0` waits forever (fail closed: the pod stays in `Init`). |
| `--retry-interval` | `500ms` | Pause between Workload API fetch attempts. |
| `--attempt-timeout` | `15s` | Upper bound on one fetch attempt. |
| `--log-every` | `10s` | How often it logs what it is still waiting for. |

The controller sets `--spire-workload-socket` and `--timeout` from its
`--identity-gate-*` flags. See [`runbook.md`](./runbook.md) § *Pod held in Init by
aether-identity-ready (#1053)*.

### `uds-csi` (standalone binary — the `aether-uds-csi` DaemonSet, proposal 039)

The `csi.aether.io` CSI node plugin. Stdlib `flag` parsing; no Kubernetes client,
no telemetry exporter (`//agent/cmd/uds-csi:deps_test`,
`scripts/check-uds-csi-deps.sh`).

| Flag | Default | Purpose |
|---|---|---|
| `--kubelet-root` | `/var/lib/kubelet` | The kubelet's `--root-dir`. Every `target_path` must lie under `<kubelet-root>/pods/<pod-uid>/`, and the socket defaults derive from it. |
| `--csi-socket` | `<kubelet-root>/plugins/csi.aether.io/csi.sock` | CSI Identity + Node endpoint; also the `endpoint` reported to the kubelet, so it must be the path the kubelet sees. |
| `--registration-socket` | `<kubelet-root>/plugins_registry/csi.aether.io-reg.sock` | The kubelet plugin-registration endpoint (served by this binary, not a sidecar). |
| `--node-id` | `$NODE_NAME` | `NodeGetInfo`'s node ID. |
| `--root` | `/run/aether/uds` | Host directory holding the per-pod tmpfs mounts. |
| `--size` | `1Mi` | Size cap of each per-pod tmpfs. |
| `--inodes` | `64` | Inode cap (`nr_inodes`) of each per-pod tmpfs, its root directory included; at least `8`. |
| `--debug` | `false` | Debug logging. |
| `--probe` | `false` | Liveness mode: exit 0 iff the CSI socket exists, then exit. |

Both sockets are created at start (stale socket files are removed first; a
non-socket at either path is refused) and removed on SIGTERM, the registration
socket first so the kubelet deregisters the driver before its endpoint stops
answering. A kubelet-reported registration error exits the process non-zero.

### `registrar`

`--cluster-name` (required), `--mesh-domain` (`aether.internal`),
`--control-cluster` (`""`), `--region` (`""`), `--registry-backend`
(`kubernetes`), `--etcd-endpoints` (`[localhost:2379]`), `--peer-etcd`
(repeatable, `<region>=<endpoint>[,<endpoint>...]` — cross-region replication,
006 Phase 2; requires the etcd backend + an explicit `--region`),
`--sync-interval` (`5s`), `--enable-mcs`
(`false`), `--grpc-address` (`:8443`), `--spire-enabled` (`true`),
`--spire-workload-socket`. The mesh-Service generator is unconditional (031
round 2), and the mTLS peer trust domain is resolved from the registrar's own
SVID (no `--spire-trust-domain`).

### `controller`

`--mesh-config-configmap` (`aether-mesh-config`), `--spire-enabled` (`false`),
`--spire-workload-socket`, `--webhook-config-name` (`""`),
`--mutating-webhook-config-name` (`""`), `--mesh-domain` (`aether.internal` —
the pod-mutating webhook derives its injected ndots from the domain's label
count; the old `--pod-ndots` was retired).

The egress identity gate (#1053/#1055), rendered from
`controller.webhook.identityGate.*`:

| Flag | Default | Purpose |
|---|---|---|
| `--identity-gate` | `false` (the chart sets it) | Inject the `aether-identity-ready` init container into mesh-managed pods on `/mutate`, holding their app containers until SPIRE has issued the pod's SVID. Opt a pod out with `aether.io/identity-gate=false`. |
| `--identity-gate-image` | `""` | Image the init container runs `/identity-ready` from (the agent image). Required with `--identity-gate`. |
| `--identity-gate-image-pull-policy` | `IfNotPresent` | `Always`, `IfNotPresent` or `Never`. |
| `--identity-gate-workload-socket` | `/run/secrets/workload-spiffe-uds/socket` | Workload API socket path inside the init container; the `csi.spiffe.io` volume is mounted at its directory. |
| `--identity-gate-timeout` | `0` | Give up after this long (the init container exits 1 and the kubelet retries it); `0` waits forever (fail closed). |
| `--identity-gate-cpu-request` / `--identity-gate-cpu-limit` | `5m` / `""` | Init container CPU; empty leaves that entry unset. |
| `--identity-gate-memory-request` / `--identity-gate-memory-limit` | `16Mi` / `64Mi` | Init container memory; empty leaves that entry unset. |

### `cni-install` (init container)

`--cni-bin-dir`, `--cni-bin-target-dir`, `--mounted-cni-net-dir`,
`--capture-redirect-all-default`,
`--mesh-dns`, `--host-ip`, `--debug`. The per-pod capture redirect is
unconditional (no `--transparent-capture`; per-pod `capture.aether.io/*`
annotations opt out). `--otlp-endpoint` and `--otlp-pin-endpoint` are deprecated
no-ops since #1166 (the plugin exports no telemetry), kept parseable for one release
so an older chart still starts a newer image. (The `cni` plugin binary itself is
configured via CNI-spec stdin, not flags.)

Netconf keys the plugin reads but `cni-install` does not write (edit the conflist to
override a default): `netns_pin_disabled`, `netns_pin_dir` (`/run/aether/netns`),
`netns_unpin_delay_seconds` (`0` = 60s), `readiness_probe_disabled`, and
`netns_del_give_up_after_seconds` — how long CNI DEL keeps failing back to the runtime
when a *reachable* agent answers the removal with an error before it degrades to the
agent-unreachable path (unpin on the normal delay, return success, let the ghost sweep
reconcile). `0` = 5m default; negative = give up on the first failure (#796).

### `prober` (standalone chart `charts/prober`, proposal 013)

The synthetic **mesh-availability prober**: a per-node DaemonSet, mesh-managed
like any other client, that black-box probes the data plane from the *client*
side and emits its own pass/fail SLI. It exists because a source proxy cannot
report its own outage, so the proxy-emitted `aether_stats` metric is structurally
blind to the connection-level failures a hot restart can produce. It has its own
chart and its own image (`//prober/cmd/prober`) and is installed independently of
the `aether` chart. It does **not** take the shared manager flags; the list below
is its whole flag set.

| Flag | Default | Purpose |
|---|---|---|
| `--egress` | `127.0.0.1:18081` | Local mesh egress listener the prober dials (the per-pod listener the CNI plumbs). |
| `--liveness-path` | `/-/-/live` | Proxy local-reply liveness route. The agent programs a `direct_response` 200 on this exact path, first in the catch-all vhost — no upstream, no app — so a failure is unambiguously the mesh's fault. |
| `--liveness-authority` | `liveness.aether.internal` | Reserved `Host` authority for the liveness probe; must not be a real service, so the request lands on that catch-all vhost. |
| `--mesh-domain` | `aether.internal` | Mesh authority suffix for the reachability tier. |
| `--reachability-targets` | `[]` | Echo upstream service names for the `reachability` tier. Empty disables the tier. |
| `--mesh-dns-targets` | `[]` | Namespace-qualified FQDN authorities (`host[:port]`, default port `18081`) for the `mesh_dns` tier. Unlike the other two tiers these are **resolved** through the mesh DNS path rather than Host-overridden, so a real mesh-DNS outage becomes an alertable `dns_*` result (#574). Probed on a no-keep-alive client, so every probe resolves and dials afresh. |
| `--rate` | `5` | Probes per second, per target. |
| `--timeout` | `2s` | Per-probe timeout. |
| `--max-concurrent` | `16` | Max in-flight probes per target; a tick that finds the semaphore full records `saturated` instead of probing. |
| `--otlp-endpoint` | `""` | OTLP gRPC collector `host:port` (insecure). Empty disables telemetry — the prober still runs but emits nothing. |

**Metrics.** Two instruments, both carrying `tier` (`liveness`, `reachability`,
`mesh_dns`), `target` (the probed name), `result` and `pod` (the prober pod, from the
resource's `k8s.pod.name`; #1041):

| Metric | Type | Notes |
|---|---|---|
| `aether_probe_requests_total` | counter | `result` is one of `success`, `http_error`, `connection_error`, `timeout`, `saturated`, plus — `mesh_dns` tier only — `dns_error`, `dns_nxdomain`, `dns_timeout`. A resolution failure is an independently alertable signal; a post-resolution connect failure stays `connection_error`. A probe deadline that interrupts the name lookup is `dns_timeout`; a deadline in any later phase is `timeout` (#1252). |
| `aether_probe_request_duration_seconds` | histogram | Explicit **seconds**-valued buckets (`0.001` … `5`). They have to be set explicitly: the SDK's default boundaries are tuned for millisecond-valued durations, so against seconds the first bucket is `<= 5s` and a healthy 2 ms probe is indistinguishable from a timed-out 2 s one (#732). |

Per-node identity is a **resource** attribute, not a metric label: the chart sets
`OTEL_RESOURCE_ATTRIBUTES=k8s.node.name=$(NODE_NAME),…` and the prober's resource
builder reads it from the environment, so the series de-collapse per node once
the collector promotes it to `node` (#210). The prober deliberately sets **no**
`host.name`. On a pod without hostNetwork that is the pod name, and a collector that
promotes `host.name` ahead of `k8s.node.name` would export `node="prober-xxxxx"`, which
is what happened until #1041.

**Failure log.** Every non-success probe prints one bounded
`AETHER_PROBE_FAIL {t, tier, target, result, err, elapsed_ms, phase, reused, conn_ms, dns_ms, connect_ms, tls_ms, write_ms, ttfb_ms, pod, node, n, truncated}`
line to stdout: at most 20 per `(tier, result)` per minute, then one summary line with
the `suppressed` count (#1040). `phase` and the `*_ms` fields come from a per-probe
`httptrace` trace and say which step of the request the time went to (#1252; `-1` =
the phase never started). See [`runbook.md`](./runbook.md), "Attributing a prober
failure".

**Deployment.** The chart renders a DaemonSet + ServiceAccount into a namespace
that must already be mesh-managed — the probe only works if the CNI has plumbed
the pod's egress listener. `namespace.create` is `false` and `namespace.name`
defaults to the release namespace; on talos-main that is `aether-test`. The pod
carries `aether.io/managed: "true"`, and the chart derives
`config.aether.io/upstreams` from the union of the reachability and `mesh_dns`
targets so the agent programs those clusters (proposal 004). It ships **no CPU
limit** on purpose: the limit is a CFS quota, which quantises probe latency in
~100 ms steps, and at the previous `50m` limit that was the dominant term in the
published SLI (#735). The memory limit stays — `GOMEMLIMIT` is rendered at 90 % of it (chart 1.0.2; it used to be the whole limit).

**Resources.** Three containers, three values keys (chart 1.0.4):

| Key | Default | Container |
|---|---|---|
| `resources.{requests,limits}` | requests cpu `100m` / mem `32Mi`; limits mem `64Mi`, **no CPU limit** | `prober`, the DaemonSet's only container (see above for why it has no CPU limit). |
| `authzCanary.resources.{requests,limits}` | requests cpu `10m` / mem `16Mi`; limits mem `32Mi`, no CPU limit | `curl`, the canary client (Deployment `authz-canary`). Only rendered with `authzCanary.enabled`. Written into the template before chart 1.0.4 (#1362). |
| `authzCanary.echo.resources.{requests,limits}` | requests cpu `10m` / mem `32Mi`; limits mem `64Mi`, no CPU limit | `echo`, the canary's target (Deployment `authz-echo`, `authzCanary.echo.replicas` pods). Only rendered with `authzCanary.enabled`. Written into the template before chart 1.0.4 (#1362). |

To remove a default request or limit, set it to `null` or to an empty value
(`--set resources.limits.memory=null`, `--set authzCanary.resources.requests.cpu=`,
or `limits: {memory: null}` in a values file): the key is left out of the pod
spec. Before chart 1.0.4 an emptied quantity on `resources` rendered as
`cpu: ""`, which the apiserver rejects (#1361). Removing the prober's
`limits.memory` also removes its `GOMEMLIMIT`. To check a release's values
against a chart before upgrading, read them back and render (never
`--reuse-values`):

```sh
helm get values prober -n <namespace> -o yaml > values.yaml
helm template prober <chart> -n <namespace> -f values.yaml | grep -n -E '^[[:space:]]+(cpu|memory): (""|null)?$'
```

No output means no container carries an empty quantity.

**Images of the authz canary** (only rendered with `authzCanary.enabled`):

| Key | Default | Notes |
|---|---|---|
| `authzCanary.image` | `curlimages/curl@sha256:58adaa4e…6777` (the multi-arch index of `curlimages/curl:8.22.0`) | The client: needs `/bin/sh`, `curl` and `date`. Pinned by digest since chart 1.0.5 (#1374); it was the tag. Nothing refreshes the pin automatically: see [`runbook.md`](./runbook.md), "The prober chart". The container has no liveness probe on purpose: a canary that stops shows up as the `ext_authz` counters no longer increasing. |
| `authzCanary.echo.image` | `gcr.io/k8s-staging-gateway-api/echo-basic@sha256:eb739672…37c3` | The target. |

**Labels** (chart 1.0.5). The prober pods carry the DaemonSet's selector labels
(`app.kubernetes.io/name`, `instance`, `component: prober`), `part-of`,
`managed-by` and `aether.io/managed`; `helm.sh/chart` and
`app.kubernetes.io/version` are on the DaemonSet object only, so a chart release
no longer rolls the prober by itself (#1372). The canary's objects carry
`app.kubernetes.io/component: authz-canary` on their own metadata, not
`prober` (#1373); its pods carry `app: authz-canary` / `app: authz-echo` as
before. What the upgrade to 1.0.5 rolls, and how to check it:
[`runbook.md`](./runbook.md), "The prober chart".

---

## 4. Labels & annotations

Defined in [`common/constants/`](../common/constants). Prefixes:
`config.aether.io/*` = what a pod **consumes** (client config);
`endpoint.aether.io/*` = endpoint registration facts (what a pod **serves**);
`capture.aether.io/*` = transparent-capture behavior; `metadata.endpoint.aether.io/*`
= free-form endpoint metadata usable as routing subsets.

### Pod / namespace labels

| Label | Value | Meaning |
|---|---|---|
| `aether.io/managed` | `"true"` | Opt a pod (or, with `controller.namespaceInjection`, a namespace) into the mesh. |
| `aether.io/agent-not-ready` | (taint) | Startup taint keeping pods off a node until the agent's CNI serves. |

### Pod annotations (`aether.io/*`)

| Annotation | Value | Meaning |
|---|---|---|
| `aether.io/identity-gate` | `"false"` | Skip the egress identity gate for this pod: no `aether-identity-ready` init container, so the app may start (and send) before its SVID exists. Any other value, or absent, leaves `controller.webhook.identityGate.enabled` in charge. |

### Endpoint annotations (`endpoint.aether.io/*`)

| Annotation | Default | Meaning |
|---|---|---|
| `endpoint.aether.io/port` | `8080` | Primary/default service port. |
| `endpoint.aether.io/ports` | — | All served ports, comma-separated (multi-port, 005). |
| `endpoint.aether.io/weight` | `1024` | Load-balancing weight. |
| `endpoint.aether.io/health-path` | `/` | Path the agent active-health-checks. |
| `endpoint.aether.io/health-check-mode` | `eds` | `eds` (agent vets + publishes over EDS) or `active` (each client proxy probes). |
| `endpoint.aether.io/protocol` | `http` | Wire protocol served: `http` or `tcp`. |
| `endpoint.aether.io/uds-socket` | — | Deliver inbound to a Unix socket (`<volume>/<file>`) instead of the TCP port (034). `<volume>` must be the pod's inline `csi: {driver: csi.aether.io}` volume (039; the pod needs `securityContext.fsGroup`; an `emptyDir` is denied at admission), `<file>` at most 54 bytes. Wins over an `EndpointPolicy` on the service; needs `udsCsi.enabled` (default). |
| `metadata.endpoint.aether.io/<key>` | — | Free-form metadata → selectable routing subset (e.g. `…/version=v2`). |

### Config annotations (`config.aether.io/*`)

| Annotation | Meaning |
|---|---|
| `config.aether.io/upstreams` | Comma-separated upstream services this pod calls; drives demand-scoped distribution + ODCDS (004). |

### Capture annotations (`capture.aether.io/*`, proposal 022)

| Annotation | Meaning |
|---|---|
| `capture.aether.io/redirect-all` | `"true"` force redirect-all, `"false"` opt out, else node default. |
| `capture.aether.io/exclude-outbound-ports` | Comma-separated outbound ports to carve out of capture (TCP+UDP). |
| `capture.aether.io/exclude-outbound-ip-ranges` | Comma-separated IPv4 CIDRs to carve out (TCP+UDP). |

### Gateway / other

| Constant | Value | Meaning |
|---|---|---|
| edge GatewayClass controller | `gateway.aether.io/edge` | `controllerName` of the edge GatewayClass. |
| mesh (GAMMA) controller | `gateway.aether.io/mesh` | `controllerName` for Service-parented route status. |
| Gateway HTTP redirect | `gateway.aether.io/http-redirect: "true"` | Opt a Gateway's plain-HTTP listener into HTTP→HTTPS 301. |
| workload SPIFFE ID | `aether.io/spiffe-id` | **Rejected and ignored** (#669). A pod's mesh identity is always `spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>`, derived from the API server. A pod carrying this annotation is logged at WARN and counted by `aether.agent.identity.spiffe_id_override_rejected`. |

### Always-ignored namespaces

Never intercepted regardless of labels (so the control plane + SPIRE never depend
on the mesh): `kube-system`, `aether-system`, `spire-mgmt`, `spire-server`,
`spire-system`.

---

## See also

- [`getting-started.md`](./getting-started.md) — install + workload onboarding.
- [`runbook.md`](./runbook.md) — build/test/e2e developer loop.
- [`workload-requirements.md`](./workload-requirements.md) — the full workload contract.
- [`charts/README.md`](../charts/README.md) — chart layout, image mirroring, versioning.
- [`../charts/aether/values.yaml`](../charts/aether/values.yaml) — the authoritative values with full inline comments.
