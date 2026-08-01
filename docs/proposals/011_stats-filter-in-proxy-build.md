# Proposal: Port the aether_stats filter into the proxy build

**Status:** Design — 2026-06-15
**Author:** Bruno Palermo
**Follows:** proposal 010 (custom proxy workspace), proposal 007 (telemetry filter)

## Problem statement

The `aether_stats` Envoy **dynamic module** (proposal 007 — source→destination
request metrics) is **parked** under `proxy/filters/http/aether_stats/` (source
only, no `BUILD.bazel`). It was unwired when the proxy became a separate WORKSPACE
workspace (proposal 010) so we could first get a vanilla custom Envoy building.

This re-wires it into the proxy build. Since we already build a **custom Envoy
from source**, the recommended form is a **native (statically-linked) module** —
the filter compiled into the Envoy binary, not a runtime-loaded `.so`. The
dynamic-module `.so` is kept as a documented alternative.

**Both use the exact same `src/lib.rs` and the same agent config** — Envoy's
loader auto-selects static vs dynamic by probing for the symbols (below).

## Key simplification: use the in-tree Envoy SDK

In the old aether **bzlmod** workspace, building the module was painful: the
`envoy-proxy-dynamic-modules-rust-sdk` was a **git dependency** resolved by
`crate_universe`, whose `bindgen` build script needed `abi.h` — which we **fetched**
separately (`@envoy_abi_h`) and fed in via a patch + a hermetic-LLVM `libclang` +
glibc sysroot dance (the whole `crate.annotation` block in `MODULE.bazel`).

In the proxy workspace **`@envoy` is present**, so the SDK is **in-tree**:

```
@envoy//source/extensions/dynamic_modules/sdk/rust:envoy_proxy_dynamic_modules_rust_sdk
```

(verified to exist at our pinned **v1.38.0** — a `rust_library`). Depending on it
directly means **no git dep, no `crate_universe` for the SDK, no fetched `abi.h`,
no bindgen/sysroot plumbing** — the SDK's own build script runs bindgen against
the in-tree `abi.h` with Envoy's toolchain. This is exactly the model
`bpalermo/istio-proxy` uses for its `rust_module`, and the 010 "the bindgen
gymnastics disappear" prediction, realized.

It also makes the SDK **inherently version-matched** to the proxy's Envoy (same
`@envoy` source) — retiring the `Cargo.toml` git-tag lockstep (010 B1).

## Design

Mirror istio-proxy's `filters/http/rust_module` exactly.

### How native static modules work

Envoy's dynamic-modules loader auto-detects static vs dynamic by **name**
(`source/extensions/dynamic_modules/dynamic_modules.cc`):
`newDynamicModuleByName("aether_stats")` first probes
`dlsym(RTLD_DEFAULT, "aether_stats_envoy_dynamic_module_on_program_init")`. If
those **prefixed** symbols are statically linked into the Envoy binary, it builds
a `newStaticModule` — a native, compiled-in module. Otherwise it falls back to
loading `libaether_stats.so` from `ENVOY_DYNAMIC_MODULES_SEARCH_PATH`.

So the **agent config (`DynamicModuleConfig{name:"aether_stats"}`) is identical**
for both, and the loader picks whichever is present. v1.38.0 ships a first-class
build macro for the static path (`envoy_dynamic_module_prefix_symbols`, used by
Envoy's own `test_data/rust/test_data.bzl`).

### `proxy/filters/http/aether_stats/BUILD.bazel` (new) — native

```starlark
load("@rules_rust//rust:defs.bzl", "rust_static_library", "rust_test")
load("@envoy//source/extensions/dynamic_modules:dynamic_modules.bzl", "envoy_dynamic_module_prefix_symbols")

rust_static_library(
    name = "aether_stats_lib",
    srcs = glob(["src/**/*.rs"]),
    edition = "2021",
    rustc_flags = ["-C", "link-args=-Wl,-undefined,dynamic_lookup"],
    deps = [
        "@crate_index//:serde",
        "@crate_index//:serde_json",
        "@envoy//source/extensions/dynamic_modules/sdk/rust:envoy_proxy_dynamic_modules_rust_sdk",
    ],
)

# Renames the ABI symbols to the aether_stats_<symbol> prefix the static loader
# resolves via dlsym(RTLD_DEFAULT).
envoy_dynamic_module_prefix_symbols(
    name = "aether_stats",
    archive = ":aether_stats_lib",
    module_name = "aether_stats",
    visibility = ["//visibility:public"],
)

rust_test(
    name = "aether_stats_test",
    size = "small",
    srcs = glob(["src/**/*.rs"]),
    crate_root = "src/lib.rs",
    edition = "2021",
    deps = [
        "@crate_index//:serde",
        "@crate_index//:serde_json",
        "@envoy//source/extensions/dynamic_modules/sdk/rust:envoy_proxy_dynamic_modules_rust_sdk",
    ],
)
```

Then link it into the custom Envoy in `proxy/BUILD.bazel`:

```starlark
AETHER_EXTENSIONS = ["//filters/http/aether_stats:aether_stats"]

envoy_cc_binary(
    name = "envoy",
    repository = "@envoy",
    deps = AETHER_EXTENSIONS + ["@envoy//source/exe:envoy_main_entry_lib"],
)
```

The prefixed symbols must remain in the binary's dynamic symbol table for
`dlsym(RTLD_DEFAULT)` — `envoy_cc_binary` exports dynamic symbols (Envoy already
relies on this for dynamic modules); verify on the first build (else add
`-rdynamic`/`alwayslink`).

`src/lib.rs` is unchanged — it already `use envoy_proxy_dynamic_modules_rust_sdk::*`,
`use serde::Deserialize`, calls `declare_init_functions!`, and its tests use the
SDK's `MockEnvoyHttpFilter`.

### Alternative: dynamic `.so`

Swap `rust_static_library` + `envoy_dynamic_module_prefix_symbols` for a
`rust_shared_library` named `aether_stats` (same deps/flags), bake it into the
image at `/modules` via `pkg_tar`, and set
`ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/modules` in the image. Pick this only if
decoupling the module from the Envoy binary (rebuild just the `.so`) is wanted —
but we rebuild Envoy anyway, so native is preferred.

### Crate graph for serde — `proxy/bazel/rust/`

`rules_rust` + `crate_universe` are already available (Envoy uses them, e.g.
`@envoy_rust_crate_index`). Add a small `crate_index` for the two crates.io deps:

```starlark
# proxy/bazel/rust/crates_repository.bzl
load("@rules_rust//crate_universe:defs.bzl", "crate", _crates_repository = "crates_repository")

def crates_repository():
    _crates_repository(
        name = "crate_index",
        cargo_lockfile = "//bazel/rust:Cargo.lock",
        lockfile = "//bazel/rust:Cargo.Bazel.lock",
        packages = {
            "serde": crate.spec(version = "1.0", features = ["derive"]),
            "serde_json": crate.spec(version = "1.0"),
        },
    )
```

`proxy/WORKSPACE` (after the Envoy chain) re-adds:

```starlark
load("//bazel/rust:crates_repository.bzl", "crates_repository")
crates_repository()
load("@crate_index//:defs.bzl", "crate_repositories")
crate_repositories()
```

### Image — nothing to bake (native)

With the native module the filter is **inside `//:envoy`**, so the image is
unchanged: no `/modules` layer, no `ENVOY_DYNAMIC_MODULES_SEARCH_PATH`, no
`pkg_tar`. The module cross-compiles for each arch as part of the existing
multi-arch `oci_image_index` (it's linked into the binary the transition builds).
`image.yaml` stays as-is (the structure test already checks `/usr/local/bin/envoy`).

*(Dynamic alternative only: `pkg_tar` the `.so` to `/modules`, set
`ENVOY_DYNAMIC_MODULES_SEARCH_PATH`, and assert `/modules/libaether_stats.so` in
`image.yaml`.)*

### Remove the now-vestigial bzlmod-era files

- `proxy/filters/http/aether_stats/Cargo.toml`, `Cargo.lock` — the Bazel build
  uses the in-tree SDK + `@crate_index`; like istio's `rust_module`, the filter
  needs no Cargo manifest. (Keep one only if cargo-based local dev is wanted.)
- `proxy/filters/http/aether_stats/patches/` (`sdk_abi_header.patch`) — obsolete;
  the in-tree SDK reads `abi.h` itself.

## What does NOT change

- **`src/lib.rs`** — same SDK API and tests.
- **Agent wiring** — the agent already attaches the filter to per-pod outbound
  HCMs and passes the per-instance `filter_config` (proposal 007, shipped), with
  `DynamicModuleConfig{name:"aether_stats"}`. That config is **identical** for
  native and dynamic — Envoy auto-detects. The proxy build just makes the module
  present (linked in).
- **Chart** — references the proxy image, which now carries the module in the
  binary; no chart env / image volume.

## Risks & validation

- **arm64 cross-compile.** Linked into Envoy, the module shares the (currently
  **broken**) arm64 Envoy build — the LuaJIT `foreign_cc` `ld`/`lld` failure from
  #189's `proxy-pr` must be fixed first (v1.38's `envoy.bazelrc` lacks an
  `arm64-clang` config). Once Envoy builds arm64, the module links in for free.
- **Dynamic symbol visibility.** The prefixed symbols must be in the binary's
  dynamic symbol table for `dlsym(RTLD_DEFAULT)`. Envoy already exports dynamic
  symbols for dynamic modules; verify on the first build (else `-rdynamic`).
- **`MockEnvoyHttpFilter` for `rust_test`.** The test links the in-tree SDK; confirm
  the mock is exposed (it was via the git-dep SDK before; same source).
- **Build/iteration cost.** Each filter change relinks Envoy. Marginal (small
  crate); use the dynamic `.so` if fast standalone iteration is needed.

## Rollout

Single PR, additive: filter `BUILD.bazel` (`rust_static_library` +
`envoy_dynamic_module_prefix_symbols`) + `bazel/rust/` crate graph + WORKSPACE
crate setup + `AETHER_EXTENSIONS` in `proxy/BUILD.bazel`; delete the vestigial
Cargo/patches. Gated on the arm64 Envoy fix. `proxy-pr` validates the module
builds (both arches, linked into `//:image_index`) and
the image structure test sees `/modules/libaether_stats.so`.
```
