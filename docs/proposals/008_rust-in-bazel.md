# Proposal: Rust in the Bazel build (rules_rust + hermetic_cc_toolchain)

**Status:** Draft — toolchain validated by a smoke target (host + Zig cross, 2026-06-13)
**Author:** Bruno Palermo
**Date:** 2026-06-13

## Problem Statement

Aether is Go + Bazel. Proposal 007 (source↔destination telemetry) needs an Envoy
**dynamic module** — a native shared library loaded by the stock proxy at runtime.
The performant, no-custom-proxy-build option is a **Rust dynamic module** (see 007
for the WASM/Rust/C++ analysis). That requires first-class Rust support in the
Bazel build: a Rust toolchain, a way to link a `cdylib`, and — since the proxy
ships multi-arch (amd64/arm64) — hermetic cross-compilation.

This proposal adds that toolchain layer on its own, decoupled from any module
implementation, so the build-system change can be reviewed and validated in
isolation. The first consumer (the `aether_telemetry` module) lands in a
follow-up PR stacked on this one.

## Goals

1. Build Rust targets in Bazel (`rules_rust`), with a hermetic, pinned `rustc`.
2. Link `cdylib`s and **cross-compile for linux amd64 + arm64** hermetically,
   without requiring host clang.
3. **Do not disturb the existing Go/protobuf build** — the default C++ toolchain
   used by cgo/protobuf must stay exactly as-is.
4. Keep `bazel test //...` green and exercise the Rust toolchain in CI.

## Design

### rules_rust
`rules_rust` 0.70.0. The `rust` module extension downloads a pinned `rustc`
(edition 2021, version 1.83.0) — hermetic, independent of any host Rust. No
`crate_universe` is added here; external-crate management arrives with the first
consumer that needs it (proposal 007).

### hermetic_cc_toolchain (the C linker + cross-compile)
`hermetic_cc_toolchain` 4.1.0 provides a Zig-based C/C++ toolchain
(`@zig_sdk`). It is the Bazel-native equivalent of the `cargo-zigbuild` approach
the upstream dynamic-module examples use for multi-arch.

**Critically, it is registered libc-aware:**

```starlark
register_toolchains("@zig_sdk//libc_aware/toolchain/...")
```

libc-aware toolchains only resolve for a platform that declares a libc-version
constraint (`@zig_sdk//libc_aware/platform:linux_{amd64,arm64}_gnu.2.28`). A
normal `bazel build //...` / `bazel test //...` therefore continues to use the
repo's existing default C++ toolchain — the Go/protobuf builds are untouched.
Hermetic cross-compilation is opt-in per invocation, e.g. for the first
consumer (proposal 007):

```
bazel build //telemetry/edge:aether_telemetry \
  --platforms=@zig_sdk//libc_aware/platform:linux_arm64_gnu.2.28
```

This is what the module's release-artifact build (proposal 007) uses to produce
both-arch `.so`s.

This PR adds only the toolchain layer — no Rust target, since the toolchain is
exercised by its first consumer (proposal 007, stacked) and the validation
below. A standalone smoke target was prototyped and dropped once the real
module proved the toolchain.

## Validation (2026-06-13)

- Module `//telemetry/edge:aether_telemetry` (proposal 007, stacked) builds
  green on the host toolchain and under
  `--platforms=@zig_sdk//libc_aware/platform:linux_amd64_gnu.2.28` and
  `…linux_arm64_gnu.2.28` (arm64 output confirmed `ELF … ARM aarch64`), and
  loads into stock `envoyproxy/envoy:distroless-v1.38.0`.
- Existing Go/protobuf builds unaffected (default C++ toolchain unchanged;
  libc-aware Zig toolchains do not resolve for default platforms).

## Follow-ups

- Proposal 007 stacks on this: vendored Envoy SDK + `crate_universe` (serde,
  serde_json, mockall) + the `aether_telemetry` module + the OCI-artifact /
  image-volume delivery.
