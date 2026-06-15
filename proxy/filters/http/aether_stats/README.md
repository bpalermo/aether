# aether_stats — stats Envoy dynamic module

[Proposal 007](../../docs/proposals/007_telemetry-edge-filter.md), Phase 1:
source-reported source↔destination request metrics, aggregated **in-proxy** into
cumulative Envoy counters and exported by the existing OpenTelemetry stat sink.
Analogous to Istio's `istio_stats` (an HTTP filter recording a tagged request
counter at the log phase).

## What it emits

A counter vector (clean OTLP attributes, no `stats_tags` regex — the
dynamic-module `counter_vec` registers native Envoy tags that the sink's
`emit_tags_as_attributes` + `use_tag_extracted_name` export directly):

```
envoy.dynamicmodulescustom.aether_requests_total{
  reporter, source_service, source_pod, destination_service,
  response_code, response_flags }
```

In Prometheus: `envoy_dynamicmodulescustom_aether_requests_total{...}`.

`response_flags` is a bounded cause label (`UF`/`UH`/`NC`/`NR`/`UO`/`UT`/`UR`,
`-` for upstream/success). The `ResponseFlags` attribute isn't exposed to
dynamic-module HTTP filters at v1.38, so the module derives it from the
`on_local_reply` details — reproducing Istio's `toShortString` vocabulary.

## How identity is resolved

The proxy is a node-level DaemonSet, so source is not a node constant. The agent
attaches this filter to every **per-pod outbound HCM** (unconditionally) and
passes the local pod's identity as the per-instance `filter_config` JSON:

```json
{"reporter":"source","source_service":"<svc>","source_pod":"<pod>",
 "mesh_domain":"aether.internal","emit_pod":false}
```

The module derives the destination per request from the routed cluster name
(`get_cluster_name()` → `<svc>.<mesh_domain>[:port]` → `<svc>`) and the response
code/flags at completion (`on_stream_complete`). Set `emit_pod:true` only when
per-pod cardinality is wanted.

## Build (Bazel — canonical)

```
bazel build //proxy/filters/http/aether_stats:aether_stats            # host

# Hermetic cross-compile for the proxy arches (clang/lld + glibc sysroot via
# toolchains_llvm; see //bazel/llvm):
bazel build //proxy/filters/http/aether_stats:aether_stats \
  --platforms=//bazel/llvm/platform:linux_amd64
bazel build //proxy/filters/http/aether_stats:aether_stats \
  --platforms=//bazel/llvm/platform:linux_arm64
# -> bazel-bin/proxy/filters/http/aether_stats/libaether_stats.so
```

### SDK dependency (nothing vendored)

The Envoy dynamic-modules Rust SDK is a **git dependency** pinned to the Envoy
tag (`Cargo.toml`), resolved by `crate_universe`. Its `bindgen` build script
reads `../../abi/abi.h`, outside the crate package (excluded by crate_universe's
sandbox), so: `abi.h` is **fetched** from the Envoy repo at the pinned tag
(`@envoy_abi_h` `http_file`, see `//bazel/envoy`), re-exported as the main-repo
alias `//proxy/filters/http/aether_stats:abi_h`, and `patches/sdk_abi_header.patch`
redirects the build script to it via `AETHER_ABI_H`. bindgen is **fully
hermetic**: it parses `abi.h` with the hermetic LLVM `libclang`
(`@llvm_toolchain_llvm`, via `LIBCLANG_PATH`) against a hermetic glibc sysroot
(chromium, `//bazel/llvm`) — no host clang or system headers; the patch derives
`--sysroot` from the `AETHER_SYSROOT` marker. On an Envoy bump: update the tag in
`Cargo.toml` + the `abi.h` URL/sha256 in `//bazel/envoy`, `CARGO_BAZEL_REPIN=1
bazel build …`, re-verify the patch. On an LLVM bump: update
`//bazel/llvm:version.bzl` + `llvm_version` in `MODULE.bazel`.

## Packaging & delivery — custom aether-proxy image

`//proxy:image` builds a **custom `aether-proxy` image** (the image targets live
at `//proxy`, not under this filter, so future proxy customizations share it):
a **`gcr.io/distroless/cc` base** + the **official Envoy release binary** at
`/usr/local/bin/envoy` (both pinned via `//bazel/envoy` `ENVOY_VERSION`) + the
module `.so` baked at `/modules/libaether_stats.so`, with
`ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/modules` in the image config — so the proxy
loads it **self-contained, with no K8s image volume and no chart env** (drops the
containerd ≥ 2.0 image-volume runtime dependency). Multi-arch; pushed by
`//proxy:image_push` → `ghcr.io/bpalermo/aether/aether-proxy`; the chart's
`proxy.image.ref` is substituted from it.

**Why distroless/cc, not `envoyproxy/envoy:distroless`:** the LLVM-built `.so`
needs `libgcc_s.so.1` at runtime (its unwinder; the old Zig build avoided it via
compiler-rt, but the single-arch LLVM dist can't cross-compile compiler-rt for
arm64). The stock Envoy distroless image is built on distroless/**base** and
lacks `libgcc_s`, so the module fails to load there. distroless/**cc** ships
`libstdc++` + `libgcc_s` + glibc (debian12, matching the Envoy release), which
both the Envoy binary and the module need. Validated:
`Dynamic module ABI version v0.1.0 matched` + `configuration OK` loading the
baked module from the image with no volume/env.

Default-on: the agent always attaches the filter, and the proxy image always
carries the module — they move together. First rollout introducing it has a
brief agent↔proxy skew window (independent DaemonSets) — see the release note.

## Status

Phase-1 validated 2026-06-13/14: builds (host + LLVM cross amd64/arm64), loads on stock
distroless Envoy, records `aether_requests_total` with `response_flags` (UF on
connect-refused), exported via the OTel sink. Agent wiring + chart image-volume
done. Remaining: helm upgrade + e2e on talos. Phase 2 = inbound/destination-
reported (peer URI SAN).
