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
# --config=release bakes the optimized, stripped Envoy (plain builds are
# fastbuild/dev). The Makefile `load-proxy-image` target does this for you.
bazel build --config=release //:image
bazel run --config=release //:load   # ghcr.io/bpalermo/aether/aether-proxy:latest

# Image smoke test (container-structure-test).
bazel test //:image_test
```

The image is `distroless/cc` base + the custom `//:envoy` binary at
`/usr/local/bin/envoy`. Plain `bazel build //:image` produces a **fastbuild**
(unoptimized, unstripped) binary — always pass `--config=release` (CI and the
Makefile targets do) to bake the production binary.

> **`aether_stats` is a compiled-in C++ extension** (proposal 012), built into
> `//:envoy` via `AETHER_EXTENSIONS` in `BUILD.bazel`. It records
> source→destination request metrics from `StreamInfo` at stream completion. The
> earlier Rust dynamic-module approach was dropped — no Rust toolchain or
> dynamic-module wiring is needed.

## Layout

| Path | Purpose |
|---|---|
| `WORKSPACE` | `@envoy` + Envoy's macro chain, rules_oci |
| `.bazelversion` | `7.7.1` (independent of the root repo) |
| `.bazelrc` → `envoy.bazelrc` | build config; selects Envoy's hermetic clang on Linux |
| `bazel/envoy/repository.bzl` | pinned Envoy **source** (`ENVOY_SHA` = v1.38.0) |
| `bazel/extension_config/` | the compiled-in Envoy extension set |
| `bazel/oci/images.bzl` | `distroless/cc` base pull |
| `BUILD.bazel` | custom `envoy_cc_binary` + `oci_image`/`oci_push`/`oci_load` |
| `source/extensions/filters/http/aether_stats/` | native C++ `aether_stats` filter (compiled into `//:envoy`) |
| `patches/` | source patches applied to `@envoy` |

## Customizing

- **Compiled extensions:** add your `envoy_cc_library` config target to
  `AETHER_EXTENSIONS` in `BUILD.bazel`.
- **Source patches:** drop a `*.patch` in `patches/` and list it in
  `bazel/envoy/repository.bzl` (`patches = [...]`, `patch_args = ["-p1"]`).

## Envoy version bumps

`bazel/envoy/repository.bzl` is the single source of truth (`ENVOY_SHA` /
`ENVOY_SHA256`). On a bump, re-check `.bazelversion` / `envoy.bazelrc` against the
new Envoy release. The `aether_stats` C++ extension builds against this same Envoy
tree, so there is no separate SDK version to keep in sync.

## Status

**Custom Envoy + `aether_stats` C++ extension building on CI** (proposals 010 /
012). The Envoy source compile requires a Bazel 7.7.1 build host with adequate
RAM and runs on BuildBuddy RBE (amd64) / native arm64 runners; the
`aether_stats` filter and its `envoy_cc_test` build and pass there.
