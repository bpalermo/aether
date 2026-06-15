# aether-proxy

A **custom Envoy** build for the Aether data plane, structured after
[`bpalermo/istio-proxy`](https://github.com/bpalermo/istio-proxy).

> This is a **separate Bazel workspace** from the root `aether` repo. The root
> `//.bazelignore` lists `proxy`, so `bazel build //...` / Gazelle in the repo
> root never descend here. It pins its **own** Bazel version
> (`.bazelversion = 7.7.1`, required by Envoy 1.38's WORKSPACE build) and uses
> Envoy's pre-bzlmod dependency macro chain. The two workspaces share only the
> git repo. See [`docs/proposals/010`](../docs/proposals/010_custom-proxy-workspace.md).

## Build

All commands run **from inside `proxy/`** (so the right Bazel version is used):

```bash
cd proxy

# Build the custom Envoy binary (multi-hour C++ build; use a warm cache / CI).
bazel build //:envoy

# Build + load the custom aether-proxy image into the local Docker daemon.
bazel build //:image
bazel run //:load            # ghcr.io/bpalermo/aether/aether-proxy:latest

# Image smoke test (container-structure-test).
bazel test //:image_test
```

The image is `distroless/cc` base + the custom `//:envoy` binary at
`/usr/local/bin/envoy`.

> **Filter customization is currently unwired.** The first goal is a vanilla
> custom Envoy that builds. The `aether_stats` dynamic module and its
> crate_universe/bindgen wiring are not in the build yet — its source is parked
> under `filters/http/aether_stats/` and re-wired in the proposal-010 follow-up.

## Layout

| Path | Purpose |
|---|---|
| `WORKSPACE` | `@envoy` + Envoy's macro chain, rules_oci, rust crates |
| `.bazelversion` | `7.7.1` (independent of the root repo) |
| `.bazelrc` → `envoy.bazelrc` | build config; selects Envoy's hermetic clang on Linux |
| `bazel/envoy/repository.bzl` | pinned Envoy **source** (`ENVOY_SHA` = v1.38.0) |
| `bazel/extension_config/` | the compiled-in Envoy extension set |
| `bazel/oci/images.bzl` | `distroless/cc` base pull |
| `BUILD.bazel` | custom `envoy_cc_binary` + `oci_image`/`oci_push`/`oci_load` |
| `filters/http/aether_stats/` | parked `aether_stats` module source (not built yet) |
| `patches/` | source patches applied to `@envoy` |

## Customizing

- **Compiled extensions:** add your `envoy_cc_library` config target to
  `AETHER_EXTENSIONS` in `BUILD.bazel`.
- **Source patches:** drop a `*.patch` in `patches/` and list it in
  `bazel/envoy/repository.bzl` (`patches = [...]`, `patch_args = ["-p1"]`).

## Envoy version bumps

`bazel/envoy/repository.bzl` is the single source of truth (`ENVOY_SHA` /
`ENVOY_SHA256`). On a bump, re-check `.bazelversion` / `envoy.bazelrc` against the
new Envoy release. (When the `aether_stats` filter is re-wired, also bump the SDK
tag in `filters/http/aether_stats/Cargo.toml` to match.)

## Status

**Scaffold (proposal 010, PR 1) — not yet build-validated.** The Envoy source
compile requires a Bazel 7.7.1 build host with adequate RAM and is gated in CI,
not validated in-editor. Filter customization is intentionally unwired for now
(see above) so the first milestone is a clean vanilla custom Envoy build.
