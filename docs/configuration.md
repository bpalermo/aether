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
| `namespace.create` | `true` | Create the release namespace with privileged pod-security labels (the agent needs `hostNetwork` + `NET_ADMIN`). |
| `namespace.name` | `""` | Namespace all resources deploy into (defaults to the release namespace). |
| `clusterName` | `talos-main` | Cluster name passed to agent + registrar (`--cluster-name`); used in registry keys. |
| `controlCluster` | `""` | Cross-cluster config authority (026 EM3). Set to a cluster name → only that cluster's registrar exports GAMMA config and everyone imports only from it. Empty = federated (any peer, highest-version wins). |
| `debug` | `true` | Verbose logging on all components (`--debug`). |
| `meshDomain` | `aether.internal` | DNS-style domain services are addressed under (`<service>.<meshDomain>`); also the ODCDS catch-all suffix. |

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
| `agent.importConfig` | `false` | Cross-cluster config import (026): poll the registrar for peer-exported GAMMA projections and materialize them (merged with local; local wins). Pairs with `registrar.registryBackend=etcd`. |
| `agent.eastWestWaypoint` | `false` | East/west waypoint (019): dial **cross-cluster** endpoints at their node's routable IP + the fixed tunnel port `18009` instead of the (unroutable) pod IP; this node's host-network proxy SNI-forwards inbound tunnel traffic to local pods. Intra-cluster stays direct pod-to-pod. Needs cross-cluster endpoint visibility (shared or replicated etcd) + a shared SPIRE trust domain. |
| `agent.captureRedirectAllDefault` | `true` | Redirect-all as the DEFAULT for managed pods (022 Step 4); opt out per-pod with `capture.aether.io/redirect-all="false"`. `false` = per-pod opt-in via the same annotation set to `"true"`. (Transparent capture itself and the passthrough chain are unconditional since proposal 031.) |
| `agent.meshDns` | `true` | Per-pod mesh DNS (018). Gates BOTH halves: the agent's in-process resolver (which writes the record snapshot) and the separate `aether-mesh-dns` DaemonSet that serves pods from it. |
| `agent.meshDnsUpstream` | `[]` | Upstream resolver(s) for non-mesh queries, passed to the **mesh-dns daemon** (`--mesh-dns-upstream`), not the agent. Empty = the daemon's own resolv.conf (kube-dns). |
| `agent.meshDnsDaemon.image.*` / `.resources` | repo+digest placeholders | The slim `mesh-dns` image, pinned separately from the agent's (#583) — the DaemonSet ships only the `/mesh-dns` binary. Rendered when `agent.meshDns` is true. |
| `agent.meshDnsDaemon.forwardPoolSize` | `null` | Pooled, already-connected UDP sockets per upstream on the forward path (`--forward-pool-size`; #674 — dialling per query was 20.97% of this daemon's CPU). Unset emits no flag and takes the built-in default of `8`; `0` disables pooling and restores dial-per-query. An escape hatch, not a tuning knob: reuse trades per-query source-port randomisation for the socket's lifetime, which is safe only because the socket is *connected* (the kernel drops any datagram not from the upstream's exact address). |
| `agent.meshDnsDaemon.lameDuckMax` | `10s` | Ceiling on the post-SIGTERM window in which the resolver stops reporting ready but keeps answering (`--lame-duck-max`; #729). It closes as soon as it observes a *different* instance answering on the same address, so the ceiling only bites when no successor appears (scale-down, node drain, failed surge). The DaemonSet's `terminationGracePeriodSeconds` is derived from this (+5s). `"0s"` restores the pre-#729 close-on-SIGTERM behaviour, which dropped one queued datagram on every roll. |
| `agent.meshDnsDaemon.debug` | `false` | TRACE logging for this daemon only (#684). The top-level `debug` deliberately no longer reaches mesh-dns — it is the one component on every managed pod's `:53` path, and everything it logs is also fanned out to the OTLP log exporter. |
| `agent.image.*` | repo+digest placeholders, `pullPolicy: Always` | Digest-pinned image; mirror by overriding `repository` alone. |
| `agent.resources.{requests,limits}` | cpu `200m`, mem `64Mi` | |

> The agent reaches the registry only through the registrar (gRPC) — it never
> talks to an external registry backend and carries no backend credentials.

### `proxy` — per-node Envoy DaemonSet

| Key | Default | Purpose |
|---|---|---|
| `proxy.enabled` | `true` | Deploy the per-node Envoy. Disable to run only the agent. |
| `proxy.image.repository` | `ghcr.io/bpalermo/aether/aether-proxy` | External image built by the `//proxy` workspace, tag-pinned. |
| `proxy.image.tag` | (commit SHA) | The publishing commit. |
| `proxy.logLevel` | `info` | Envoy log level. |
| `proxy.jsonLogs` | `true` | Envoy application logs as one JSON object per line. |
| `proxy.hotRestart.baseId` | `0` | Envoy hot-restart tunables (mechanism is not optional; see proposal 001). |
| `proxy.hotRestart.drainTime` | `10s` | Graceful connection-close window for the draining epoch. |
| `proxy.hotRestart.parentShutdownTime` | `15s` | When the previous epoch is terminated (must exceed drainTime). Also the supervisor's admin re-verify budget: the epoch-identity probe re-confirms on a fresh connection every `parentShutdownTime/3` (floor 2s, ceiling 15s), so a cross-pod takeover is diagnosed while the draining parent still lives. Below ~6s the floor takes over and the supervisor logs the lost margin at startup; raising it also delays successor-pod readiness by the same amount. |
| `proxy.hotRestart.handoffDeadline` / `adminUnresponsiveDeadline` | `0` | Supervisor watchdogs (0 = built-in defaults). |
| `proxy.hotRestart.shmHostPath` | `/run/aether/shm` | Shared-memory hostPath for cross-pod hot restart. |
| `proxy.udsWorkloads.enabled` | `true` | UDS delivery (034). Gates the proxy's `/var/lib/kubelet/pods` hostPath mount, the agent's `--kubelet-pods-dir`, and the agent's read access to the `EndpointPolicy` CRD. Inert until a workload asks for it; turning it off later silently degrades annotated pods to TCP (nothing listens, so their endpoints stay unpromoted). |
| `proxy.overload.enabled` | `true` | Envoy overload-manager graceful-degradation ladder. |
| `proxy.overload.maxHeapSizeBytes` | `402653184` (384Mi) | Keep at ~75% of `resources.limits.memory`. |
| `proxy.resources.{requests,limits}` | cpu `500m`, mem `512Mi` | |

#### `proxy.authzSidecar` — external authorization (proposal 027)

| Key | Default | Purpose |
|---|---|---|
| `proxy.authzSidecar.enabled` | `false` | Add a node-local authz gRPC sidecar (UDS) + a DISABLED ext_authz filter entry; zero effect until an `HTTPFilter` (extAuthz) opts a route/service in. |
| `proxy.authzSidecar.opa.enabled` | `false` | Built-in OPA preset (opt-in). |
| `proxy.authzSidecar.opa.image` | `openpolicyagent/opa:1.20.2-envoy-static` | OPA image. |
| `proxy.authzSidecar.opa.policy` | `""` | Rego policy (ConfigMap-mounted); required when `opa.enabled`. |
| `proxy.authzSidecar.image.{repository,tag,args}` | `""` / `[]` | Bring-your-own authz container (serves `envoy.service.auth.v3.Authorization` on `unix:///run/aether/authz/authz.sock`). |
| `proxy.authzSidecar.timeout` | `200ms` | Per-check gRPC timeout. |
| `proxy.authzSidecar.failureMode` | `DENY` | `DENY` (fail-closed, 403 when unreachable) or `ALLOW` (fail-open). |
| `proxy.authzSidecar.resources` | `10m` / `32Mi` requests, `128Mi` memory limit | Sidecar resources (OPA preset and bring-your-own). No CPU limit on purpose: it is on the request path, and throttling becomes ext_authz timeouts — 403s under `DENY`. |

Envoy exports **no** `ext_authz` statistics until a route actually uses the filter
(its OTLP stats sink only flushes counters that have been used). The prober chart's
`authzCanary` gives a cluster one such route and asserts an allow and a deny
decision every cycle — see `charts/prober/values.yaml`.

### `cniInstall` — CNI installer init container

| Key | Default | Purpose |
|---|---|---|
| `cniInstall.image.*` | repo+digest placeholders, `pullPolicy: Always` | Digest-pinned image. |
| `cniInstall.resources.{requests,limits}` | cpu `100m`, mem `32Mi` | |

### `registrar`

| Key | Default | Purpose |
|---|---|---|
| `registrar.registryBackend` | `kubernetes` | Backend (`--registry-backend`): `kubernetes` or `etcd`. |
| `registrar.replicaCount` | `2` | Always 2 (exercises the multi-replica write-behind topology). |
| `registrar.enableMCS` | `false` | Multi-Cluster Services phase 1 (018 + 006): export `ServiceExport`s and materialize `ServiceImport`s + clusterset VIPs. Requires the etcd backend + the MCS-API CRDs. |
| `registrar.region` | `local` | Region owning this registrar's etcd partition (006); keys are `/aether/v1/regions/<region>/clusters/<clusterName>/…`. One region = one etcd. |
| `registrar.etcd.endpoints` | `[]` | etcd client endpoints (etcd backend). |
| `registrar.peerEtcd` | `[]` | Cross-region replication (006 Phase 2), one entry per peer region: `"<region>=<endpoint>[,<endpoint>...]"`. The leader registrar mirrors this region's own registry subtree verbatim into each peer's etcd under an **origin-heartbeat lease** (TTL ~30s): if this region dies, its mirror expires on the peers — whole-region failover cleanup with no peer-side GC. Requires the etcd backend + a non-default `region`. |
| `registrar.service.{port,targetPort}` | `443` / `8443` | gRPC service ports. |
| `registrar.image.*` / `registrar.resources.*` | placeholders / cpu `100m`, mem `64Mi` | |

### `controller`

| Key | Default | Purpose |
|---|---|---|
| `controller.replicaCount` | `2` | Reconcilers and the node-taint guard are leader-elected, but every replica serves the admission webhooks, which are `failurePolicy: Ignore`: with none answering, a pod in an `aether.io/managed` namespace is admitted **unmeshed**. One replica measured a 49 s gap on a leader delete. |
| `controller.injectPodNdots` | `true` | Pod-mutating webhook injects `dnsConfig` ndots into managed pods so mesh FQDNs resolve absolute-first (musl/Alpine safety). Pairs with mesh DNS. |
| `controller.namespaceInjection` | `true` | Namespace auto-injection: a pod in a namespace labeled `aether.io/managed=true` is given the pod label automatically (opt out with `aether.io/managed=false`). |
| `controller.webhook.spire` | `false` | Webhook serving cert source — decoupled from mesh SPIRE. `false` = Helm self-signed cert (works out of the box). `true` = serve with the controller's SPIRE SVID + inject the trust bundle. |
| `controller.webhook.clusterSpiffeID.create` | `true` | When `spire=true`, create the controller's `ClusterSPIFFEID` with the webhook Service DNS SANs. |
| `controller.webhook.clusterSpiffeID.className` | `""` | spire-controller-manager class name; REQUIRED when `create=true`. |
| `controller.image.*` / `controller.resources.*` | placeholders / cpu `50m`, mem `64Mi` | |

### `edge` — north-south ingress gateway (proposals 003/018/021/028)

An unprivileged Deployment (Envoy + `agent edge`) that dials mesh pods directly
over mTLS and routes external traffic via the Gateway API. Disabled by default.

| Key | Default | Purpose |
|---|---|---|
| `edge.enabled` | `false` | Deploy the edge. |
| `edge.namespace` | `aether-ingress` | The edge runs in its own namespace, isolated from the control plane. |
| `edge.namespaceCreate` | `true` | Let the chart create it (baseline PSA). |
| `edge.replicaCount` | `2` | Gateway replicas (standard RollingUpdate + readiness gate; no hot-restart supervisor). |
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
| `edge.service.{type,port,httpsPort,annotations,extraPorts}` | `LoadBalancer` / `80` / `443` / `{}` / `[]` | The edge's Service; `extraPorts` exposes TCP/TLS listener ports. |
| `edge.drain.preStopSeconds` | `10` | preStop sleep holding off SIGTERM during drain (matches `proxy.hotRestart.drainTime`). 0 disables. |
| `edge.drain.terminationGracePeriodSeconds` | `30` | Must exceed preStop + Envoy drain. |
| `edge.admin.{enabled,port}` | `false` / `9901` | Envoy admin on loopback only; off by default. |
| `edge.overload.{enabled,maxHeapSizeBytes}` | `true` / `201326592` (192Mi) | Overload monitor (works here; the pod is unprivileged). |
| `edge.spire.clusterSpiffeID.{create,className}` | `true` / `""` | Create the edge's `ClusterSPIFFEID` (when `spire.enabled`); `className` required when `create=true`. |
| `edge.resources.{requests,limits}` | cpu `200m`, mem `128Mi`/`256Mi` | |

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
| `endpointpolicies.config.aether.io` | `EndpointPolicy` | Service-scoped UDS delivery (proposal 034 Phase 1b): `spec.targetRef` (kind=Service, same namespace) + `spec.udsSocket` (`<volume>/<file>`) declares socket delivery for every pod of a service. The per-pod `endpoint.aether.io/uds-socket` annotation wins; one policy per Service (lexicographically smallest name wins). Read by the agent only when `proxy.udsWorkloads.enabled`. |

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
| `--kubelet-pods-dir` | `/var/lib/kubelet/pods` | Kubelet's pod-volumes dir, mounted into the proxy at the identical host path, through which the proxy reaches a workload's Unix socket (034). Empty disables UDS delivery: pods annotated `endpoint.aether.io/uds-socket` fall back to TCP loopback. Gated by the chart's `proxy.udsWorkloads.enabled`. |
| `--spire-broker-socket` | `/run/spire/broker-sockets/broker.sock` | SPIRE agent's SPIFFE Broker Endpoint socket, over which the agent brokers an X.509-SVID for every pod on its node (036). Replaced `--spire-admin-socket`; requires SPIRE >= 1.15.2 with its experimental broker enabled. |
| `--gamma` | `true` | GAMMA east-west routing (018); default-on kill switch (031). CRD-detected. |
| `--cni-conflist-reassert` | `true` | Re-assert the chained `aether-cni` entry in the node's active CNI conflist whenever a competing writer strips it (#645). Watches `--mounted-cni-net-dir` (fsnotify) plus a 60s re-check; only ever appends to an existing, valid conflist that still carries a primary CNI plugin. |
| `--mounted-cni-net-dir` | `/host/etc/cni/net.d` | Host CNI config dir as mounted into the agent (read-write) for the re-assert loop. |
| `--import-config` | `false` | Enable cross-cluster config import (026). |
| `--control-cluster` | `""` | Trust imported config ONLY from this origin (026 EM3). Empty = federated. |
| `--east-west-waypoint` | `false` | Per-node east/west waypoint for cross-cluster traffic (019); tunnel port is the fixed constant 18009. |
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
(`ghcr.io/bpalermo/aether/proxy-supervisor`), which also means the proxy
DaemonSet no longer depends on the agent image at all. `agent proxy-supervisor`
remains as a deprecated alias for one release so a chart predating #772 still
has a working initContainer against a newer agent image.

Flags (every name and default unchanged by the split — the chart passes them
literally): `--envoy-path`
(`/usr/local/bin/envoy`), `--config` (`/etc/envoy/envoy.yaml`), `--base-id` (`0`),
`--drain-time` (`45s`), `--parent-shutdown-time` (`60s`), `--watch-config`
(`true`), `--state-dir` (`/run/aether/hotrestart`), `--ready-marker`, `--envoy-arg`
(repeatable), `--handoff-deadline`/`--admin-unresponsive-deadline` (`0` = defaults),
`--termination-grace` (`0`), `--shutdown-drain-immediately` (`false`),
`--admin-address` (`127.0.0.1:9901`), `--install-path`,
`--install-readiness-path`, `--readiness-check` (deprecated), `--otlp-endpoint`.

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
reachable through the supported path — the check stays for the deprecated
`agent proxy-supervisor` alias, where the source is the agent image.

`--readiness-check` is the pre-#673 exec probe and is deprecated: re-execing a
supervisor binary every 2s per pod spent >=31% of the supervisor container's CPU
on Go package init alone (which runs before `main()`, so no argv check can avoid
it) — measured when that binary was the agent's 67MB one.
The chart now execs the standalone `proxy-ready` binary below instead. The flag
still works, so a chart predating #673 keeps a probe against a newer image — it is
marked deprecated in cobra, so using it prints a warning and it no longer appears
in `--help`.

### `proxy-ready` (standalone binary — bundled in the proxy-supervisor image, not run from it)

The `aether-proxy` pod's exec readiness probe (#673). One flag: `--ready-marker`
(`/var/run/aether-proxy/ready`); exit 0 iff that path stats. It is deliberately
stdlib-only (~1.7MB vs the agent's 67MB) — it imports nothing but
`common/readymarker`, and `//agent/cmd/proxy-ready:deps_test` fails the build if
that ever changes. It ships as an extra layer in the `proxy-supervisor` image (no
second pull: the `install-supervisor` initContainer, which already runs that
image, copies it onto the proxy pod's shared volume at
`/opt/aether/proxy-ready`). It is also still layered into the agent image, for
the deprecated `agent proxy-supervisor` alias.

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
(`/run/aether/mesh-dns.ready`), `--readiness-check` (deprecated),
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

`--readiness-check` is the pre-#683 exec probe and is deprecated: re-execing this
16.9MB daemon every 15s per pod spent ~10 core-seconds per 25 minutes fleet-wide
(~3-4% of the container's CPU) on container exec and Go package init alone (which
runs before `main()`, so no argv check can avoid it). The chart execs the
standalone `mesh-dns-ready` binary below instead. The flag still works, so a
chart predating #683 keeps a probe against a newer image — it is marked deprecated
in cobra, so using it prints a warning and it no longer appears in `--help`.

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

### `cni-install` (init container)

`--cni-bin-dir`, `--cni-bin-target-dir`, `--mounted-cni-net-dir`,
`--otlp-endpoint`, `--capture-redirect-all-default`,
`--mesh-dns`, `--host-ip`, `--debug`. The per-pod capture redirect is
unconditional (no `--transparent-capture`; per-pod `capture.aether.io/*`
annotations opt out). (The `cni` plugin binary itself is configured via
CNI-spec stdin, not flags.)

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
`mesh_dns`), `target` (the probed name) and `result`:

| Metric | Type | Notes |
|---|---|---|
| `aether_probe_requests_total` | counter | `result` is one of `success`, `http_error`, `connection_error`, `timeout`, `saturated`, plus — `mesh_dns` tier only — `dns_error`, `dns_nxdomain`, `dns_timeout`. A resolution failure is an independently alertable signal; a post-resolution connect failure stays `connection_error`. |
| `aether_probe_request_duration_seconds` | histogram | Explicit **seconds**-valued buckets (`0.001` … `5`). They have to be set explicitly: the SDK's default boundaries are tuned for millisecond-valued durations, so against seconds the first bucket is `<= 5s` and a healthy 2 ms probe is indistinguishable from a timed-out 2 s one (#732). |

Per-node identity is a **resource** attribute, not a metric label: the chart sets
`OTEL_RESOURCE_ATTRIBUTES=k8s.node.name=$(NODE_NAME),…` and the prober's resource
builder reads it from the environment, so the series de-collapse per node once
the collector promotes it (#210).

**Deployment.** The chart renders a DaemonSet + ServiceAccount into a namespace
that must already be mesh-managed — the probe only works if the CNI has plumbed
the pod's egress listener. `namespace.create` is `false` and `namespace.name`
defaults to the release namespace; on talos-main that is `aether-test`. The pod
carries `aether.io/managed: "true"`, and the chart derives
`config.aether.io/upstreams` from the union of the reachability and `mesh_dns`
targets so the agent programs those clusters (proposal 004). It ships **no CPU
limit** on purpose: the limit is a CFS quota, which quantises probe latency in
~100 ms steps, and at the previous `50m` limit that was the dominant term in the
published SLI (#735). The memory limit stays — it also feeds `GOMEMLIMIT`.

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

### Endpoint annotations (`endpoint.aether.io/*`)

| Annotation | Default | Meaning |
|---|---|---|
| `endpoint.aether.io/port` | `8080` | Primary/default service port. |
| `endpoint.aether.io/ports` | — | All served ports, comma-separated (multi-port, 005). |
| `endpoint.aether.io/weight` | `1024` | Load-balancing weight. |
| `endpoint.aether.io/health-path` | `/` | Path the agent active-health-checks. |
| `endpoint.aether.io/health-check-mode` | `eds` | `eds` (agent vets + publishes over EDS) or `active` (each client proxy probes). |
| `endpoint.aether.io/protocol` | `http` | Wire protocol served: `http` or `tcp`. |
| `endpoint.aether.io/uds-socket` | — | Deliver inbound to a Unix socket (`<volume>/<file>`, `emptyDir` only, no `subPath`) instead of the TCP port (034). Wins over an `EndpointPolicy` on the service; needs `proxy.udsWorkloads.enabled`. |
| `metadata.endpoint.aether.io/<key>` | — | Free-form metadata → selectable routing subset (e.g. `…/version=v2`). |

### Config annotations (`config.aether.io/*`)

| Annotation | Meaning |
|---|---|
| `config.aether.io/upstreams` | Comma-separated upstream services this pod calls; drives demand-scoped distribution + ODCDS (004). |

### Capture annotations (`capture.aether.io/*`, proposal 022)

| Annotation | Meaning |
|---|---|
| `capture.aether.io/redirect-all` | `"true"` force redirect-all, `"false"` opt out, else node default. |
| `capture.aether.io/exclude-outbound-ports` | Comma-separated outbound TCP ports to carve out of capture. |
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
