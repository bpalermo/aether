# aether_telemetry — edge-telemetry Envoy dynamic module

Phase 1 of [proposal 007](../../docs/proposals/007_telemetry-edge-filter.md):
source-reported source↔destination request metrics, aggregated **in-proxy** into
cumulative Envoy counters and exported by the existing OpenTelemetry stat sink.

## What it emits

A counter vector (clean OTLP attributes, no `stats_tags` regex needed — the
dynamic-module `counter_vec` registers native Envoy tags that the sink's
`emit_tags_as_attributes` + `use_tag_extracted_name` export directly):

```
envoy.dynamicmodulescustom.aether_requests_total{
  reporter, source_service, source_pod, destination_service, response_code }
```

In Prometheus: `envoy_dynamicmodulescustom_aether_requests_total{...}`.

`response_flags` is intentionally **not** emitted by this HTTP-filter variant —
it is only reliably available at log time. The access-logger variant (Phase 1b)
adds it; an extra label is an additive Prometheus change.

## How identity is resolved

The proxy is a node-level DaemonSet, so source is **not** a node constant. The
agent attaches this filter to each **per-pod outbound HCM** and passes the local
pod's identity as the per-instance `filter_config` JSON:

```json
{"reporter":"source","source_service":"<svc>","source_pod":"<pod>",
 "mesh_domain":"aether.internal","emit_pod":false}
```

The module derives the destination per-request from the routed cluster name
(`get_cluster_name()` → `<svc>.<mesh_domain>[:port]` → `<svc>`) and the response
code at completion. Set `emit_pod:true` only when per-pod cardinality is wanted.

## Build (Bazel — canonical)

```
# Host build (uses the default CC toolchain):
bazel build //telemetry/edge:aether_telemetry

# Hermetic cross-compile for the proxy arches (Zig via hermetic_cc_toolchain):
bazel build //telemetry/edge:aether_telemetry \
  --platforms=@zig_sdk//libc_aware/platform:linux_amd64_gnu.2.28
bazel build //telemetry/edge:aether_telemetry \
  --platforms=@zig_sdk//libc_aware/platform:linux_arm64_gnu.2.28
# -> bazel-bin/telemetry/edge/libaether_telemetry.so
```

The Envoy SDK is **vendored** under `third_party/` with the ABI bindings
**pre-generated and checked in** (`…/src/bindings.rs`), so the build needs no
`bindgen`/libclang. The bindings are ABI-pinned to Envoy v1.38.0 — keep them in
lockstep with the proxy image (`charts/agent/values.yaml`).

### Regenerating the vendored SDK / bindings on an Envoy bump

Re-vendor `source/extensions/dynamic_modules/sdk/rust` + `…/abi/abi.h` from the
new Envoy tag, run a one-off `cargo build` (with `LIBCLANG_PATH` set) to produce
`bindings.rs` from the build script, copy it to
`third_party/envoy_dynamic_modules_sdk/src/bindings.rs`, then re-apply the two
local patches: drop `build.rs` + the `bindgen` build-dep, and change the
`include!` in `src/lib.rs` to `include!("bindings.rs")`.

## Cargo (dev convenience)

`cargo build` also works for quick local iteration (the vendored SDK is a path
dependency); Bazel is the build of record.

## Packaging & delivery (no custom proxy image)

The stock `envoyproxy/envoy:distroless-v1.38.0` lacks `libgcc_s.so.1`, which the
Rust `.so` needs for unwinding (kept for the SDK's `catch_unwind` crash
isolation). Rather than fork the proxy image, publish a minimal OCI artifact
containing **both** `libaether_telemetry.so` and a matching `libgcc_s.so.1`
(copied from `envoyproxy/envoy:v1.38.0`), and mount it into the proxy pod as a
**Kubernetes image volume** (`volume.image`, beta/on-by-default in 1.34):

```yaml
# proxy pod spec (chart)
volumes:
  - name: aether-telemetry
    image: { reference: <registry>/aether-telemetry:<ver>, pullPolicy: IfNotPresent }
containers:
  - name: aether-proxy
    env:
      - { name: ENVOY_DYNAMIC_MODULES_SEARCH_PATH, value: /modules }
      - { name: LD_LIBRARY_PATH, value: /modules }
    volumeMounts:
      - { name: aether-telemetry, mountPath: /modules, readOnly: true }
```

Requires a container runtime with image-volume support (containerd ≥ 2.0 /
recent CRI-O) — verify on talos.

## Status

Phase-1 task 1 validated 2026-06-13: builds, loads on **stock distroless** Envoy
1.38.0 via volume-mounted `.so`+libgcc, increments the edge counter, and exports
through the OTel sink as OTLP attributes. Remaining: agent per-pod
`filter_config` injection + chart image-volume wiring + OCI-artifact build/publish
(rules_img + CI), then helm + e2e. Phase 2 = inbound/destination-reported via the
access-logger variant (peer URI SAN + response_flags).
