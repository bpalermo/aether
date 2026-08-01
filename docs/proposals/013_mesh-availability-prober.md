# Proposal: synthetic mesh-availability prober

**Status:** Implemented — the mesh-availability prober shipped (`charts/prober`).
(Originally 2026-06-17; amended: probe a proxy local-reply liveness endpoint, not a
real service.)
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
**mesh-managed** like any client (`aether.io/managed=true`). Each pod issues a
steady, low-rate stream of HTTP probes through its node's local egress listener
(`127.0.0.1:18081`) and emits its **own** pass/fail counter via OTel. Because the
prober (not the proxy) emits the metric, it survives proxy restarts and records
the connection-level failures the mesh metric cannot.

**Probe target is a proxy local-reply, not a real service.** Probing
`svc-1/2/3` would fold the *application's* health into the mesh SLI — the same
app-vs-mesh confusion we removed from the dashboard. Instead the prober hits a
liveness endpoint the proxy answers **locally** (`direct_response`, no upstream),
so a non-200 / connection error is unambiguously the **mesh's** fault, decoupled
from both the app and upstream reachability.

One-per-node is load-bearing: each node's prober observes *its* proxy, so a
node-proxy outage shows as that node's prober failing — the correct attribution,
since that node's real clients would fail too.

## Two tiers: liveness (primary) vs reachability (optional)

A local-reply probe is green even if the proxy can't route anywhere (no clusters,
stale EDS). So it measures **liveness**, not **reachability**. This mirrors the
mesh's own structural-health split (local-pod HC = liveness, tunnel HC =
reachability):

| Tier | Target | Proves | Decoupled from |
|---|---|---|---|
| **Liveness** (primary) | egress `/-/-/live` local-reply (`direct_response` 200) | source proxy listener serving + config loaded + mTLS up | app **and** upstream |
| **Reachability** (optional) | a dedicated trivial **echo** upstream, full round-trip | routing + EDS propagation + transport + dest-proxy inbound | app health (echo is always-up) |

With both you can distinguish *"proxy down"* (liveness dips — the hot-restart gap)
from *"proxy up but can't deliver"* (reachability dips — EDS/propagation lag during
a deploy). A third, cheaper middle option is the existing `/healthz/<cluster>`
health gateway (`agent/internal/xds/proxy/healthgateway.go`), which surfaces the
proxy's *own* active-HC verdict as a local reply — reachability signal without a
real round-trip, but it is the proxy's self-assessment, so the echo round-trip is
the truly independent reachability check.

aether already has the liveness primitive on the **inbound** listener
(`agent/internal/xds/proxy/ingress.go`):

- `MeshLivePath = "/-/-/live"` — 200 proves config loaded + listener serving +
  mTLS up (no app, no upstream).
- `MeshReadyPath = "/-/-/ready"` — liveness **plus** the pod's app-health cluster;
  re-couples app health, so **not** a mesh-SLI target.

## Design

### Small agent xDS addition (prerequisite for the liveness tier)

The prober is an **egress** client, but `/-/-/live` exists only on the inbound
listener. So the agent emits a reserved liveness route on the **egress** route
config: `direct_response: { status: 200 }` for a reserved authority/path (e.g.
`127.0.0.1:18081/-/-/live`). This is nearly free — the agent already generates the
egress route config and already uses `direct_response` (the catch-all 404 in
`route.go`, the `/healthz/<cluster>` gateway). The route is always present,
independent of EDS/clusters/apps, so a 200 means "this node's egress data plane is
serving" and a connection error means the listener is down.

### Component layout (mirrors `agent` / `registrar` / `cni`)

```
prober/
  cmd/prober/                 # cobra entrypoint + BUILD (go_binary, image)
  internal/probe/             # sampler loop, HTTP client, result classification
  internal/config/            # flags/env -> Config
  internal/telemetry/         # OTel meter (reuse pattern from
                              # agent/internal/proxy/hotrestart/telemetry.go)
```

Build via Bazel like the other binaries: `go_binary` +
`//bazel/img:go_multi_arch_image` → distroless image; `make gazelle`.

### Sampler (open-loop)

- A `time.Ticker` at `--rate` probes/sec (default 5/s) issues
  `GET http://127.0.0.1:18081/-/-/live` (liveness tier) and, if enabled, a
  round-trip to each `--reachability-target` (reachability tier).
- **Open-loop on purpose** (same rationale as the k6 `constant-arrival-rate` rule):
  never back off when latency rises, or a slow hop hides the dip we measure.
- A bounded worker pool services ticks so a hung probe never blocks the next tick;
  over-cap ticks count as `saturated` rather than silently dropping.
- `--timeout` (default 2s) per probe.

### Result classification (the whole point)

| `result` | condition | mesh self-metric sees it? |
|---|---|---|
| `success` | nil err, 200 | yes |
| `http_error` | nil err, non-2xx | usually (flagged 503) |
| `connection_error` | dial refused / reset (`ECONNREFUSED`/`ECONNRESET`) | **no — the blind spot** |
| `timeout` | context deadline exceeded | rarely |
| `saturated` | worker pool full (prober-side) | n/a |

`connection_error` against the **liveness** endpoint is the unambiguous
"mesh/proxy down" signal and the reason this component exists.

### Metrics (the SLI)

Emitted through the existing OTel → `otel-collector` → Prometheus pipeline:

- `aether_probe_requests_total{tier, target, result}` — counter
  (`tier` = `liveness` | `reachability`).
- `aether_probe_request_duration_seconds{tier, target}` — histogram (optional).

Per-node de-collapse via `OTEL_RESOURCE_ATTRIBUTES=k8s.node.name=$(NODE_NAME)`
(downward API) + the collector `transform/promote` already in place (à la PR #210).
Cardinality stays low: `result` is bucketed (no raw status codes), `tier`/`target`
are tiny, `node` from the resource attr.

**Liveness SLI** =
`sum(rate(...{tier="liveness",result="success"})) / sum(rate(...{tier="liveness"}))`,
sliceable by `node`. This is the trustworthy data-plane-availability number across
churn.

### Configuration (flags, with env fallbacks)

| flag | default | meaning |
|---|---|---|
| `--egress` | `127.0.0.1:18081` | local mesh egress listener |
| `--liveness-path` | `/-/-/live` | proxy local-reply liveness route |
| `--rate` | `5` | probes/sec |
| `--timeout` | `2s` | per-probe deadline |
| `--reachability-targets` | "" (disabled) | optional echo upstreams, full round-trip |
| `--mesh-domain` | `aether.internal` | Host authority suffix (reachability tier) |
| `--otlp-endpoint` | "" | collector; empty disables telemetry |

Note the liveness tier needs **no** `config.aether.io/upstreams` — it never leaves
the proxy. Only the optional reachability tier requires the echo upstream to be
declared (demand-scoped distribution, proposal 004).

### Deployment

New optional chart `charts/prober` (DaemonSet + values), **off by default**.
Non-hostNetwork pod (so the CNI plumbs the per-pod `127.0.0.1:18081`, as `loadgen`
does), `aether.io/managed=true`, `NODE_NAME` via downward API. Minimal resources;
rate kept low (it is an SLI sampler, not load).

### Observability wiring (follow-up, not this component)

- Add a **"Mesh Availability (synthetic)"** dashboard row on
  `aether_probe_requests_total{tier="liveness"}` — overall + per-node — as the
  **headline SLO**, and demote/relabel the self-reported "Mesh Request Success
  Rate" panel ("proxy-observed, not churn-safe"). Add a reachability panel from
  `tier="reachability"`.
- Alert (burn-rate) on the liveness SLI, never on `aether_stats`.
- Overlay `aether_supervisor_epoch` bumps so a dip maps to a node/epoch.

Division of labor: **prober liveness = availability SLO**; prober reachability =
delivery health; `aether_stats` flag breakdown = failure-**mode** diagnosis;
supervisor metrics = **when/where** churn happened.

## Alternatives considered

- **Probe a real service (`svc-1/2/3`)** — rejected: folds app health into the
  mesh SLI (the app-vs-mesh confusion already removed from the dashboard) and adds
  load to real services. The local-reply liveness endpoint isolates the mesh.
- **Permanent k6 DaemonSet** — proved the concept in the soak, but its
  OTLP-exported counter *magnitudes* are unreliable (720/s then 60M for the same
  load; only the ratio is usable) and it is heavyweight for an always-on
  component. A purpose-built Go counter is exact and tiny.
- **blackbox-exporter** — pull-model, not mesh-native; still needs a mesh-managed
  sidecar to reach `127.0.0.1:18081`, with less control over result classification.
- **Access-log scraping** — same data-plane origin, same blind spot, plus volume.
- **In-app client instrumentation** — requires touching every workload.

## Open questions

1. **Reachability echo target:** ship a dedicated always-up echo service (a tiny
   `direct_response`-style backend) for the reachability tier, or defer the tier
   entirely and start liveness-only? (Liveness alone already closes the soak's
   blind spot.)
2. **Rate vs sensitivity:** 5/s/node catches ~hundreds-of-ms gaps; raise to
   10–20/s for sub-second-blip detection. One knob, documented.
3. **Identity/authz:** the prober gets a SPIFFE SVID like any managed pod; the
   liveness tier needs no upstream authz; the reachability tier needs its echo
   target in `config.aether.io/upstreams`.
4. **Latency SLO:** ship the duration histogram now or defer.

## Rollout

1. Agent egress liveness `direct_response` route (small xDS change) + a route test.
2. `prober` binary + Bazel + image (no cluster impact).
3. `charts/prober` (off by default); enable on talos-main; validate the liveness
   SLI tracks k6 during a manual proxy roll.
4. Dashboard row + alerts; demote the self-reported panel. Reachability tier later.

## Validation

Reproduce the soak: enable the prober (liveness tier), run a proxy hot-restart
(`kubectl rollout restart ds/aether-proxy`) and confirm the prober records
`connection_error` on the rolling node while `aether_requests_total` shows ~0 infra
failures — i.e., the prober sees what the mesh cannot. Confirm per-node attribution
(only the rolling node's prober dips) and steady-state ~100%. With the reachability
tier enabled, a service deploy should dip reachability (EDS/propagation) while
liveness stays flat.
