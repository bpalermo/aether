# Proposal: proxy access logs to VictoriaLogs over OTLP

**Status:** Design — 2026-06-17
**Relates:** proposal 007 (telemetry filter), proposal 013 (prober);
[[project_observability_otel]]

## Why

The data-plane proxy emits **metrics** (native `envoy_*` + `aether_stats`) over its
OTLP stat sink, but **no access logs** today — `buildHTTPConnectionManager`
(`agent/internal/xds/proxy/networkfilter.go`) sets no `access_log`. Metrics give
rates and the response-flag breakdown but not the per-request detail needed to
answer "*which* request, from *which* source, to *which* upstream host, failed
with *which* flag, and when" — the questions that matter when chasing a churn drop
or a single bad client. We now run a logs backend (VictoriaLogs) that ingests OTLP
logs at:

```
http://victorialogs-victoria-logs-single-server.o11y.svc.cluster.local:9428/insert/opentelemetry/v1/logs
```

Goal: ship Envoy access logs as OTLP logs to VictoriaLogs, **reusing the existing
collector path** the stats sink already uses, with per-request fields mapped to
log attributes and **volume controlled** (sample + always-log-failures), so the
logs are queryable per source/destination/node/flag and correlate with traces.

## What exists to reuse

- The proxy bootstrap (`charts/agent/templates/configmap.yaml`) already defines an
  **`otel_collector` cluster** (OTLP gRPC, endpoint from `telemetry.otlpEndpoint`)
  and an `envoy.stat_sinks.open_telemetry` sink with the proxy's **per-node k8s
  identity** on the OTLP resource (`k8s.node.name`, …). Access logs reuse the same
  cluster and identity.
- The `otel-collector` (contrib image) already runs an OTLP receiver and exports
  metrics; it gains a **logs pipeline**.

## Design

```
Envoy HCM (egress out_http + inbound)               otel-collector                 VictoriaLogs
  envoy.access_loggers.open_telemetry  --OTLP/gRPC-->  otlp receiver  --OTLP/HTTP-->  /insert/opentelemetry/v1/logs
   -> grpc_service: otel_collector cluster            logs pipeline:
                                                      [batch] -> otlphttp/victorialogs
```

Go through the collector (not Envoy → VictoriaLogs direct) because Envoy's
OpenTelemetry access logger speaks **OTLP gRPC**, the VL endpoint is **OTLP/HTTP**,
and the `otel_collector` cluster + batching + per-node resource are already wired.

### 1. Envoy access logger (agent HCM builder)

Add an `envoy.access_loggers.open_telemetry`
(`OpenTelemetryAccessLogConfig`) to `buildHTTPConnectionManager`, gated on a new
value (`telemetry.accessLogs.enabled`). `common_config.grpc_service` →
`envoy_grpc{ cluster_name: otel_collector }`. Map the rich format we already use
in the e2e Envoy configs to OTLP:

- **body**: the human-readable line (`%RESPONSE_CODE% %RESPONSE_FLAGS% …`).
- **attributes** (`KeyValueList`, for querying): `response_code`, `response_flags`,
  `method`, `authority`, `path`, `protocol`, `duration_ms`,
  `upstream_service_time`, `bytes_sent/received`, `upstream_host`,
  `upstream_cluster`, `x_request_id`, `user_agent`, and the aether netns filter
  states (`%FILTER_STATE(aether.network.network_namespace)%`).
- **trace correlation**: include `trace_id`/`span_id` operators so logs join to
  traces when tracing is on.

Apply on **both** the egress `out_http` HCM and the per-pod inbound HCM, tagged
with `reporter=source|destination` (same source/destination duality as the
`aether_stats` metrics) so a request is queryable from either side.

### 2. Volume control (mandatory — per-request at mesh scale)

At thousands of req/s, logging every request is untenable. Use Envoy access-log
**filters**, default to *errors-always + sampled-success*:

- **always** log when interesting: `response_flag_filter` (any flag set) OR
  `status_code_filter` (>= 500) OR connect failures.
- **sample** the rest: `runtime_filter` / `extension_filter` at a low rate
  (e.g. 1–5%), configurable.

This keeps the failure signal complete (the cases we built the prober/flag-SLI
for) while bounding steady-state volume.

### 3. Collector logs pipeline

Add to the otel-collector config (in-cluster; reuse-values per
[[feedback_observability_reuse_values]]):

```yaml
exporters:
  otlphttp/victorialogs:
    logs_endpoint: http://victorialogs-victoria-logs-single-server.o11y.svc.cluster.local:9428/insert/opentelemetry/v1/logs
service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch]   # + resource/attributes if VL stream fields need shaping
      exporters: [otlphttp/victorialogs]
```

`otlphttp.logs_endpoint` is set to the **full** VL path (the exporter otherwise
appends `/v1/logs`). VictoriaLogs maps OTLP resource + log attributes to fields;
confirm VL **stream fields** (e.g. `k8s.node.name`, `destination_service`) and the
`_msg` field via VL ingestion params/headers so queries (`LogsQL`) are efficient.

### 4. Per-node identity & retention

Reuse the bootstrap's `k8s.node.name`/pod resource attrs (already on the proxy
OTLP resource) so logs are filterable per node/pod. Set a VL retention budget
appropriate to the sampled volume.

## Alternatives considered

- **Envoy → VictoriaLogs direct** — Envoy's OTel access logger is OTLP gRPC; the VL
  endpoint is OTLP/HTTP, and direct wiring loses the collector's batching, the
  existing `otel_collector` cluster, and central sampling/shaping. Rejected.
- **File/stdout access log + node log shipper (vector/promtail/fludbit)** — adds a
  second per-node agent and a parsing step; the OTLP path already exists. Rejected
  unless we want stdout logs regardless.
- **Log everything (no sampling)** — volume/cost untenable at mesh request rates.

## Open questions

1. **Sampling policy**: default success sample rate and the always-log predicate
   set (flags-only vs >=500 vs both).
2. **Inbound, egress, or both**: both (source/destination duality) vs egress-only
   to halve volume.
3. **VictoriaLogs OTLP specifics**: required stream-field / `_msg` mapping and
   whether attributes need a `resource`/`transform` processor to land as VL fields.
4. **Trace correlation**: only meaningful once proxy tracing is enabled — ship the
   trace_id fields now (cheap) or defer.
5. **Retention/quota** for the sampled volume.

## Rollout

1. Collector logs pipeline → VictoriaLogs (no proxy change yet; verify VL ingests a
   hand-sent OTLP log).
2. Agent HCM access logger behind `telemetry.accessLogs.enabled` (default off) +
   the filter/sampling values; chart-template tests.
3. Enable on talos-main at a low sample rate; confirm in VL: errors complete,
   success sampled, fields queryable, per-node/source/destination filterable.
4. Tune sampling; add a Grafana "access logs" explore over the VL datasource.

## Validation

With it enabled at a low rate: drive churn (a proxy roll / svc deploy), then in
VictoriaLogs (LogsQL) confirm the failure records are present and complete
(`response_flags` set), success is sampled, and each record carries
`reporter`, `destination_service`, `k8s.node.name`, and `x_request_id`. Cross-check
a failed `x_request_id` appears on both the source and destination reporter.
