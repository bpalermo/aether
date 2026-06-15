# Proposal: Custom Envoy proxy as a separate sibling Bazel workspace

**Status:** Design — supersedes proposal 009's "reject Option A" for the
*separate-workspace* framing
**Author:** Bruno Palermo
**Date:** 2026-06-15

## Problem statement

Aether runs **stock** upstream Envoy. We now want the ability to **customize the
proxy build** — apply source **patches** and/or compile in our own **extensions**
(C++ or Rust HTTP filters) — rather than only loading runtime dynamic modules.

Proposal 009 evaluated "bring the Envoy build into the workspace" *literally —
inside aether's bzlmod workspace* — and rejected it: Envoy ≤1.38 isn't a bzlmod
citizen (`envoy`/`envoy_build_config`/`envoy_mobile` not on the BCR; pins
unpublished `-dev` internal modules), and aether is on Bazel 9.x while an Envoy
1.38 source build needs Bazel 7.7.1. Merging the two build systems is the part
that fails.

**This proposal changes the framing, not the goal.** We do *not* integrate
Envoy's build system into aether. We add a **second, fully independent Bazel
workspace** at `proxy/`, modeled on [`bpalermo/istio-proxy`], that owns the
custom Envoy build end to end. The two workspaces only **share the git repo**.
Aether's build ignores `proxy/` via `.bazelignore`; `proxy/` builds with its own
`WORKSPACE`, its own `.bazelversion`, and Envoy's own dependency macros — the
canonical pre-bzlmod custom-Envoy pattern that *does* work.

[`bpalermo/istio-proxy`]: https://github.com/bpalermo/istio-proxy

## Goals

1. A custom Envoy binary built in-repo from pinned Envoy **source**, with hooks
   for **source patches** and **compiled extensions** (the istio-proxy model).
2. **Zero impact on aether's build.** `bazel build //...` / `make test` / Gazelle
   in the repo root never descend into `proxy/`; aether's `MODULE.bazel` and
   toolchains are untouched by the Envoy build graph.
3. The custom proxy **image** (`ghcr.io/bpalermo/aether/aether-proxy`) is produced
   by the `proxy/` workspace using **`rules_oci` (`oci_image`/`oci_load`/`oci_push`)**
   — the istio-proxy packaging model — replacing the current `rules_img` build.
4. **One Envoy version source of truth** in `proxy/`. Building Envoy from a pinned
   source tree makes the binary, the dynamic-modules `abi.h`, and the filter SDK
   *inherently* the same version — no more cross-pin drift (009 B1).

## Design

### Two independent workspaces, one repo

```
aether/                      ← bzlmod workspace, Bazel 9.x (unchanged)
  MODULE.bazel
  .bazelignore               ← add: proxy
  agent/ registrar/ cni/ ...
  proxy/                     ← SEPARATE WORKSPACE workspace, Bazel 7.7.1
    WORKSPACE
    .bazelversion            ← 7.7.1 (Envoy 1.38 toolchain), independent of root
    ...
```

The root `.bazelignore` entry is **load-bearing, not cosmetic**: a nested
`WORKSPACE` is *not* automatically invisible to the outer workspace. Without
`proxy` in `.bazelignore`, root `bazel build //...` / Gazelle still descend into
`proxy/`, hit the new `proxy/BUILD.bazel`'s `load("@envoy//…")`, and fail
(`@envoy` doesn't exist in aether's module graph). So the `.bazelignore` entry and
the new `proxy/` package files **must land in the same commit** — there is no
intermediate "scaffold only, aether untouched" state that stays green. Once
ignored, `//...` won't descend and `proxy/` is built explicitly with
`cd proxy && bazel build //…` (its own Bazel version from `proxy/.bazelversion`).
This is the standard way a monorepo hosts two unrelated Bazel workspaces.

**Why a separate workspace cleanly solves what 009 couldn't:**

| 009 blocker (merge into aether) | Resolved by separate workspace |
|---|---|
| Envoy not on BCR / bzlmod | `proxy/` uses **legacy WORKSPACE** + Envoy's `repositories.bzl` macro chain (sidesteps BCR) |
| aether on Bazel 9.x, Envoy needs 7.7.1 | `proxy/.bazelversion = 7.7.1`, root stays 9.x |
| Drags Envoy's C++ toolchain into aether | Envoy's toolchain lives only in `proxy/`; aether's LLVM/Go toolchains untouched |
| Multi-hour C++ build in every `bazel test //...` | `proxy/` builds only when explicitly invoked / on its own CI |

### Target `proxy/` structure (mirrors istio-proxy)

```
proxy/
  WORKSPACE                       # http_archive @envoy + Envoy macro chain:
                                  #   envoy_api_binding → envoy_api_dependencies
                                  #   → envoy_dependencies → *_extra
                                  #   → envoy_python_dependencies → dependency_imports
                                  #   + our rust crates_repository
  .bazelversion                   # 7.7.1
  .bazelrc                        # imports envoy.bazelrc + aether.bazelrc
  envoy.bazelrc                   # vendored from @envoy (clang/compile/sanitizer flags)
  aether.bazelrc                  # our overrides (the istio.bazelrc analog)
  BUILD.bazel                     # custom envoy binary alias + packaging + image
  bazel/
    envoy_version.bzl             # ENVOY_VERSION — single source of truth (moved here)
    repositories.bzl              # http_archive(@envoy, sha == ENVOY_VERSION, patches=…)
    extension_config/
      BUILD                       # curated extension set
      extensions_build_config.bzl # which Envoy extensions to compile in
    rust/
      crates_repository.bzl       # crate_universe for the Rust filter SDK
  source/exe/
    BUILD                         # envoy_cc_binary that registers our extensions
    main.cc                       # custom Envoy main (registers static extensions)
  filters/http/
    aether_stats/                 # the existing Rust filter, relocated (see below)
      BUILD.bazel Cargo.toml Cargo.lock src/lib.rs patches/
  patches/
    *.patch                       # patches applied to the @envoy source archive
  integration/                    # envoy --mode validate + filter-load tests (009 B2)
  testdata/
  tools/
  .github/workflows/proxy.yml     # proxy CI (Bazel 7.7.1; separate from aether CI)
  README.md
```

### What moves out of aether into `proxy/`

Everything Envoy/Rust-module-specific that currently lives in aether's bzlmod
workspace migrates into `proxy/`; aether keeps only what its Go/cgo build needs.

| Today (aether root) | After |
|---|---|
| `//bazel/envoy` (`ENVOY_VERSION`, `abi.h` fetch, Envoy **binary** fetch) | `proxy/bazel/envoy_version.bzl` + `repositories.bzl`. Aether stops fetching Envoy entirely. The binary is no longer downloaded — it's **built** from `@envoy//source/exe:envoy`. |
| `//proxy:image` rules_img repackage (stock binary + module `.so`) | `proxy//:image` — now bakes **our** `source/exe:envoy`. |
| `MODULE.bazel`: `rules_rust`, `crate` extension, `envoy` extension, proxy sysroot/bindgen `crate.annotation` | `proxy/WORKSPACE` + `proxy/bazel/rust/crates_repository.bzl`. The hermetic-bindgen sysroot gymnastics **disappear** — Envoy's own build already provides clang + the SDK. |
| `//proxy/filters/http/aether_stats` | `proxy/filters/http/aether_stats` (same files; built against the custom Envoy's SDK). |

**Audit before deleting from aether (shared, must stay):** `//bazel/llvm` is the
repo's single C/C++ toolchain — used by the Go **cgo/protobuf** build, not just
the proxy module. Keep the toolchain registration and any sysroot wiring the
cross-arch Go image build depends on; move only the proxy-module-only pieces
(`abi_h` alias, the SDK `crate.annotation`, the `crates` repo). Verify with
`bazel cquery` which non-proxy targets reference the sysroots before removing.

### The `aether_stats` filter: dynamic module now, extension later

The filter (proposal 007, validated in vivo through the 6h soak) stays a
**runtime dynamic module** in the first cut — minimal blast radius, and it's
already decoupled. It simply builds in the `proxy/` workspace now, against the
custom Envoy's in-tree SDK, so `abi.h`/SDK/binary are guaranteed aligned (no more
manual `Cargo.toml` git-tag lockstep). Migrating it to a **compiled-in Rust
extension** (the istio-proxy `filters/http/rust_module` model) is a clean
follow-up once the custom build is the default — that's the payoff that justifies
the custom build, but it doesn't gate this migration.

### Version alignment (009 B1, now structural)

`proxy/bazel/envoy_version.bzl` holds one `ENVOY_VERSION`. The `@envoy`
`http_archive` pin, the compiled binary, the `abi.h` (now read straight from the
`@envoy` source tree, not fetched separately), and the SDK all derive from it —
alignment is *structural*, not a documented convention. `go-control-plane/envoy`
in aether's `go.mod` remains a separate, upstream-gated pin (xDS is
wire-compatible across minors); document the mapping as 009 B1 prescribed.

### Build & release flow

- Local: `cd proxy && bazel build //:image_load` → loads
  `ghcr.io/bpalermo/aether/aether-proxy:latest` for the e2e suite.
- Makefile: a `make proxy-image` / `make load-proxy-image` target shells into
  `proxy/` (so the root Bazel version isn't used). Root `make test` /
  `make build-*` are unchanged and never compile Envoy.
- CI: a dedicated `.github/workflows/proxy.yml` builds/pushes the image on changes
  under `proxy/**`, with its own Bazel cache (7.7.1). Aether's existing CI gains
  nothing slow — it `.bazelignore`s `proxy/`.
- `.gitignore`: add `proxy/bazel-*` (the nested workspace's convenience symlinks).

## The chart image-pin coupling (must be resolved in the cutover)

Today the chart pins the proxy image **through an in-workspace Bazel target**:
`charts/agent/values.yaml` has `ref: "{@//proxy:image_push}"` and
`charts/agent/BUILD.bazel` depends on `//proxy:image_push` — aether's chart build
stamps the pushed image's digest from that target at build time. Once `proxy/` is
a separate, bazelignored workspace, `//proxy:image_push` no longer exists in
aether's graph, so this reference **breaks**. The cutover must replace it. Options:

- **(A, recommended)** Chart pins an **immutable digest**; the `proxy/` CI, after
  `oci_push`, commits the new digest back into `values.yaml` (a bot bump). Keeps
  the repo's "digest pins immutability" philosophy ([[feedback_helm_reuse_values_gotcha]]).
- **(B)** Chart pins a **floating tag** (`:dev` / git-sha) that `proxy/` CI pushes;
  simpler, loses immutability.
- **(C)** Keep a thin aether-side `oci_pull` alias re-pinning the externally
  published image so a `{@//…}`-style stamp still resolves; most machinery.

Recommend **A**. This is a real behavior change to chart image pinning, not a
mechanical move — call it out in the cutover PR.

## Sequencing (so the build is never red)

Because the `.bazelignore` entry is load-bearing (above), the scaffold, the
`.bazelignore` flip, and the aether-side removals (`//bazel/envoy`, `//proxy`
rules_img target, the `MODULE.bazel` rust/crate/envoy wiring, the chart pin, the
Makefile/CI repoint) **cannot be split into separately-green PRs** — root
`bazel build //...` is only green before the scaffold exists or after the full
cutover. Realistic stack:

1. **Cutover PR — `proxy/` becomes a separate oci workspace.** Scaffold the
   istio-proxy-structured `proxy/` workspace (WORKSPACE, bazelrc chain,
   `repositories.bzl`, `extension_config`, `source/exe`, `bazel/oci`, `bazel/rust`)
   building the custom `envoy_cc_binary` + `oci_image`; relocate the `aether_stats`
   filter; add `proxy` to root `.bazelignore`; remove the migrated aether targets;
   resolve the chart pin (option A); repoint Makefile + add the `proxy/` CI
   workflow. One atomic PR — the only green boundary. **Validation gate:** the
   Envoy source compile (Bazel 7.7.1, multi-hour) runs on a build host / CI, not
   in-editor; land behind that green check.
2. **(Follow-up)** First real **patch** to `@envoy` and/or migrate `aether_stats`
   from a runtime dynamic module to a compiled-in extension — exercising the new
   capability the custom build unlocks.

> The earlier draft proposed a 3-PR stack with a "scaffold only, no aether
> changes" PR 1; that was wrong — see the load-bearing `.bazelignore` note. The
> scaffold and cutover are one PR.

## Risks & mitigations

- **Multi-hour C++ build / RAM.** Real cost, but now isolated to `proxy/` CI and
  on-demand local builds; it never touches aether's inner loop. Mitigate with a
  warm remote/CI Bazel cache keyed to `ENVOY_VERSION` + patch set.
- **WORKSPACE is upstream-deprecated (Envoy → bzlmod in 1.39, [#42910]).** We
  intentionally start on WORKSPACE to match istio-proxy and Envoy 1.38. Because
  `proxy/` is *isolated*, flipping it to bzlmod on the 1.39 bump is a
  self-contained change with **zero** risk to aether — exactly the forward path
  009 wanted, now de-risked.
- **Two Bazel versions in one repo.** Expected and supported via per-workspace
  `.bazelversion`; document in `proxy/README.md` that `proxy/` commands must run
  from inside `proxy/`.
- **Scaffolding the Envoy macro chain is fiddly** (009 hit
  `cycles detected … main repo mapping`). **Resolved empirically (2026-06-15):**
  the cycle was a version mismatch — the `bpalermo/istio-proxy` fork's
  `WORKSPACE`/`envoy.bazelrc` target a *newer* Envoy than our v1.38.0 pin and had
  dropped three macros v1.38.0's WORKSPACE calls: `envoy_bazel_dependencies()`
  (sets up `bazel_features` → defines `@bazel_features_version`, the cycle),
  `envoy_repo()`, and `envoy_toolchains()` (registers the hermetic LLVM toolchain
  + `@clang_platform` — no local clang). Lesson: pin `@envoy` **+** `envoy.bazelrc`
  **+** the WORKSPACE macro chain to the *same* Envoy version. With that,
  `cd proxy && bazel build //:envoy` compiles green (hermetic, no local clang).

## Status

**PR 1 landed and build-validated (2026-06-15).** `proxy/` is a separate oci
workspace building a **vanilla custom Envoy v1.38.0** from source — full
`bazel build //:envoy` compiles green hermetically. Filter customization
(`aether_stats`) is intentionally deferred (parked under
`filters/http/aether_stats/`); the aether-side cutover is validated green
(`bazel query //...`, Go targets build). Remaining: re-wire the filter, the
chart digest-pin (option A), and the `proxy/` CI workflow.

## Recommendation

Adopt the separate-sibling-workspace model. It delivers the custom Envoy build
(patches + extensions) the spike's Option A wanted, while honoring 009's core
finding — *don't merge Envoy's build into aether's bzlmod workspace*. The repo is
shared for convenience now; the two build systems stay fully decoupled, so either
can evolve (aether → newer Bazel/bzlmod features; proxy → Envoy 1.39 bzlmod)
without touching the other.
```
