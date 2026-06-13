# Proposal: Source↔Destination Telemetry via a Rust Dynamic Module

**Status:** Draft — Phase-1 module validated end-to-end on stock distroless Envoy 1.38.0 (2026-06-13)
**Author:** Bruno Palermo
**Date:** 2026-06-13

## Problem Statement

The mesh today cannot answer "**which caller saw failures talking to which
service**." Two structural reasons:

1. **Destination-only, mesh-wide-collapsed request stats.** Native Envoy
   request metrics live under `cluster.<dest>.*` (keyed by destination only).
   The OTel sink exports `envoy_cluster_upstream_rq_*{aether_cluster=<dest>}`
   with no source dimension, and because the sink stamps no per-proxy identity,
   every node's counters for a given destination **collapse into one mesh-wide
   series** (measured 2026-06-13: `envoy_cluster_upstream_rq_2xx_total{aether_cluster="svc-1"}`
   is a single series). There is no source attribution at all.

2. **The proxy is node-shared, so source can't be a static tag.** `aether-proxy`
   is a node-level DaemonSet. Outbound listeners/HCMs are per-pod
   (`out_http_<pod>`, netns-bound), but **egress clusters are shared by every
   local pod** (`<svc>.<meshDomain>`, see `egress.go:ServiceClusterName`). A
   bootstrap `stats_tags` `fixed_value` is constant for the whole node Envoy, so
   it can only stamp `source_node`, never `source_service`. Native stats
   fundamentally cannot co-locate source and destination on one series here:
   the source pod is known only at the listener stat tree, the destination only
   at the (shared) cluster stat tree.

We want Istio-style edge metrics — `{reporter, source_service,
destination_service, response_code, response_flags}` — for both the
source-reported (outbound) and destination-reported (inbound) views, so e2e
runs and production incidents can be assessed from metrics alone (no log
spelunking), including attributing the connect-refused drops the source side
sees at last-old-pod exit.

See [[project_per_service_final_drop]] (final-drop is already per-service via
`cluster.upstream_rq_5xx`; this proposal adds the **edge** dimension),
[[project_source_dest_metrics]] (why the cheap/native path is impossible here),
[[project_envoy_stats_cardinality]] (the cardinality discipline this must
respect), and proposals 004 (demand-scoped distribution) / 005 (multi-port).

## Design Goals

1. **Source↔destination edge metrics**, service granularity by default, both
   reporters (source on outbound, destination on inbound).
2. **In-proxy aggregation.** Per-request work increments **native Envoy
   counters** — never a per-request export. This keeps the counters cumulative
   and exact, reuses the existing OTel stat sink unchanged, and avoids the
   access-log→collector volume that drove Istio off per-request telemetry.
3. **No custom Envoy build.** Load the extension at runtime.
4. **Response-cause visibility.** Carry `response_flags` (UF/UH/URX…) so a
   connect-refused drop is distinguishable from no-healthy-host.
5. **Bounded cardinality.** Service-granularity + response-code class by
   default; `*_pod` and full status code are opt-in. Respect demand-scoping;
   never reintroduce the per-pod stat explosion collapsed in stats round 2.
6. **Reuse the existing pipeline.** Metrics must surface as Envoy stats so
   `envoy.stat_sinks.open_telemetry` → collector → Prometheus carries them with
   no new export path.

## Why a custom extension (and not the alternatives)

| Approach | Verdict |
|---|---|
| Native stats + tags | **Impossible** on a node-shared proxy (source and dest are in different, partly-shared stat trees; `fixed_value` gives only `source_node`). |
| OTLP access-log → collector `count` connector | Works (spiked GREEN 2026-06-13) but **per-request export**, needs a contrib collector + `deltatocumulative`, and showed **loose counts**. Rejected as the steady-state design; acceptable only as a stop-gap. |
| **Custom extension, in-proxy aggregation** | The right shape: per-request callback reads identities + outcome and increments cumulative Envoy counters. Exact, no export volume, core collector unchanged. **Chosen.** |

## Technology evaluation (the requested WASM / Rust / C++ comparison)

| | WASM (proxy-wasm) | **Rust dynamic module** | Native C++ |
|---|---|---|---|
| Per-request cost | Highest — VM boundary + arg serialization on every property read & `record_metric` | **Native** — in-process `.so`, direct C-ABI, no VM | Native (floor) |
| Build | None (runtime) | **None (runtime `.so`)**, ABI pinned to Envoy version | Custom proxy build |
| Isolation | Sandboxed | In-process (crash → proxy) | In-process |
| Maturity | Proven by Istio telemetry-v2 — **then abandoned for perf** | New but capability-complete in 1.38 (see spike) | Istio's current default |
| Capability (1.38) | peer SAN via properties; flat-named metrics only | **Full** (spike below) | Full |

**Decisive precedent:** Istio shipped telemetry on WASM, measured unacceptable
always-on per-request overhead at scale, and moved its default to a **native**
stats filter. For every-request telemetry the WASM VM tax is the known wall.

**Spike result (2026-06-13) locks the choice → Rust dynamic module.** The
v1.38 dynamic-modules ABI (`source/extensions/dynamic_modules/abi/abi.h`) and
the in-tree ABI-matched Rust SDK (`sdk/rust/src/access_log.rs`) expose every
capability the design needs, with native performance and no custom build:

- Source/dest identity: `downstream_peer_uri_san()`, `upstream_peer_uri_san()`,
  `upstream_cluster()`, `get_dynamic_metadata(filter,key)`, `get_filter_state(key)`.
- Outcome: `response_code()`, `has_response_flag(flag)`, `virtual_cluster_name()`.
- In-proxy metrics: `define_counter()`, `increment_counter()`,
  `define_histogram()`, `record_histogram()`.

(The crates.io `envoy-dynamic-modules-rust-sdk` 0.1.1 is a **stale thin mirror**
— docs show HTTP-filter header manipulation only. Use the **in-tree SDK at the
matching Envoy tag**, not crates.io.)

**A better-fit extension point than an HTTP filter:** the spike found a
dynamic-module **access-logger** variant (`access_loggers/dynamic_modules`)
whose ABI carries *all* of the above plus the counter callbacks. An access
logger fires **once per request at log time** (final response code/flags
available) and is attached to the HCM via `access_log:` — no filter-chain
ordering concerns, no per-request decode/encode hooks. We use the access-logger
variant, not an HTTP filter.

## Design

### Extension placement
Attach the dynamic-module access logger to the per-pod HCMs the agent already
generates:
- **Outbound** (`buildDefaultOutboundHTTPFilterChain`) → emits `reporter=source`.
- **Inbound** (`buildInboundFilterChain`) → emits `reporter=destination`.

### Identity: agent injects *local*, the module derives *remote*

| Direction / reporter | Local (agent-injected, per-pod) | Remote (module reads per-request) |
|---|---|---|
| Outbound / source | `source_service`,`source_pod` via listener `metadata` (read with `get_dynamic_metadata`) | `destination_service` from `upstream_cluster()` → strip `.<meshDomain>[:port]` (`ServiceFromClusterName`) |
| Inbound / destination | `destination_service`,`destination_pod` via listener `metadata` | `source_service` from `downstream_peer_uri_san()` → parse SPIFFE `…/ns/<ns>/sa/<svc>` |

The agent already templates per-pod listeners (stat_prefix, SNI, SAN pinning);
adding a `typed_filter_metadata`/`metadata` block with the local identity is a
small, mechanical change. **Outbound needs no TLS introspection** (local id from
metadata, dest from cluster name) — only inbound parses the peer SAN. This
asymmetry drives phasing.

### Metric shape and export
The module defines, per worker, cumulative counters/histograms:
```
aether_requests_total{reporter, source_service, destination_service,
                      response_code, response_flags}
aether_request_duration_seconds{...}   # histogram
```
Counters are created via `define_counter` with the dimension values encoded in
the Envoy stat name; a bootstrap `stats_tags` rule set (the pattern already used
for `aether.cluster`) lifts them into tags, and the existing sink's
`use_tag_extracted_name` + `emit_tags_as_attributes` exports them as OTLP
attributes. Because they are ordinary Envoy counters they are **cumulative**
(Prometheus-native, no `deltatocumulative`) and flushed by the existing sink —
**zero new export path, core collector unchanged.**

### Packaging & delivery — image volume, no custom proxy image
The stock `envoyproxy/envoy:distroless-v1.38.0` is missing `libgcc_s.so.1`, which
the Rust `.so` needs for unwinding (kept, so the SDK's `catch_unwind` crash
isolation works; `-static-libgcc` does not remove it and `panic=abort` would
defeat `catch_unwind`). Instead of forking the proxy image, publish a minimal
**OCI artifact** containing `libaether_telemetry.so` **and** a matching
`libgcc_s.so.1` (copied from `envoyproxy/envoy:v1.38.0`), and mount it into the
proxy pod as a **Kubernetes image volume** (`volume.image`, beta/on-by-default in
1.34), with `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/modules` and
`LD_LIBRARY_PATH=/modules`. Stock proxy image, module released on its own cadence.
Requires a runtime with image-volume support (containerd ≥ 2.0) — verify on talos.

### Cardinality discipline
- Default dimensions: `source_service` × `destination_service` ×
  `response_code_class` (2xx/4xx/5xx) × `reporter`. Per-node series bounded by
  demand-scoping (local source services × dep dest set). Globally aggregates
  across nodes by `(source,dest)` since the sink adds no node label.
- `*_pod` and full status code: **opt-in flags** (pod multiplies by replica
  count — dangerous at the 3k-svc/10k-node target).
- **Hard cap + overflow bucket:** beyond a configured edge limit, fold into
  `source_service="overflow"` (Istio-style) to protect Prometheus.

## Phased plan

- **Phase 1 — source-reported (outbound).** No TLS introspection. Build the
  `.so` against the in-tree SDK at the Envoy tag; agent injects `source_*`
  metadata + attaches the access logger to outbound HCMs; ship the `.so` in the
  proxy image layer + the `dynamic_module_config`/search-path wiring; bootstrap
  `stats_tags` for the new dims. Validate
  `aether_requests_total{reporter="source",…,response_flags}` in Prometheus.
- **Phase 2 — destination-reported (inbound).** Add `downstream_peer_uri_san()`
  SPIFFE parse → `source_service`; inject `destination_*` metadata. Exercises
  the peer-SAN path.
- **Phase 3 — hardening.** Cardinality cap + overflow, duration histogram,
  `*_pod` flag, fuzz/soak the SAN parser, e2e drop-attribution check (kill
  last-old pod; confirm a `UF` edge appears source→dest).

## Risks & mitigations

- **ABI version pinning.** The `.so` is bound to the Envoy build's ABI hash.
  *Mitigation:* the SDK is a git dependency pinned to the proxy's Envoy tag, with
  the matching `abi.h` pinned in-tree; rebuild the `.so` in CI on every proxy
  bump; the loader rejects a mismatch loudly (fail-closed).
- **In-process crash = proxy crash** (no WASM sandbox). *Mitigation:* the SDK
  wraps module entrypoints in `catch_unwind`; fuzz the only untrusted parse
  (peer SAN) before Phase 2 ships; soak behind the supervisor.
- **Cardinality** (technology-independent, the real operational risk). *Mitigation:*
  service-granularity default, code-class not full-code, hard cap + overflow.
- **Per-worker counter aggregation.** Counters are per-worker and summed by the
  sink/Prometheus as usual; verify no double-count across workers in Phase 1.

## Spike appendix (2026-06-13, Envoy 1.38.0)

- `envoy.filters.http.dynamic_modules` **is compiled into** the published
  `envoyproxy/envoy:distroless-v1.38.0` (validate failed on "module `.so` not
  found", not "unknown type").
- ABI `source/extensions/dynamic_modules/abi/abi.h` @ v1.38.0 declares the full
  callback set: `*_get_downstream_peer_uri_san`, `*_get_upstream_cluster`,
  `*_get_response_code`, `*_get_response_flags`, `*_has_response_flag`,
  `*_get_dynamic_metadata`, `*_get_filter_state`,
  `*_config_define_counter/gauge/histogram`, `*_increment_counter`,
  `*_record_histogram_value(_vec)` — for both the HTTP-filter and access-logger
  extension variants.
- In-tree Rust SDK `sdk/rust/src/access_log.rs` @ v1.38.0 wraps them:
  `downstream_peer_uri_san() -> Vec<EnvoyBuffer>`, `upstream_cluster()`,
  `response_code() -> Option<u32>`, `has_response_flag()`,
  `get_dynamic_metadata()`, `define_counter()`, `increment_counter()`,
  `define_histogram()`, `record_histogram()`.
- Not yet done: end-to-end `.so` build + load + mTLS traffic (Phase-1 task 1);
  capability proven by ABI/SDK inspection.

## Phase-1 result (2026-06-13) — module validated end-to-end

The Bazel Rust + hermetic_cc toolchain layer this builds on is its own change,
[proposal 008](008_rust-in-bazel.md) (the stacked base PR).

`proxy/filters/telemetry/` (crate `aether_telemetry`, cdylib) built against the in-tree
SDK at the v1.38.0 tag and **loaded into stock `envoyproxy/envoy:distroless-v1.38.0`
via a volume-mounted `.so` + `libgcc_s.so.1`** (no custom image). Driving traffic
through a 200 path and a connect-refused 503 path produced, in `/stats` and via
the OTel sink:

```
envoy.dynamicmodulescustom.aether_requests_total
  {reporter=source, source_service=checkout, source_pod=checkout-abc,
   destination_service=payments, response_code=200} = 6
  {…, destination_service=broken, response_code=503} = 2
```

Confirmed:
- `counter_vec` labels export through the existing OTel sink as **OTLP attributes
  with no `stats_tags` regex** (the vec registers native Envoy tags;
  `use_tag_extracted_name`+`emit_tags_as_attributes` carry them) → Prometheus
  `envoy_dynamicmodulescustom_aether_requests_total{…}`.
- Source identity via per-instance `filter_config` JSON (agent-templated per pod).
- Destination via `get_cluster_name()` — the `XdsClusterName` CEL attribute is
  **not** populated in the HTTP-filter context (capture the cluster this way, not
  via attributes).
- Counters are cumulative/exact; **no `deltatocumulative`, core collector
  unchanged**.

Remaining for Phase 1: agent per-pod `filter_config` injection on outbound HCMs +
chart image-volume wiring + OCI-artifact build/publish (rules_img + CI), then helm
+ e2e. `response_flags` deferred to the access-logger variant (Phase 1b) — it is
the only place the final flags are reliably available.
