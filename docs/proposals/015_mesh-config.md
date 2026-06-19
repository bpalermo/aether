# Proposal: aether system config (umbrella globals) + proxy MeshConfig CRD

**Status:** Design — 2026-06-18
**Relates:** every telemetry/security proposal that added a flag (007, 012, 013,
014); [[project_observability_otel]], [[project_telemetry_edge_filter]]

## Final architecture (authoritative)

> This section reflects what ships and supersedes the longer exploration below
> (kept for rationale). The design landed in several steps; read this first.

Aether is treated as **one system**. There are two config scopes:

1. **Aether system config** — set ONCE and inherited by every component
   (registrar, agent, proxy, controller): **OTEL** (enable, OTLP endpoint, logs,
   trace sample rate/export), **SPIRE** (enable), and the **mesh domain**. It is
   expressed as Helm **`global`** values in an umbrella **`aether`** chart and
   passed to each component as flags. Enable OTEL or SPIRE once → the whole
   system inherits it.
2. **Proxy MeshConfig (CRD)** — the proxy data plane may **override** its own
   observability (access logs, tracing, metrics/stats-pod). Unset fields
   **inherit** the aether system config. This is the only thing in the
   `MeshConfig` proto/CRD.

Charts (consolidated — agent/registrar/controller always deploy together, so
they're **one chart**, not separate charts or an umbrella):

- **`charts/crds`**: the `meshconfigs.config.aether.io` **v1** CRD, installable
  ahead of time (`spec` is `x-kubernetes-preserve-unknown-fields`; the webhook is
  the authoritative validator).
- **`charts/aether`**: the whole system in one flat chart — agent DaemonSet +
  per-node Envoy proxy, registrar Deployment, and the `aether-controller`
  (validating webhook + reconciler). System config (otel/spire/meshDomain) is
  top-level `values` referenced directly by each component — no umbrella, no
  `global` indirection, no cross-chart name conventions. The controller projects
  the singleton `MeshConfig` CR into a release-named ConfigMap
  (`<release>-mesh-config`) the agent mounts; it seeds the default CR **once**
  (`lookup` + `resource-policy: keep`) so `helm upgrade` never re-stamps it. The
  webhook serving cert is the **SPIRE SVID** when `spire.enabled` (controller
  injects the trust bundle into `caBundle`), else a Helm self-signed cert. The
  registrar (control plane) does not use MeshConfig.

Images are `repository` + `digest` (in-repo, digest-pinned) or `repository` +
`tag` (the external proxy image); override `repository` to mirror to a private
registry. CNI and the Envoy bootstrap take **deterministic, deploy-time**
config (chart-templated), never a runtime ConfigMap — a config change can't
break CNI on freshly-rolled nodes while old nodes keep working.

Types (`api/aether/config/v1`):

- **`MeshConfigSpec`** proto: a single `proxy` message of **optional** fields —
  `access_logs_enabled`, `access_log_success_sample_rate`, `tracing_enabled`,
  `trace_sample_rate`, `emit_stats_pod` — where unset = inherit.
- **`MeshConfig`** is a hand-written typed Kubernetes CRD object
  (TypeMeta/ObjectMeta/`Spec *MeshConfigSpec`/Status) with a **manual jsonshim**
  (`.spec` (un)marshalled via protojson — strict) and **manual DeepCopy** (proto
  `Spec` cloned via `proto.Clone`), registered on a scheme. The controller and
  webhook work against this typed object — **no `unstructured`**. API version is
  `config.aether.io/v1` (proto field-number evolution guarantees compatibility).

`common/config.{Load,Parse}` is the one validator (YAML/JSON → protojson strict →
protovalidate on `MeshConfigSpec`), reused at agent file-load, controller
reconcile, and admission. The validating webhook is served at `/validate`.

---

## Why (original exploration)

Mesh-wide behavior is currently configured by a steadily growing set of CLI
flags, fanned out by the charts into `args:` lists and mirrored field-for-field
in `values.yaml`. The agent alone now passes `--mesh-domain`, `--otel-enabled`,
`--otlp-endpoint`, `--stats-emit-pod`, `--access-logs-enabled`,
`--access-log-success-sample-rate`, `--proxy-tracing-enabled`,
`--proxy-trace-sample-rate`, `--trace-sample-rate`, `--trace-export`,
`--logs-enabled`, `--spire-enabled`. Each new knob is a four-place edit (flag
registration, `*Config` struct, chart `args`, `values.yaml`) and the registrar
duplicates the telemetry subset. There is no validation beyond cobra's type
parse, and no way to express "this is the mesh's policy" as a single object.

These knobs split cleanly into two kinds:

- **Mesh-wide policy** — values that should be *uniform across every agent,
  proxy and registrar in a mesh*: the mesh domain, the whole telemetry posture,
  and whether mTLS (SPIRE) is on. This is exactly Istio's `MeshConfig` concept.
- **Per-instance / topology** — values that are intrinsically per-binary or
  per-deployment: node/cluster/proxy identity, socket paths, bind addresses,
  the registry backend and its endpoints, sync intervals. These stay flags.

Goal: move **mesh-wide policy** out of flags into a single **protobuf-backed
config object** read from a mounted **ConfigMap**, validated with
**protovalidate** at load. The same proto then backs a **CRD + validating
webhook** so operators can `kubectl apply` mesh config and have it rejected
server-side if invalid — without bloating the charts with one value/flag per
knob.

Per-instance flags are explicitly **out of scope** and stay exactly as they are.

## What moves vs. what stays

| Setting | Today | After |
| --- | --- | --- |
| `mesh-domain` | flag | **MeshConfig** |
| `otel-enabled`, `otlp-endpoint` | flag | **MeshConfig.telemetry** |
| `stats-emit-pod` | flag | **MeshConfig.telemetry** |
| `access-logs-enabled`, `access-log-success-sample-rate` | flag | **MeshConfig.telemetry.access_logs** |
| `proxy-tracing-enabled`, `proxy-trace-sample-rate` | flag | **MeshConfig.telemetry.proxy_tracing** |
| `trace-sample-rate`, `trace-export` | flag | **MeshConfig.telemetry.tracing** |
| `logs-enabled` | flag | **MeshConfig.telemetry.logs** |
| `spire-enabled` | flag | **MeshConfig.security** |
| `node-name`, `cluster-name`, `proxy-id` | flag | **flag (unchanged)** |
| `mounted-registry-dir`, `registrar-address` | flag | **flag (unchanged)** |
| `spire-admin-socket`, `spire-workload-socket`, `spire-trust-domain` | flag | **flag (unchanged)** |
| `registry-backend`, `etcd-endpoints`, `sync-interval`, `grpc-address` | flag | **flag (unchanged)** |
| `metrics-enabled`, `metrics-bind-address`, `health-probe-bind-address`, `debug` | flag | **flag (unchanged)** |

`debug`, `metrics-enabled` and the bind addresses are operational/per-instance
and stay flags. Only the rows marked **MeshConfig** migrate.

This is a **clean break** (no fallback flags — consistent with the proposal-005
directive): once a field lives in `MeshConfig`, its flag is removed.

## Phase 1 — proto + ConfigMap loader

### 1. The proto (`api/aether/config/v1/mesh_config.proto`)

```proto
syntax = "proto3";
package aether.config.v1;
import "buf/validate/validate.proto";
option go_package = "github.com/bpalermo/aether/api/aether/config/v1;configv1";

// MeshConfig is the mesh-wide policy shared by every aether component.
message MeshConfig {
  // DNS-style domain mesh authorities live under (<service>.<mesh_domain>).
  string mesh_domain = 1 [
    (buf.validate.field).required = true,
    (buf.validate.field).string.hostname = true
  ];

  Telemetry telemetry = 2;
  Security  security  = 3;
}

message Telemetry {
  // Enable the OTel MeterProvider (Prometheus bridge) on the control plane.
  bool   enabled       = 1;
  // OTLP gRPC collector endpoint (host:port); empty disables OTLP export.
  string otlp_endpoint = 2 [(buf.validate.field).cel = {
    id: "otlp_endpoint.hostport"
    message: "must be empty or host:port"
    expression: "this == '' || this.matches('^[^:/]+:[0-9]+$')"
  }];

  bool         stats_emit_pod = 3;
  AccessLogs   access_logs    = 4;
  ProxyTracing proxy_tracing  = 5;
  Tracing      tracing        = 6;
  Logs         logs           = 7;
}

message AccessLogs {
  bool enabled = 1;
  // Percent (0-100) of successful requests logged; failures always logged.
  optional uint32 success_sample_rate = 2 [(buf.validate.field).uint32.lte = 100];
}

message ProxyTracing {
  bool enabled = 1;
  // Fraction (0.0-1.0) of requests traced by the data plane.
  optional double sample_rate = 2 [
    (buf.validate.field).double = {gte: 0.0, lte: 1.0}
  ];
}

message Tracing {
  // Head-sampling ratio for control-plane spans (0.0-1.0).
  optional double sample_rate = 1 [
    (buf.validate.field).double = {gte: 0.0, lte: 1.0}
  ];
  // Export spans over OTLP (requires a collector traces pipeline).
  bool export = 2;
}

message Logs {
  // Export control-plane application logs over OTLP.
  bool enabled = 1;
}

message Security {
  // Whether the mesh uses SPIRE mTLS. Socket paths/trust domain stay per-binary
  // flags; this is the mesh-wide on/off policy.
  bool spire_enabled = 1;
}
```

Rate/percentage fields are proto3 `optional` so the loader can distinguish
"unset" (apply the historical default) from an explicit `0` (which is a
meaningful value — e.g. sample nothing). Everything else defaults to its zero
value, which matches today's flag defaults (all the booleans default off).

### 2. Loader (`common/config`)

```go
// Load reads, parses, validates and defaults a MeshConfig from path.
func Load(path string) (*configv1.MeshConfig, error)
```

Pipeline, mirroring Istio's `protomarshal`:

1. `os.ReadFile(path)`.
2. `sigs.k8s.io/yaml`.YAMLToJSON — YAML is the human-friendly ConfigMap format
   and a JSON superset.
3. `protojson.UnmarshalOptions{DiscardUnknown: false}` — **strict**: an unknown
   field (a typo, a stale key) is an error, not a silent drop.
4. `protovalidate.Validate` — the same library already used by the CNI server
   (`agent/internal/cni/server/server.go`). A constraint violation fails startup
   with a precise field path.
5. `withDefaults` — fill unset `optional` fields:
   `access_logs.success_sample_rate=100`, `proxy_tracing.sample_rate=0.01`,
   `tracing.sample_rate=0.1`. Pure function, unit-tested.

The loader is shared; agent and registrar both call it and read the subset they
care about (registrar uses only `telemetry`; the agent uses all of it).

### 3. Wiring

- Add one flag per binary: `--mesh-config` (path), default
  `/etc/aether/mesh-config.yaml`.
- `common/manager`: the telemetry fields (`OTelEnabled`, `OTLPEndpoint`,
  `TraceSampleRate`, `TracingExport`, `LogsEnabled`) stop being registered as
  flags by `RegisterFlags`; they remain fields on `manager.Config` but are
  populated from the loaded `MeshConfig` before `Bootstrap`.
- `agent/internal/cmd` and `registrar/internal/cmd`: in `PersistentPreRunE`,
  after logging is up, `config.Load(cfg.MeshConfigPath)` and copy the mesh-wide
  values onto the existing `*Config` structs. The downstream call sites
  (`proxy.SetAccessLogConfig`, `SetTracingConfig`, `snapshotCache.SetMeshDomain`,
  `SetEmitStatsPod`, the SPIRE bridge gate) are unchanged — only their source
  changes from `cfg.<flag>` to `cfg.<field-from-MeshConfig>`.
- Remove the migrated flag registrations.

### 4. Charts

- New template `templates/mesh-config.yaml` rendering a ConfigMap from a single
  `meshConfig:` values block (structured, not one value per knob):

  ```yaml
  meshConfig:
    meshDomain: aether.internal
    telemetry:
      enabled: false
      otlpEndpoint: ""
      statsEmitPod: false
      accessLogs: {enabled: false, successSampleRate: 100}
      proxyTracing: {enabled: false, sampleRate: 0.01}
      tracing: {sampleRate: 0.1, export: false}
      logs: {enabled: false}
    security:
      spireEnabled: true
  ```

  The ConfigMap `data."mesh-config.yaml"` is `{{ .Values.meshConfig | toYaml }}`
  — the values block *is* the proto in YAML form (camelCase keys match the
  protojson field names), so there is no per-field template plumbing.
- Mount it at `/etc/aether` (read-only) in the agent DaemonSet and registrar
  Deployment; drop the migrated `args`.
- Add `checksum/mesh-config: {{ include (print $.Template.BasePath
  "/mesh-config.yaml") . | sha256sum }}` to the pod template annotations so
  editing the ConfigMap rolls the workload (Phase 1 is **restart-applied**; the
  migrated knobs are all set-once-at-startup globals — `SetAccessLogConfig`
  etc.). Live reload is future work (the storage package already has fsnotify if
  we want it).
- The proxy ConfigMap (Envoy bootstrap) is unaffected — its knobs
  (`otlpEndpoint`, `jsonLogs`, overload) are Envoy bootstrap, not agent flags.
  `telemetry.otlpEndpoint` there is read from the same `meshConfig` block.
- Bump both `Chart.yaml` `version:` (enforced by ci.yaml).

### 5. Tests

- `common/config`: table tests for YAML→proto, strict-unknown rejection, each
  protovalidate constraint (bad hostname, sample_rate > 1, percent > 100, bad
  otlp endpoint), and the defaults pass.
- `helm template` golden: `meshConfig` values render valid YAML the loader round
  trips (a small Go test that loads the rendered ConfigMap fixture).

> **Revision 2026-06-18 (final):** the CRD + webhook ships *with* Phase 1, and
> the chart does **not** render the ConfigMap from values — that would re-stamp
> mesh config on every `helm upgrade` and force operators to curate values to
> keep upgrades no-op. Instead a dedicated **`aether-controller`** owns the
> `MeshConfig` CRD: it validates the CR (protovalidate webhook) and projects it
> into the ConfigMap the agent and registrar mount. The "ConfigMap-from-values"
> template in §4 above is **not** built. The loader, proto and binary wiring
> (§1–§3) are unchanged — the binaries still just load a mounted YAML file. The
> CRD API is **`config.aether.io/v1`** from the start (proto field-number
> evolution gives backward compatibility, so no alpha/beta churn).

## CRD + webhook + controller (ships now)

### Data flow

```
 helm install crds chart ──────────> MeshConfig CRD (install ahead of time)
 helm install controller chart ────> default MeshConfig CR (seeded ONCE)
                          │
        kubectl edit ─────┤ (CREATE/UPDATE)            config.aether.io/v1
                          ▼
        ValidatingWebhook  ── protovalidate ──> reject on violation
                          │ (admitted)
                          ▼
        aether-controller reconciler ── protovalidate ──> projects ConfigMap
                          │                                  "aether-mesh-config"
            ┌─────────────┼─────────────────────────────┐
            ▼             ▼                               ▼
   agent mounts CM   registrar mounts CM         controller reads the CR
   (file load)       (file load)                 (API) for its own telemetry
```

Everything that runs as a workload (agent, registrar) consumes mesh config the
**same way**: mount the projected ConfigMap, `config.Load` the file. Only the
controller — the CR's owner — reads the CR directly, and only for its own
telemetry; it never mounts the CM it produces, so there is no deadlock.

### Why a separate `aether-controller` (not folded into the registrar)

- Uniform consumption: agent **and** registrar just mount the CM. No component
  has to special-case reading the CR (which folding into the registrar forces,
  since a pod can't mount the ConfigMap its own controller produces).
- Folding would push **leader election** and **webhook serving** onto the
  registrar and skew its resource budget; the singleton controller owns those on
  its own footprint.
- New binary `controller/cmd/controller` + image + `charts/controller`.

### CRD

- `meshconfigs.config.aether.io`, **`v1`**, **cluster-scoped**, singleton (name
  `default`). `.spec` carries the `aether.config.v1.MeshConfig` fields.
- `.spec` (and `.status`) are `type: object` with
  **`x-kubernetes-preserve-unknown-fields: true`** — the structural schema is
  permissive and the **webhook is the authoritative validator** (protovalidate
  constraints — CEL, ranges, hostname — don't translate to OpenAPI). New config
  fields therefore need **no CRD change**.
- Hand-authored CRD in a **standalone `charts/crds` chart** so it can be
  installed/upgraded ahead of the controllers (with `helm.sh/resource-policy:
  keep` so uninstall never cascade-deletes CRs). No `controller-gen`/typed Go
  API: the controller reads the CR as `unstructured`; the proto is the single
  schema.

### Validating webhook

- `ValidatingWebhookConfiguration` on `meshconfigs` CREATE/UPDATE, served by the
  controller's controller-runtime webhook server. The handler json-marshals
  `.spec` and runs it through the **same `common/config.Parse`** the file loader
  uses (YAML/JSON → protojson strict → `protovalidate.Validate` → defaults),
  denying with the violation message. One validator, three call sites: agent/
  registrar file load, controller self-config, admission.
- **`failurePolicy: Ignore`** so a down webhook (e.g. during the controller's own
  rollout) never wedges CR writes; the reconciler re-validates before projecting
  and refuses to write a bad ConfigMap (status condition instead) — validation is
  defense-in-depth, not a bootstrap single point of failure.
- **Cert — SPIRE when enabled, else Helm self-signed:**
  - `spire.enabled=true` (chart default): the controller serves the webhook with
    its **SPIRE X.509 SVID** over the Workload API (`common/spire.WebhookServerCert`
    sets `tls.Config.GetCertificate`, which makes controller-runtime skip its
    CertDir watcher), and a leader-only `CABundleInjector` runnable writes the
    **SPIRE trust bundle** into the `ValidatingWebhookConfiguration` caBundle on
    startup and on every SVID rotation (`src.Updated()`). One-way TLS — the
    apiserver isn't a SPIFFE peer. **Requirement:** the controller's SPIRE
    registration entry must list the webhook Service DNS name
    (`<release>-controller-webhook.<ns>.svc`) in `dnsNames`, or the apiserver's
    TLS hostname check fails. No cert files, auto-rotating, no cert-manager.
  - `spire.enabled=false`: Helm `genSignedCert` (self-signed CA, `caBundle` baked
    into the webhook config, `lookup`-reused across upgrades).

### Projection reconciler

- Watches the singleton `MeshConfig` CR, validates with `config.Parse`, and
  upserts the **`aether-mesh-config`** ConfigMap (key `mesh-config.yaml` = the
  validated config re-serialized) in the controller's namespace. Agent and
  registrar mount it by name (shared `aether-system`). They need **no** CRD RBAC.
- On validation failure it sets a `False` `Projected` status condition and leaves
  the last-good ConfigMap untouched. A unit test asserts the projected document
  round-trips back through `config.Parse` (what the controller writes is exactly
  what the binaries accept).

### No re-stamp on upgrade (the core requirement)

- The chart seeds the default `MeshConfig` CR **once**: `lookup` skips rendering
  when `meshconfigs/default` already exists, and `helm.sh/resource-policy: keep`
  stops Helm deleting it when it drops out of the rendered manifest on later
  upgrades. Net effect: a `helm upgrade` is a **no-op for mesh config** — the
  operator owns the CR via `kubectl`, and new chart versions (with new config
  defaults) never clobber it. `meshConfig.createDefault: false` opts out of
  seeding entirely.

### Bootstrap (deadlock-free)

- The **controller reads its own telemetry from the CR via the API** (a direct
  client in `PersistentPreRunE`); it never mounts the CM, so no deadlock. Missing
  CR → telemetry-off defaults until it appears (+ a restart).
- The **agent and registrar wait in `ContainerCreating`** until the reconciler
  has created the CM, then start — a non-optional `configMap` volume blocks the
  mount but does **not** crashloop, so ordering resolves itself.
- Telemetry/security are **set-once at startup**; a CR edit is applied by a
  workload restart. Live reload remains future work (the storage package has
  fsnotify if we want it).

## Decisions

- **Mesh-wide only.** Per-instance/topology flags are untouched (user directive).
  The line is "must this be identical across the whole mesh?".
- **Clean break, no fallback flags** for migrated knobs (proposal-005 precedent).
- **CRD ships now** in a standalone **`crds`** chart (install ahead of time),
  with a dedicated **`aether-controller`** binary/chart for the webhook +
  reconciler — not folded into the registrar (leader election + webhook +
  resource skew).
- **`config.aether.io/v1`** from the start; proto evolution gives backward
  compatibility. `spec` is `x-kubernetes-preserve-unknown-fields`.
- **One loader** (`common/config`): YAML/JSON → protojson strict → protovalidate
  → defaults, reused at file-load, controller self-config, and admission.
- **Webhook authoritative**, `failurePolicy: Ignore` + reconciler re-validation
  backstop. Serving cert is the **SPIRE SVID when SPIRE is enabled** (controller
  injects the trust bundle into caBundle, auto-rotating), else a Helm self-signed
  cert; no cert-manager either way.
- **Everyone mounts the projected CM**; only the controller reads the CR.
- **Seed-once CR** (`lookup` + `resource-policy: keep`): no re-stamp on upgrade,
  no operator value-curation.
- **Restart-applied**; live reload deferred.
```
