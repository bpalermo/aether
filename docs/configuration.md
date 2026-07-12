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
| `spire.trustDomain` | `""` | SPIFFE trust domain for the registrar's mTLS peers. Empty = `ROOTCA` sentinel. |
| `spire.adminSocket.hostPath` | `/run/spire/agent/sockets/csi.spiffe.io/admin` | SPIRE agent admin (Delegated Identity) socket the agent uses to mint proxy SVIDs. |
| `spire.adminSocket.mountPath` | `/run/spire/admin-sockets` | Where the admin socket is mounted. |
| `spire.adminSocket.socketName` | `admin.sock` | Admin socket filename. |

### `meshConfig` — proxy MeshConfig seeding

| Key | Default | Purpose |
|---|---|---|
| `meshConfig.createDefault` | `true` | Seed the singleton `MeshConfig` (`default`) on first install only — never overwritten on upgrade (operators own it via kubectl). |
| `meshConfig.proxy` | `{}` | The `spec.proxy` seeded into that CR (protojson field names). Empty = proxy inherits everything from system config. |

### `agent`

| Key | Default | Purpose |
|---|---|---|
| `agent.gamma` | `false` | GAMMA east-west L7 routing (018 Phase 2): watch `HTTPRoute`s parented to a Service and enrich outbound routes. Requires the Gateway API CRDs. |
| `agent.importConfig` | `false` | Cross-cluster config import (026): poll the registrar for peer-exported GAMMA projections and materialize them (merged with local; local wins). Pairs with `registrar.registryBackend=etcd`. |
| `agent.l4Routes` | `false` | L4 route types (018 Phase 3b): `TCPRoute`/`TLSRoute`/`UDPRoute` parented to a Service → weighted TCP + per-SNI TLS chains on the capture listener. Requires `transparentCapture`. UDPRoute is control-plane only. |
| `agent.transparentCapture` | `true` | Transparent capture (018 Phase 3a): per-pod capture listeners + the `cap_http` route table from generated mesh Services. Pairs with `registrar.generateMeshServices`. |
| `agent.captureRedirectAll` | `false` | Add the dormant ORIGINAL_DST passthrough chain (022) so a pod annotated `capture.aether.io/redirect-all="true"` can redirect ALL outbound TCP. Per-pod opt-in. Requires `transparentCapture`. |
| `agent.captureRedirectAllDefault` | `true` | Make redirect-all the DEFAULT for managed pods (022 Step 4); opt out per-pod with `capture.aether.io/redirect-all="false"`. Implies `captureRedirectAll`. Requires `transparentCapture`. Riskiest blast radius; soak-validated hitless. |
| `agent.meshDns` | `true` | Per-pod mesh DNS (018): resolve `<svc>.<meshDomain>` from generated mesh Services, forward the rest to `meshDnsUpstream`. |
| `agent.meshDnsUpstream` | `[]` | Upstream resolver(s) for non-mesh queries. Empty = the agent's own resolv.conf (kube-dns). |
| `agent.image.*` | repo+digest placeholders, `pullPolicy: Always` | Digest-pinned image; mirror by overriding `repository` alone. |
| `agent.resources.{requests,limits}` | cpu `200m`, mem `64Mi` | |

> The agent reaches the registry only through the registrar (gRPC) — it carries no
> AWS credentials. AWS is used solely by the registrar's dynamodb backend.

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
| `proxy.hotRestart.parentShutdownTime` | `15s` | When the previous epoch is terminated (must exceed drainTime). |
| `proxy.hotRestart.handoffDeadline` / `adminUnresponsiveDeadline` | `0` | Supervisor watchdogs (0 = built-in defaults). |
| `proxy.hotRestart.shmHostPath` | `/run/aether/shm` | Shared-memory hostPath for cross-pod hot restart. |
| `proxy.overload.enabled` | `true` | Envoy overload-manager graceful-degradation ladder. |
| `proxy.overload.maxHeapSizeBytes` | `402653184` (384Mi) | Keep at ~75% of `resources.limits.memory`. |
| `proxy.resources.{requests,limits}` | cpu `500m`, mem `512Mi` | |

#### `proxy.authzSidecar` — external authorization (proposal 027)

| Key | Default | Purpose |
|---|---|---|
| `proxy.authzSidecar.enabled` | `false` | Add a node-local authz gRPC sidecar (UDS) + a DISABLED ext_authz filter entry; zero effect until an `HTTPFilter` (extAuthz) opts a route/service in. |
| `proxy.authzSidecar.opa.enabled` | `false` | Built-in OPA preset (opt-in). |
| `proxy.authzSidecar.opa.image` | `openpolicyagent/opa:1.18.2-envoy-static` | OPA image. |
| `proxy.authzSidecar.opa.policy` | `""` | Rego policy (ConfigMap-mounted); required when `opa.enabled`. |
| `proxy.authzSidecar.image.{repository,tag,args}` | `""` / `[]` | Bring-your-own authz container (serves `envoy.service.auth.v3.Authorization` on `unix:///run/aether/authz/authz.sock`). |
| `proxy.authzSidecar.timeout` | `200ms` | Per-check gRPC timeout. |
| `proxy.authzSidecar.failureMode` | `DENY` | `DENY` (fail-closed, 403 when unreachable) or `ALLOW` (fail-open). |

### `cniInstall` — CNI installer init container

| Key | Default | Purpose |
|---|---|---|
| `cniInstall.image.*` | repo+digest placeholders, `pullPolicy: Always` | Digest-pinned image. |
| `cniInstall.resources.{requests,limits}` | cpu `100m`, mem `32Mi` | |

### `registrar`

| Key | Default | Purpose |
|---|---|---|
| `registrar.registryBackend` | `kubernetes` | Backend (`--registry-backend`): `kubernetes`, `dynamodb`, or `etcd`. |
| `registrar.replicaCount` | `2` | Always 2 (exercises the multi-replica write-behind topology). |
| `registrar.generateMeshServices` | `true` | Project the mesh catalog into selectorless k8s Services (transparent-capture VIPs, 018 Phase 3a) that `transparentCapture` + `meshDns` depend on. |
| `registrar.enableMCS` | `false` | Multi-Cluster Services phase 1 (018 + 006): export `ServiceExport`s and materialize `ServiceImport`s + clusterset VIPs. Requires the etcd backend + the MCS-API CRDs. |
| `registrar.region` | `local` | Region owning this registrar's etcd partition (006); keys are `/aether/v1/regions/<region>/clusters/<clusterName>/…`. |
| `registrar.etcd.endpoints` | `[]` | etcd client endpoints (etcd backend). |
| `registrar.aws.region` | `us-east-1` | AWS region for the dynamodb backend (IRSA; no static keys). |
| `registrar.aws.roleArn` | `""` | Role ARN annotated onto the registrar ServiceAccount. Empty = no AWS access. |
| `registrar.service.{port,targetPort}` | `443` / `8443` | gRPC service ports. |
| `registrar.image.*` / `registrar.resources.*` | placeholders / cpu `100m`, mem `64Mi` | |

### `controller`

| Key | Default | Purpose |
|---|---|---|
| `controller.replicaCount` | `1` | Leader election is on; extra replicas are warm standbys. |
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
| `edge.perGatewayAddressing` | `true` | 021 Phase 2: one LoadBalancer Service + distinct internal port per Gateway (each gets its own IP). `false` = Phase 1 (single shared IP). |
| `edge.tls.enabled` | `false` | Downstream TLS: HTTPS listener (certs per Gateway listener via SDS) + HTTP→HTTPS redirect. The edge→pod hop stays mTLS. |
| `edge.geoip.enabled` | `false` | Emit `x-geo-*` request headers from a MaxMind DB (028). The `x-geo-*` namespace is always stripped from client requests. |
| `edge.geoip.headers` | `[country]` | Which headers to emit: `country`, `city`. |
| `edge.geoip.database.secretName` / `fileName` | `""` / `GeoLite2-City.mmdb` | The bring-your-own mmdb Secret + key. |
| `edge.xffNumTrustedHops` | `0` | Trusted proxies in front of the edge (feeds HCM client-address + geoip XFF). |
| `edge.httpPort` / `httpsPort` | `80` / `443` | Public listener ports (privileged ports via `NET_BIND_SERVICE`; pod stays unprivileged). |
| `edge.readinessPort` | `18021` | Dedicated always-bound readiness listener (kubelet probe target; never exposed). |
| `edge.routeNamespace` | `""` | Namespace the edge watches Gateways/HTTPRoutes in. Empty = its own namespace. |
| `edge.service.{type,port,httpsPort,annotations,extraPorts}` | `LoadBalancer` / `80` / `443` / `{}` / `[]` | The edge's Service; `extraPorts` exposes TCP/TLS listener ports. |
| `edge.drain.preStopSeconds` | `10` | preStop sleep holding off SIGTERM during drain (matches `proxy.hotRestart.drainTime`). 0 disables. |
| `edge.drain.terminationGracePeriodSeconds` | `30` | Must exceed preStop + Envoy drain. |
| `edge.admin.{enabled,port}` | `false` / `9901` | Envoy admin on loopback only; off by default. |
| `edge.overload.{enabled,maxHeapSizeBytes}` | `true` / `201326592` (192Mi) | Overload monitor (works here; the pod is unprivileged). |
| `edge.spire.clusterSpiffeID.{create,className}` | `true` / `""` | Create the edge's `ClusterSPIFFEID` (when `spire.enabled`); `className` required when `create=true`. |
| `edge.resources.{requests,limits}` | cpu `200m`, mem `128Mi`/`256Mi` | |

---

## 2. CRDs (`charts/crds`)

Both are `config.aether.io/v1`, **Namespaced**, structural-but-permissive
(`x-kubernetes-preserve-unknown-fields`); authoritative validation is the
controller's protovalidate webhook, not OpenAPI. Both carry
`helm.sh/resource-policy: keep`.

| CRD | Kind (short) | Purpose |
|---|---|---|
| `meshconfigs.config.aether.io` | `MeshConfig` (`mc`) | Per-namespace proxy observability overrides (access logs, tracing, per-pod stats). A namespace inherits the control-plane namespace's `MeshConfig` field-by-field unless it sets its own (proposal 015). |
| `httpfilters.config.aether.io` | `HTTPFilter` (`htf`) | The proxy-extension escape hatch (proposal 025): attach a supported Envoy HTTP filter (ext_authz, RBAC, header-to-metadata) at a chosen scope. |

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
(required), `--cluster-name` (required), `--proxy-id` (`proxy`, required),
`--registrar-address` (`aether-registrar.aether-system.svc:443`),
`--spire-workload-socket`.

Node-agent-specific:

| Flag | Default | Purpose |
|---|---|---|
| `--mounted-registry-dir` | `/host/var/lib/aether/registry` | Local pod-data dir for the CNI plugin. |
| `--spire-admin-socket` | `/tmp/spire-agent/private/admin.sock` | SPIRE admin socket for proxy SVID delegation. |
| `--remove-startup-taint` | `true` | Remove the `aether.io/agent-not-ready` node taint once the CNI server serves. |
| `--gamma` | `false` | Enable GAMMA east-west routing (018 Phase 2). |
| `--import-config` | `false` | Enable cross-cluster config import (026). |
| `--control-cluster` | `""` | Trust imported config ONLY from this origin (026 EM3). Empty = federated. |
| `--transparent-capture` | `false` | Per-pod capture listeners + `cap_http` (018 Phase 3a). |
| `--capture-redirect-all` | `false` | Dormant ORIGINAL_DST passthrough chain (022). |
| `--l4-routes` | `false` | L4 route types (018 Phase 3b). |
| `--mesh-dns` | `false` | Per-pod mesh DNS (018). |
| `--mesh-dns-upstream` | `[]` | Upstream resolver(s) for non-mesh queries. |
| `--authz-sidecar` | `false` | Node-local ext_authz sidecar entry (027). |
| `--authz-sidecar-timeout` | `200ms` | Per-check gRPC timeout. |
| `--authz-sidecar-failure-mode-allow` | `false` | Fail-open (default: fail-closed). |

> The chart's booleans (`agent.transparentCapture`, `agent.meshDns`,
> `agent.captureRedirectAllDefault`, …) map to these flags; the chart also
> auto-enables `--capture-redirect-all` when `captureRedirectAllDefault=true`.

### `agent edge` (subcommand)

Inherits the manager + identity flags (but `--node-name`/`--proxy-id` are relaxed,
derived from `POD_NAME`). Adds: `--edge-http-port` (`80`), `--edge-https-port`
(`443`), `--edge-readiness-port` (`18021`), `--edge-tls` (`false`),
`--gateway-class` (`aether`), `--route-namespace` (`""`), `--edge-service-name`
(`""`), `--edge-per-gateway-addressing` (`true`), `--geoip-city-db` (`""`),
`--geoip-headers` (`[country]`), `--xff-num-trusted-hops` (`0`),
`--mounted-registry-dir` (`/var/lib/aether/registry`).

### `agent proxy-supervisor` (subcommand — standalone flag set)

The Envoy hot-restart supervisor (proposal 001): `--envoy-path`
(`/usr/local/bin/envoy`), `--config` (`/etc/envoy/envoy.yaml`), `--base-id` (`0`),
`--drain-time` (`45s`), `--parent-shutdown-time` (`60s`), `--watch-config`
(`true`), `--state-dir` (`/run/aether/hotrestart`), `--ready-marker`, `--envoy-arg`
(repeatable), `--handoff-deadline`/`--admin-unresponsive-deadline` (`0` = defaults),
`--admin-address` (`127.0.0.1:9901`), `--readiness-check`, `--install-path`,
`--otlp-endpoint`.

### `registrar`

`--cluster-name` (required), `--mesh-domain` (`aether.internal`),
`--control-cluster` (`""`), `--region` (`""`), `--registry-backend`
(`kubernetes`), `--etcd-endpoints` (`[localhost:2379]`), `--sync-interval` (`5s`),
`--generate-mesh-services` (`false`), `--enable-mcs` (`false`), `--grpc-address`
(`:8443`), `--spire-enabled` (`true`), `--spire-workload-socket`,
`--spire-trust-domain` (`ROOTCA` sentinel).

### `controller`

`--mesh-config-configmap` (`aether-mesh-config`), `--spire-enabled` (`false`),
`--spire-workload-socket`, `--webhook-config-name` (`""`),
`--mutating-webhook-config-name` (`""`), `--pod-ndots` (`2`).

### `cni-install` (init container)

`--cni-bin-dir`, `--cni-bin-target-dir`, `--mounted-cni-net-dir`,
`--otlp-endpoint`, `--transparent-capture`, `--capture-redirect-all-default`,
`--mesh-dns`, `--host-ip`, `--debug`. (The `cni` plugin binary itself is
configured via CNI-spec stdin, not flags.)

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
| workload SPIFFE ID | `aether.io/spiffe-id` | Workload SPIFFE ID (SDS secret name for the pod's TLS cert). |

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
