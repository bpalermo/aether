# Proposal: synthetic mesh-availability prober

**Status:** Design — 2026-06-17
**Relates:** proposal 007 (telemetry filter), proposal 004 (demand-scoped distribution)
**Motivated by:** 1h distributed churn soak (2026-06-17)

## Why

The mesh's request metric (`aether_requests_total`, from the `aether_stats`
filter — proposal 007) is emitted **by the data-plane proxy** as it processes a
request. A failure is only recorded when a proxy is alive enough to process the
request and synthesize a response (even a flagged 503). It is therefore
structurally blind to the failures that dominate **during churn** — exactly the
events we want an SLO for:

- connection refused at the local egress listener (`127.0.0.1:18081`) while the
  source proxy's listener is briefly down (hot-restart handoff / pod gap),
- mid-flight resets when the source proxy is itself the component restarting,
- anything that produces no HTTP response → no access-log/stat.

The self-reported number is thus **biased high precisely during hot-restarts and
rollouts**. The 2026-06-17 soak quantified it: a single-source run saw **~501,502**
client failures during churn while the mesh metric recorded **9** (0.0018%). This
is inherent to self-telemetry — the data plane cannot report its own outage — and
is true of any Envoy/Istio mesh.

**A black-box client probe is the standard complement.** The same soak, rerun
**distributed** (k6 DaemonSet, one client per node), measured the truth: **99.98%**
through 3 deploys + 3 hot-restarts. We want that signal permanently, in-repo, as a
first-class component — and the soak also showed the load **must be distributed**
(a single-source client makes one rolling proxy look like a full outage).

## What it is

A small Go binary, `prober`, deployed as a **DaemonSet (one pod per node)**,
**mesh-managed** like any client (`aether.io/managed=true` +
`config.aether.io/upstreams`). Each pod issues a steady, low-rate stream of HTTP
probes through its node's local egress listener
(`GET http://127.0.0.1:18081/` with `Host: <target>.aether.internal`) and emits
its **own** pass/fail counter via OTel. Because the prober (not the proxy) emits
the metric, it survives proxy restarts and records connection-level failures the
mesh metric cannot.

One-per-node is load-bearing: each node's prober observes *its* proxy's roll, so a
node-proxy outage shows as that node's prober failing — the correct attribution,
since that node's real clients would fail too.

## Design

### Component layout (mirrors `agent` / `registrar` / `cni`)

```
prober/
  cmd/prober/                 # cobra entrypoint + BUILD (go_binary, image)
  internal/probe/             # sampler loop, HTTP client, result classification
  internal/config/            # flags/env -> Config
  internal/telemetry/         # OTel meter (reuse pattern from
                              # agent/internal/proxy/hotrestart/telemetry.go)
```

Build via Bazel exactly like the other binaries: `go_binary` +
`//bazel/img:go_multi_arch_image` → distroless image; `make gazelle`.

### Sampler (open-loop, per target)

- One goroutine per target driven by a `time.Ticker` at `--rate` probes/sec
  (default 5/s/target). **Open-loop on purpose** (same rationale as the k6
  `constant-arrival-rate` rule): never back off when latency rises, or a slow
  hop hides the very dip we measure.
- A bounded worker pool services ticks so a hung probe never blocks the next tick;
  if in-flight exceeds the cap, the tick is counted as a `saturated` result rather
  than dropped silently.
- Per-probe: `GET http://127.0.0.1:18081/<path>` with
  `Host: <target>.aether.internal`, `--timeout` (default 2s), no keep-alive reuse
  across the handoff boundary is forced off — actually *do* keep a small idle pool
  to mirror real clients, configurable via `--reuse-conns`.

### Result classification (the whole point)

Map the Go client outcome to a small, low-cardinality `result`:

| `result` | condition | mesh self-metric sees it? |
|---|---|---|
| `success` | nil err, 2xx | yes |
| `http_4xx` / `http_5xx` | nil err, non-2xx | usually (flagged 503) |
| `connection_error` | dial refused / reset (`ECONNREFUSED`/`ECONNRESET`) | **no — the blind spot** |
| `timeout` | context deadline exceeded | rarely |
| `saturated` | worker pool full (prober-side) | n/a |

`connection_error` is the class the mesh cannot record and the reason this
component exists.

### Metrics (the SLI)

Emitted through the existing OTel → `otel-collector` → Prometheus pipeline:

- `aether_probe_requests_total{target, result}` — counter.
- `aether_probe_request_duration_seconds{target}` — histogram (optional latency SLO).

Per-node de-collapse via `OTEL_RESOURCE_ATTRIBUTES=k8s.node.name=$(NODE_NAME)`
(downward API `spec.nodeName`) + the collector `transform/promote` already in
place — same approach as PR #210 for the infra clusters. Keep cardinality low:
`result` is bucketed (no raw status codes), `target` is the short list, `node`
comes from the resource attr.

**SLI** = `sum(rate(...{result="success"})) / sum(rate(aether_probe_requests_total))`,
sliceable by `target` and `node`. This is the trustworthy availability number
across churn.

### Configuration (flags, with env fallbacks)

| flag | default | meaning |
|---|---|---|
| `--targets` | (required) | `svc-1,svc-2,svc-3` — probed services |
| `--egress` | `127.0.0.1:18081` | local mesh egress listener |
| `--mesh-domain` | `aether.internal` | Host authority suffix |
| `--rate` | `5` | probes/sec **per target** |
| `--timeout` | `2s` | per-probe deadline |
| `--path` | `/` | request path |
| `--otlp-endpoint` | "" | collector; empty disables telemetry |

The chart templates `config.aether.io/upstreams` from `--targets` so the agent
programs the matching clusters (demand-scoped distribution, proposal 004).

### Deployment

New optional chart `charts/prober` (DaemonSet + values), **off by default**,
enabled per-cluster. Non-hostNetwork pod (so the CNI plumbs the per-pod
`127.0.0.1:18081`, as `loadgen` does), `aether.io/managed=true`,
`NODE_NAME` via downward API. Minimal resources; rate kept low (it is an SLI
sampler, not load).

### Observability wiring (follow-up, not this component)

- Add a **"Mesh Availability (synthetic)"** dashboard row on
  `aether_probe_requests_total` — overall + per-node + per-target — and make it the
  **headline SLO**. Demote/relabel the self-reported "Mesh Request Success Rate"
  panel ("proxy-observed, not churn-safe").
- Alert (burn-rate) on probe success, never on `aether_stats`.
- Overlay `aether_supervisor_epoch` bumps so a dip maps to a node/epoch.

Division of labor: **prober = availability SLO**; `aether_stats` flag breakdown =
failure-**mode** diagnosis; supervisor metrics = **when/where** churn happened.

## Alternatives considered

- **Permanent k6 DaemonSet** — proved the concept in the soak, but its
  OTLP-exported counter *magnitudes* are unreliable (read 720/s then 60M for the
  same load; only the ratio is usable) and it is heavyweight for an always-on
  component. A purpose-built Go counter is exact and tiny.
- **blackbox-exporter** — pull-model and not mesh-native; would still need a
  mesh-managed sidecar to reach `127.0.0.1:18081` and can't express the
  Host-authority egress cleanly. More moving parts, less control over result
  classification.
- **Access-log scraping** — same data-plane origin, same blind spot, plus volume.
- **In-app client instrumentation** — requires touching every workload; the mesh
  should provide an availability signal without that.

## Open questions

1. **Probe targets:** real services (`svc-1/2/3` — real-path coverage, tiny added
   load) vs a dedicated lightweight echo target (isolates the prober from app
   load). Recommend configurable; default to real services in `aether-test`.
2. **Rate vs sensitivity:** 5/s/node catches ~hundreds-of-ms gaps; raise to
   10–20/s/node for sub-second-blip detection. One knob, documented.
3. **Inbound coverage:** start with egress (the SLI that matters); consider an
   inbound/per-pod variant later.
4. **Identity/authz:** the prober gets a SPIFFE SVID like any managed pod; confirm
   its `source_service` (ServiceAccount) and that `config.aether.io/upstreams`
   authorizes its targets.
5. **Latency SLO:** ship the duration histogram now or defer.

## Rollout

1. `prober` binary + Bazel + image (no cluster impact).
2. `charts/prober` (off by default); enable on talos-main; validate the SLI tracks
   k6 during a manual proxy roll.
3. Dashboard row + alerts; demote the self-reported panel.

## Validation

Reproduce the soak: enable the prober, run a proxy hot-restart
(`kubectl rollout restart ds/aether-proxy`) and a service deploy, and confirm the
prober SLI dips/records `connection_error` while `aether_requests_total` shows ~0
infra failures — i.e., the prober sees what the mesh cannot. Confirm per-node
attribution (only the rolling node's prober dips) and that steady-state reads
~100%.
