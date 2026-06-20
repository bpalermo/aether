# Getting Started with Aether

A practical guide to installing Aether and onboarding your first workload.

> **What is Aether?** Aether is a Kubernetes service-mesh **data plane** written
> in Go. A per-node **agent** (DaemonSet) runs an Envoy xDS control plane and a
> chained CNI plugin; a per-node **Envoy proxy** (DaemonSet) carries the traffic;
> an in-cluster **registrar** tracks service endpoints; and a **controller**
> serves the `MeshConfig` admission webhook. Every pod-to-pod hop is mTLS with
> SPIFFE identities — including same-node hops. There is **no iptables
> interception**: applications reach the mesh by dialing a local outbound
> listener explicitly.

---

## Table of contents

1. [How it works (the 2-minute model)](#how-it-works)
2. [Prerequisites](#prerequisites)
3. [Install](#install)
4. [Configure](#configure)
5. [Which pods are managed (and which are never)](#managed-pods)
6. [Onboard a workload](#onboard-a-workload)
7. [Call other services](#call-other-services)
8. [Roll without dropping requests](#hitless-rolls)
9. [Advanced routing (pinning, subsets, locality)](#advanced-routing)
10. [North-south ingress (the edge gateway)](#edge)
11. [Observability](#observability)
12. [Troubleshooting](#troubleshooting)

---

<a name="how-it-works"></a>
## 1. How it works (the 2-minute model)

```mermaid
flowchart TB
    backend[("Registry backend<br/>(kubernetes / dynamodb / etcd)")]
    registrar["registrar (Deployment ×2)<br/>watches the backend,<br/>fans endpoint changes to agents"]
    backend <--> registrar

    subgraph node["per node"]
        agent["agent (DaemonSet)<br/>CNI plugin + xDS control plane (Unix socket)<br/>builds the Envoy snapshot<br/>(listeners / clusters / endpoints / routes)"]
        proxy["Envoy proxy (DaemonSet)<br/>binds outbound listener 127.0.0.1:18081<br/>inside EACH managed pod's netns"]
        app["app pod"]
        agent -->|xDS snapshot| proxy
        app -->|"HTTP → 127.0.0.1:18081<br/>(Host: my-svc.aether.internal)"| proxy
    end

    agent -->|"register / deregister<br/>at CNI ADD / DEL"| registrar
    registrar -->|endpoint stream| agent
    proxy -->|"mTLS — SPIFFE identity per workload"| dest["destination pod<br/>(this or another node)"]
```

- **Identity = ServiceAccount.** A pod's mesh service name is its
  **ServiceAccount name**; its SPIFFE ID is
  `spiffe://<trust-domain>/ns/<namespace>/sa/<serviceaccount>`. Pods sharing a
  ServiceAccount are endpoints of the same service.
- **Explicit addressing.** Apps call `http://127.0.0.1:18081` with
  `Host: <service>.<meshDomain>`. No traffic is transparently captured.
- **Demand-scoped.** Each node only receives the config for the services its
  local pods actually call (declared up front, or fetched on first use).

---

<a name="prerequisites"></a>
## 2. Prerequisites

| Requirement | Why | Notes |
|---|---|---|
| **Kubernetes cluster** | host | v1.30+ recommended (native `preStop.sleep` is used for hitless rolls). |
| **A primary CNI** (Calico, Cilium, flannel, …) | pod IP + connectivity | Aether installs a **chained** CNI plugin (`aether.conflist`) that appends itself to your existing CNI config. It does *not* replace your CNI. |
| **SPIRE** (SPIFFE runtime) | mTLS identity | Required when `spire.enabled=true` (the default). The agent, registrar and controller consume the **Workload API socket** (via the `csi.spiffe.io` CSI driver), and the agent additionally uses the SPIRE **agent admin / Delegated Identity** socket to mint proxy SVIDs. SPIRE pods must run in an ignored namespace (see §5) so they never depend on the mesh. |
| **A registry backend** | endpoint storage | One of `kubernetes` (default, no external dependency), `dynamodb` (AWS), or `etcd`. |
| **Helm 3** with OCI support | install | Charts are published as OCI artifacts. |
| **Privileged pod-security** in `aether-system` | the agent needs `hostNetwork` + `NET_ADMIN` | The chart labels its namespace `privileged` automatically when it creates it. |

> You can run **without mTLS** for a quick kick-the-tires install by setting
> `spire.enabled=false`, which skips the SPIRE dependency entirely. Don't do that
> in production — it disables workload mTLS.

---

<a name="install"></a>
## 3. Install

Aether ships **two** charts. Install the CRDs first (they are standalone so they
can be upgraded independently), then the system chart.

```bash
# Pick the published version (chart version == git commit of the release).
VERSION=0.x.0-<commit>

# 1) CRDs (MeshConfig, EdgeRoute) — install/upgrade first.
helm upgrade --install aether-crds \
  oci://ghcr.io/bpalermo/aether/charts/crds \
  --version "$VERSION"

# 2) The system: agent + proxy + registrar + controller.
helm upgrade --install aether \
  oci://ghcr.io/bpalermo/aether/charts/aether \
  --version "$VERSION" \
  --namespace aether-system --create-namespace \
  --set clusterName=my-cluster \
  --set meshDomain=aether.internal
```

Resource names derive from the **release** name — installing as `aether` yields
`aether-agent`, `aether-proxy`, `aether-registrar`, `aether-controller`.

> **Upgrade tip:** always pass the **full** values on every `helm upgrade` of the
> `aether` chart — do **not** use `--reuse-values` (it keeps the stale
> digest-pinned image). Bump the chart version per change.

Verify:

```bash
kubectl -n aether-system get pods         # agent + proxy per node, registrar ×2, controller
kubectl get meshconfig                    # the singleton "default" MeshConfig CR
```

---

<a name="configure"></a>
## 4. Configure

Configuration splits into two layers:

### 4a. Deploy-time system config (chart values)

Set once at the top of the `aether` chart's values; every component inherits it.

| Value | Default | What it does |
|---|---|---|
| `clusterName` | `talos-main` | Cluster name in registry keys (registrars are per-cluster; names need not be unique across clusters). |
| `meshDomain` | `aether.internal` | The authority suffix services are addressed under (`<svc>.<meshDomain>`). Also defaults the SPIRE trust domain. |
| `spire.enabled` | `true` | Mesh-wide mTLS switch. |
| `spire.trustDomain` | `""` (→ mesh domain) | SPIFFE trust domain. |
| `spire.workloadSocketPath` | `/run/secrets/workload-spiffe-uds/socket` | Workload API socket. |
| `spire.adminSocket.*` | see values | SPIRE agent admin/Delegated-Identity socket the agent uses to mint proxy SVIDs. |
| `registrar.registryBackend` | `kubernetes` | `kubernetes` \| `dynamodb` \| `etcd`. |
| `registrar.etcd.endpoints` | `[]` | etcd endpoints when backend is `etcd`. |
| `registrar.aws.region` / `agent.awsCredentials` | — | DynamoDB backend config. |
| `otel.enabled` / `otel.endpoint` | `false` / `""` | Turn on OTel and point at an OTLP gRPC collector (`host:port`). |
| `proxy.enabled` | `true` | Deploy the per-node Envoy. Set false to run only the agent. |
| `edge.enabled` | `false` | Deploy the north-south ingress gateway (see §10). |

### 4b. Runtime proxy observability (the `MeshConfig` CR)

The controller seeds a **singleton** `MeshConfig` named `default` on first install
and never overwrites it on upgrade — you own it via `kubectl` thereafter. It
**only** overrides the proxy data plane's observability (access logs, tracing,
per-pod stats); everything else stays inherited from the system config.

```bash
kubectl edit meshconfig default
```

```yaml
apiVersion: config.aether.io/v1
kind: MeshConfig
spec:
  proxy:
    accessLogsEnabled: true
    accessLogSuccessSampleRate: 100   # % of successful requests logged
    tracingEnabled: true
    traceSampleRate: 0.01             # head sampling, 0.0–1.0
    emitStatsPod: false
```

Changes are projected into a ConfigMap the agent mounts and applied to the data
plane without a redeploy.

---

<a name="managed-pods"></a>
## 5. Which pods are managed (and which are never)

### Opt **in** with a label

A pod joins the mesh **only** if it carries:

```yaml
metadata:
  labels:
    aether.io/managed: "true"
```

The CNI plugin programs the outbound listener (and mTLS identity) into a pod's
network namespace at CNI ADD time *only* for labelled pods. A pod also needs at
least one IP. Everything else on the node is left untouched.

### Always-ignored namespaces

These namespaces are **never** intercepted, regardless of labels — the control
plane and the mesh's own dependencies must never rely on the mesh to start up
(otherwise the mesh and SPIRE deadlock each other):

```
kube-system
aether-system
spire-mgmt
spire-server
spire-system
```

(Matching is case-insensitive.) So a `aether.io/managed: "true"` pod in
`kube-system` is still ignored.

### Opt **out**

To exclude a specific pod, simply omit the `aether.io/managed` label (or set it to
anything other than `"true"`). There is no force-include for ignored namespaces.

---

<a name="onboard-a-workload"></a>
## 6. Onboard a workload

A minimal mesh-managed Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-svc
spec:
  replicas: 4
  minReadySeconds: 10                         # see §8
  strategy:
    rollingUpdate: { maxSurge: 1, maxUnavailable: 0 }
  template:
    metadata:
      labels:
        app: my-svc
        aether.io/managed: "true"             # ← opt into the mesh
    spec:
      serviceAccountName: my-svc              # ← SERVICE NAME = ServiceAccount
      containers:
        - name: app
          readinessProbe:                     # gates endpoint promotion
            httpGet: { path: /healthz, port: 8080 }
          lifecycle:
            preStop: { sleep: { seconds: 10 } }   # see §8
```

Your service is now addressable mesh-wide as `my-svc.aether.internal`.

### Optional endpoint annotations

| Annotation | Default | Meaning |
|---|---|---|
| `endpoint.aether.io/port` | `8080` | Application port the mesh routes to. |
| `endpoint.aether.io/weight` | `1` | Load-balancing weight. |
| `endpoint.aether.io/health-path` | `/` | Path the node-local agent health-checks (delegated liveness). |
| `endpoint.aether.io/health-check-mode` | `eds` | `eds` = agent vets the endpoint and publishes health over EDS (clients get pre-warmed endpoints); `active` = every client proxy probes the endpoint itself. |
| `metadata.endpoint.aether.io/<key>` | — | Free-form endpoint metadata, usable as routing subsets (§9). |
| `config.aether.io/upstreams` | — | Comma-separated services this pod **calls** (§7). |

---

<a name="call-other-services"></a>
## 7. Call other services

Send requests to the **local outbound listener** with the destination's mesh FQDN
in the `Host` header:

```bash
# from inside a managed pod
curl http://127.0.0.1:18081/orders -H 'Host: svc-payments.aether.internal'
```

- **Authorities are FQDN-only:** `<service>.<meshDomain>` is the single accepted
  form. Bare names (`Host: svc-payments`), foreign domains, or nested labels match
  no route and **404 immediately**. A `:port` suffix is stripped before routing.
- The callee sees the caller's SPIFFE ID in `x-forwarded-client-cert` (XFCC).

### Declare your upstreams (recommended)

The mesh only distributes a service's config to nodes that need it. Declare what a
pod calls so those clusters are **warm before first use**:

```yaml
metadata:
  annotations:
    config.aether.io/upstreams: "svc-payments,svc-ledger,svc-audit"
```

- **Declared** upstreams are present the moment the pod lands — declare anything
  latency- or correctness-critical.
- **Undeclared** upstreams still work via the cold path (ODCDS): the first request
  pauses ~one node-local xDS round-trip while the cluster is fetched on demand,
  then stays warm (1h idle TTL). Each miss increments
  `aether.agent.upstreams.miss` — the signal to promote it to the annotation.
- A pod's **own** service is always in scope; never declare it.

### Use keep-alive / HTTP-2 connections

The mesh pools upstream mTLS connections **per downstream connection** (so one
pod's certificate is never reused for another pod's traffic). A long-lived client
connection — HTTP/1.1 keep-alive or an HTTP/2/gRPC channel — reuses its mTLS
connection across requests. Connection-per-request clients pay a fresh handshake
every time: it works, but it's the expensive shape.

---

<a name="hitless-rolls"></a>
## 8. Roll without dropping requests

The mesh does most of the work (endpoints go DRAINING the instant deletion is
*requested*, new endpoints arrive pre-vetted, connection-level failures retry on
another endpoint). Two workload settings close the rest — **without them, rolls
outrun the mesh and drop requests**:

1. **`minReadySeconds: 10`** — paces the roll so the old endpoint is retired only
   after the replacement is actually mesh-routable (~5–10s after Ready).
2. **`preStop: { sleep: { seconds: 10 } }`** — delays SIGTERM so in-flight
   requests finish through the two-phase drain. Measured: `sleep 10` → **0 failed
   requests/roll**; `sleep 3` (the supported minimum) → ~1 blip per pod.

Also keep **`maxUnavailable: 0`** and a real **`readinessProbe`** (endpoint
promotion gates on the app actually answering).

**Avoid `--grace-period=0` force deletes** for serving workloads — they skip the
draining phase, so brief errors are possible.

### What the mesh retries for you

On a **different endpoint** (2 attempts, 25–250 ms backoff): `connect-failure`,
`refused-stream`, `reset-before-request`, and `503`. These all fail before
reaching your app, so retries are safe even for non-idempotent traffic.
Application 5xx and timeouts are deliberately **not** retried.

---

<a name="advanced-routing"></a>
## 9. Advanced routing (pinning, subsets, locality)

### Pin to one endpoint

| Header | Meaning |
|---|---|
| `x-aether-ip: 10.42.1.11` | route to exactly that endpoint IP |
| `x-aether-pod: my-svc-7f9c4-xv2qp` | route to exactly that pod |

Pin-or-fail: if the target is gone, you get a 503 — never a silent fallback.

### Provider-defined subsets

Endpoints publish routing dimensions; consumers select them:

```yaml
# on the endpoint (producer)
metadata:
  annotations:
    metadata.endpoint.aether.io/version: "v2"
```

```bash
# on the request (consumer)
curl http://127.0.0.1:18081/ -H 'Host: my-svc.aether.internal' \
     -H 'x-aether-subset-version: v2'
```

Selection is strict (no fallback): asking for a subset with no endpoints **fails**
rather than spilling onto the rest of the service. Up to 4 subset keys intersect
(`...-version: v2` + `...-shard: s1` → endpoints matching both). Keys
`ip`/`pod`/`cluster`/`namespace` are reserved.

### Locality-aware failover

Endpoints carry their node's `topology.kubernetes.io/region` and `zone`. Each
node's proxy prefers same-zone endpoints (priority 0), spilling to same-region (1)
then anywhere (2) only as closer endpoints drain or fail. Nodes without topology
labels express no preference.

---

<a name="edge"></a>
## 10. North-south ingress (the edge gateway)

The optional **edge** is an unprivileged Deployment running Envoy + an
`agent edge` sidecar. It dials mesh pods **directly** over mTLS (the node
DaemonSet is not in its path) and routes external traffic by `EdgeRoute` CRs.

Enable it:

```bash
helm upgrade --install aether oci://ghcr.io/bpalermo/aether/charts/aether \
  --version "$VERSION" -n aether-system \
  --set edge.enabled=true
  # ... plus your other values
```

Expose a service:

```yaml
apiVersion: config.aether.io/v1
kind: EdgeRoute
metadata:
  name: api
  namespace: aether-system          # the namespace the edge watches
spec:
  hosts: [api.example.com]           # external Host/SNI (required, ≥1)
  service: svc-1                     # the mesh service to route to
  port: 8080                         # optional; omit for the default port
```

- The edge routes **only** by explicit external host. The internal FQDN
  (`<svc>.<meshDomain>`) is deliberately **not** routable from the edge — a route
  with no `hosts` exposes nothing.
- **TLS:** set `edge.tls.enabled=true` and reference a `kubernetes.io/tls` Secret
  per route via `spec.tls.secretName`. The edge agent serves the cert to Envoy
  over SDS (SNI-selected, hot rotation, no pod roll) and 301-redirects HTTP→HTTPS.
  Aether does **not** issue certs — bring your own (cert-manager, `kubectl`, …).
- The edge gets its own SVID straight from SPIRE; with `spire.enabled` the chart
  can create its `ClusterSPIFFEID` (`edge.spire.clusterSpiffeID`).

---

<a name="observability"></a>
## 11. Observability

Turn on OTel once at the system level:

```yaml
otel:
  enabled: true
  endpoint: "otel-collector.observability.svc:4317"   # OTLP gRPC
  logs: true            # component logs over OTLP (also tee'd to stderr)
  traceExport: true     # spans (needs a collector traces pipeline)
  traceSampleRate: 0.1
```

- The collector endpoint is **deploy-time** config baked into the CNI plugin and
  Envoy bootstrap (so the fleet rolls atomically and a config change can't break
  CNI) — it is not read from a runtime ConfigMap.
- Proxy access logs / tracing / per-pod stats are retunable at runtime via the
  `MeshConfig` CR (§4b) without a redeploy.
- Control-plane metrics, proxy stats, and a per-request source→destination request
  counter all flow over OTLP. Health-check traffic is excluded from access logs.

---

<a name="troubleshooting"></a>
## 12. Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| App can't reach the mesh (connection refused to `127.0.0.1:18081`) | Pod isn't managed, or the proxy hasn't programmed the listener yet | Confirm `aether.io/managed: "true"` and that the pod is **not** in an ignored namespace (§5); check the agent log for `adding listeners for pod`. |
| Request 404s immediately | Authority isn't a valid mesh FQDN | Use `Host: <service>.<meshDomain>` exactly — bare names and foreign domains 404 at the route table. |
| Request 503s on a pin | The pinned endpoint is gone (drained/ejected/never existed) | Pin-or-fail is by design; drop the `x-aether-ip`/`x-aether-pod` header to load-balance. |
| First call to a service is slow | Cold path (undeclared upstream) — one xDS round-trip | Declare it in `config.aether.io/upstreams`; watch `aether.agent.upstreams.miss`. |
| Dropped requests during a roll | Missing `minReadySeconds` / `preStop` / `maxUnavailable: 0` | Apply all three (§8); avoid `--grace-period=0`. |
| Mesh + SPIRE won't start | SPIRE running in a mesh-managed namespace | SPIRE must live in an ignored namespace (`spire-*`) so it doesn't depend on the mesh. |

### Useful commands

```bash
kubectl -n aether-system logs ds/aether-agent           # control plane + CNI
kubectl -n aether-system logs ds/aether-proxy           # the Envoy data plane
kubectl get meshconfig default -o yaml                  # effective proxy observability
kubectl get edgeroutes -A                               # north-south routes
```

---

## See also

- [`docs/workload-requirements.md`](./workload-requirements.md) — the full
  workload contract (the authoritative source for §6–§9).
- [`charts/README.md`](../charts/README.md) — chart layout, image mirroring,
  versioning/stamping.
- `docs/proposals/` — design records: `003` (edge), `004` (demand-scoped),
  `013` (prober), `014` (access logs), `015` (MeshConfig).
