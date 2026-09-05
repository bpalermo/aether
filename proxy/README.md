# aether-proxy

A **custom Envoy** build for the Aether data plane, structured after
[`envoyproxy/examples/filter-cc`](https://github.com/envoyproxy/examples/tree/main/filter-cc)
— the upstream downstream-build template published alongside Envoy's WORKSPACE
removal (envoy#42890 / envoy#47217).

> This is a **separate Bazel module** from the root `aether` repo. The root
> `//.bazelignore` lists `proxy`, so `bazel build //...` / Gazelle in the repo
> root never descend here. It pins its **own** Bazel version
> (`.bazelversion = 8.7.0`), its **own** module graph (`MODULE.bazel`), and its
> **own** registries (`.bazelrc`). The two workspaces share only the git repo.
> See [`docs/proposals/010`](../docs/proposals/010_custom-proxy-workspace.md).

## Build

All commands run **from inside `proxy/`** (so the right Bazel version and module
graph are used):

```bash
cd proxy

# Build the custom Envoy binary (multi-hour C++ build; use a warm cache / CI).
bazel build //:envoy

# Build + load the custom aether-proxy image into the local Docker daemon.
# --config=release bakes the optimized Envoy (plain builds are fastbuild/dev).
# The Makefile `load-proxy-image` target does this for you.
bazel build --config=release //:image
bazel run --config=release //:load   # ghcr.io/bpalermo/aether/aether-proxy:latest

# Image smoke test (container-structure-test).
bazel test //:image_test
```

> **You almost certainly cannot build this locally.** A cold Envoy build fetches
> tens of GB of module archives (BoringSSL, V8, ICU, gRPC, the LLVM 22.1.8
> distributions, …) and needs hundreds of GB of output base even with the
> compile offloaded to RBE — analysis still fetches everything. What *is* cheap
> locally, and worth running before pushing, is module resolution:
>
> ```bash
> # No unresolved modules; envoy/envoy_api at the pinned snapshot.
> bazel mod graph --depth=1
>
> # The single assertion that override_repo took: this MUST print our
> # local_repository at bazel/build_config, not Envoy's default_envoy_build_config.
> bazel mod show_repo --base_module=envoy @envoy_build_config
>
> # Selected versions match the registry pin, with no MVS surprises.
> bazel mod explain @quiche @protobuf @abseil-cpp
> ```
>
> Everything from `bazel build --nobuild //:envoy` onward is validated by
> `.github/workflows/proxy.yml`, which builds both arches on BuildBuddy RBE.
>
> `MODULE.bazel.lock` is committed as a dev convenience. CI runs with the default
> `--lockfile_mode=update`, **not** `error`: the `envoy_toolchains_extension`
> (`arch_alias` → `ctx.os.arch`) and `toolchains_llvm`'s `llvm` extension are
> host-arch dependent, and the two CI legs drive from different arches.

The image is `distroless/cc` base + the custom `//:envoy` binary at
`/usr/local/bin/envoy`. Plain `bazel build //:image` produces a **fastbuild**
(unoptimized) binary — always pass `--config=release` (CI and the Makefile
targets do) to bake the production binary.

The released binary is **not** fully stripped, on purpose. `--strip=always`
removes DWARF but keeps `.symtab` (~22 MB, ~944k symbols), which is what lets
Pyroscope put names on the proxy fleet's native frames — the profiler itself
symbolizes nothing native (aether #651). `//integration:symtab_test` guards it,
and `//integration:build_id_test` guards the content-derived GNU build-ID the
symbol upload is keyed by (#653).

> **`aether_stats` is a compiled-in C++ extension** (proposal 012), built into
> `//:envoy` via `AETHER_EXTENSIONS` in `BUILD.bazel`. It records
> source→destination request metrics from `StreamInfo` at stream completion. The
> earlier Rust dynamic-module approach was dropped — no Rust toolchain or
> dynamic-module wiring is needed.

## Layout

| Path | Purpose |
|---|---|
| `MODULE.bazel` | module graph; **pins `envoy` / `envoy_api`** (see "Envoy version bumps") |
| `.bazelrc` | build config; **pins the envoy bazel-registry commit**; clang/RBE/release configs |
| `.bazelversion` | `8.7.0` (independent of the root repo) |
| `bazel/build_config/` | the compiled-in Envoy extension set, as a tiny local module (`envoy_build_config`) |
| `bazel/platforms/BUILD.bazel` | BuildBuddy RBE exec platforms (amd64 / arm64), derived from Envoy's |
| `bazel/get_workspace_status` | `--workspace_status_command` stamping script |
| `BUILD.bazel` | custom `envoy_cc_binary` + `oci_image`/`oci_push`/`oci_load` + `image_test` |
| `integration/` | `build_id_test`, `symtab_test`, container-structure-test config |
| `source/extensions/filters/http/aether_stats/` | native C++ `aether_stats` filter (compiled into `//:envoy`) |

## Customizing

- **Compiled extensions:** add your `envoy_cc_library` config target to
  `AETHER_EXTENSIONS` in `BUILD.bazel`. It is a **dep of the binary**, not an
  entry in `bazel/build_config/extensions_build_config.bzl` — that dict is only
  for extensions that live in `@envoy`.
- **Dropping upstream extensions:** add a substring to `_DROPPED` in
  `bazel/build_config/extensions_build_config.bzl`. It filters Envoy's own
  default dict, so an Envoy bump picks up new upstream extensions for free.
  Today we drop `wasm` and `dynamic_module`.
- **Source patches:** there are none, and adding one is not free any more —
  `@envoy` is a registry module, so a patch needs a
  `single_version_override(patches = [...])` in `MODULE.bazel`. Prefer
  upstreaming.

## Envoy version bumps

**The module version and the registry commit are ONE pin and must move
together.** The envoy bazel-registry
([`envoyproxy/bazel-registry`](https://github.com/envoyproxy/bazel-registry))
publishes a rolling `main` dev snapshot and keeps **exactly one version per
module** — it *deletes* the previous version directory on every bump. An
unpinned (or mismatched) registry commit therefore does not merely drift; it
makes the module unresolvable the next time upstream publishes.

Worse, registry HEAD is frequently **not self-consistent**: sibling modules get
bumped before `modules/envoy/<version>/MODULE.bazel` is updated to match, so at
HEAD the `envoy` module can reference `.envoy`-suffixed versions that no longer
exist. Envoy's own `.bazelrc` pins a commit that predates the publication of its
own snapshot, so it cannot be copied either.

The bump recipe is therefore a **resolvability loop** — find the newest registry
commit at which the whole `.envoy`-suffixed closure resolves:

```bash
git clone https://github.com/envoyproxy/bazel-registry.git /tmp/br

# For each recent commit, newest first:
#   1. envoy_ver=$(git -C /tmp/br ls-tree -d --name-only <commit> modules/envoy/)
#   2. BFS `bazel_dep(name=..., version=...)` from
#      modules/envoy/$envoy_ver/MODULE.bazel and modules/envoy_api/...
#   3. every dep whose version ends in `.envoy` must have a
#      modules/<name>/<version>/MODULE.bazel in that tree
# Take the newest commit with zero misses.
```

Then, in one commit:

1. `MODULE.bazel`: set `envoy` and `envoy_api` to that snapshot version.
2. `.bazelrc`: set `--registry=https://raw.githubusercontent.com/envoyproxy/bazel-registry/<that commit>`.
3. Update every other `.envoy`-suffixed `bazel_dep` in `MODULE.bazel`
   (`quiche`, `googleurl`, `proxy-wasm-cpp-host`, `rules_rust`,
   `toolchains_llvm`, `protobuf`) to the versions that commit carries — they
   must match what the `envoy` module requests or MVS will fail.
4. Re-diff `.bazelrc` against the two upstream sources named at the top of that
   file (the filter-cc template and Envoy's own `.bazelrc` at the new pin).
5. Run the local `bazel mod` checks above, then push and let CI build both
   arches.

When a stable release publishes a `1.40.0.envoy` (etc.) module, move to it and
re-pin the root `MODULE.bazel`'s `@envoy_binary_linux_*` (used by
`//test/envoy_validate` for `envoy --mode validate`) to the matching release at
the same time.

The `aether_stats` C++ extension builds against this same Envoy tree, so there
is no separate SDK version to keep in sync.

## Status

**Custom Envoy + `aether_stats` C++ extension building on CI** (proposals 010 /
012), on bzlmod against an Envoy `main` dev snapshot (#697). The Envoy compile
runs on BuildBuddy RBE — amd64 driven from an x64 runner, arm64 driven from a
native `ubuntu-24.04-arm` runner (`@clang_platform` and `@llvm_toolchain` derive
their exec constraints from the *driver* host's arch, so an x64 driver cannot
drive the arm64 pool).
