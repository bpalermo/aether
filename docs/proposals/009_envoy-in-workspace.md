# Proposal: Envoy in the Bazel workspace — version alignment, validation, customization

**Status:** Superseded by [proposal 010](010_custom-proxy-workspace.md) — spike /
investigation, recommendation below (2026-06-14)
**Author:** Bruno Palermo
**Date:** 2026-06-14

> **Superseded by [proposal 010](010_custom-proxy-workspace.md).** This spike
> evaluated bringing the Envoy build *into aether's bzlmod workspace* and rejected
> it (Envoy ≤1.38 is not a bzlmod citizen; the version/Bazel mismatch makes merging
> the two build systems the part that fails). Proposal 010 keeps the goal — a
> customizable Envoy build — but changes the framing to a separate sibling Bazel
> workspace at `proxy/`, which is what was implemented (proposals 010/011/012).

## Problem statement

Aether runs the **stock** upstream Envoy as the data plane, but several things in
this repo must track the Envoy version, and today they are **independent manual
pins** that can silently drift:

| Artifact | Where | Pinned to |
|---|---|---|
| Proxy image | `charts/agent/values.yaml` (`proxy.image.digest`) | `envoyproxy/envoy:distroless-v1.38.0` |
| Dynamic-modules ABI header (`abi.h`) | `//bazel/envoy` (`ENVOY_VERSION`) | `v1.38.0` |
| Dynamic-modules Rust SDK | `proxy/filters/telemetry/Cargo.toml` (git tag) | `v1.38.0` |
| xDS control-plane API | `go.mod` (`go-control-plane/envoy`) | `v1.37.0` |

The stats filter (proposal 007) made this concrete: a dynamic module's ABI is
tied to the proxy's Envoy build, so `abi.h` + SDK + proxy image must be the same
Envoy version, or the module fails to load. The ask for this spike:

1. **Version-align** the control plane, `abi.h`, and the proxy binary.
2. Allow **customization of the proxy image** (e.g. bake the module in).
3. **Validate** the proxy + its config as part of the Bazel build/CI.

The headline framing was "bring the Envoy proxy build into the workspace." This
spike evaluates that literally, finds it impractical, and recommends a lighter
design that meets all three goals.

## Evidence gathered

- **Envoy is not consumable as a bzlmod dependency here.** `envoy`,
  `envoy_build_config`, and `envoy_mobile` are **not on the BCR** (only
  `envoy_api` is). Envoy's `MODULE.bazel` (`v1.38.0`) depends on all of them
  plus a large C++ graph; a `bazel_dep(name = "envoy")` would require
  `archive_override`/`git_override` on Envoy itself **and** its internal modules,
  then resolving Envoy's full C++ build toolchain (LLVM, sysroots, BoringSSL,
  abseil, protobuf, gRPC, QUICHE, …). (Confirmed earlier in proposal 007: even
  depending on the single in-tree `@envoy//…sdk/rust` target dragged
  `@envoy_repo`, `@llvm_toolchain_llvm`, `@envoy_rust_crate_index`, sysroots and
  `envoy_build_system`.)
- **A full Envoy source build is a multi-hour, high-RAM C++ build.** It is the
  exact cost the dynamic-modules mechanism exists to avoid — proposal 007 chose a
  runtime-loaded module specifically so we never compile Envoy.
- **go-control-plane lags Envoy releases.** The newest `go-control-plane/envoy`
  tag is `v1.37.0`; there is **no 1.38** yet. So the agent already generates xDS
  for a 1.38 proxy using 1.37 API types. xDS is wire-compatible across minor
  versions, so this is safe, but it means **the four pins cannot all be one
  version today** — go-control-plane is gated by its own upstream cadence.

## Option A — build Envoy from source in the workspace

Pull Envoy as a Bazel repo and build the proxy binary/image in-tree. Two ways to
wire it; both attempted empirically.

### A1 — `@envoy` as a bzlmod dep (fails today)
A throwaway module with `bazel_dep(name="envoy") + git_override(commit=v1.38.0)`
and `bazel mod deps` clones Envoy, reads its `MODULE.bazel`, and **fails at the
first transitive dep**:

```
ERROR: <root> -> envoy@_ -> envoy_api@1.37.0-dev: module envoy_api@1.37.0-dev
not found in registries: https://bcr.bazel.build/.../envoy_api/1.37.0-dev/MODULE.bazel: not found
```

Envoy (≤1.38) pins its own internal modules to unpublished `-dev` versions, so
`@envoy` needs `*_override`s for `envoy_api`, `envoy_build_config`,
`envoy_mobile` (the latter two aren't on BCR at all) — i.e. it isn't really a
bzlmod citizen yet.

### A2 — WORKSPACE-based custom Envoy (Envoy's own macros)
The canonical pre-bzlmod pattern (cf. `bpalermo/custom-envoy`): a **separate
WORKSPACE workspace** that `http_archive`s `@envoy` and runs Envoy's dependency
chain (`envoy_api_binding` → `envoy_api_dependencies` → `envoy_dependencies` →
`…_extra` → `python_dependencies` → `dependency_imports`), then builds
`@envoy//source/exe:envoy`. This sidesteps BCR entirely.

Empirically attempted for v1.38.0: it requires **Bazel 7.7.1** (our repo is
9.1.1 — must be a separate workspace) and the macro chain exists at v1.38.0. A
minimal port failed during WORKSPACE evaluation:

```
ERROR: Failed to load '@@bazel_features_version//:version.bzl';
cycles detected during computation of main repo mapping
```

i.e. it needs the reference repo's full scaffolding (`clang.bazelrc`,
`platform_mappings`, `setup_clang.sh`, the complete prelude) — non-trivial — and
then still a multi-hour C++ compile.

### Why A is rejected (now)
- **Upstream is killing the WORKSPACE path.** [envoyproxy/envoy#42910]: *"Envoy
  will move to bzlmod during the 1.39 dev cycle; WORKSPACE builds will no longer
  be supported,"* with docs to follow on building Envoy as an external **bzlmod**
  repo. So A2 (WORKSPACE) is a dead-end to invest in, and A1 (bzlmod) is exactly
  what 1.39 fixes — today it fails only because 1.38 predates the migration.
- A full Envoy C++ build (either way) is the multi-hour cost dynamic modules
  exist to avoid; its only unique payoff — **C++ extensions** — we don't need.

**Forward path:** when Envoy 1.39 lands its bzlmod migration + external-build
docs, re-evaluate A1 — a clean `bazel_dep`/override on `@envoy` would then give a
genuinely version-aligned, customizable, in-Bazel proxy build. Until then, A is
not worth it; adopt Option B.

## Option B — keep stock Envoy; align + validate + (optionally) repackage (recommended)

Meet the three goals without compiling Envoy.

### B1. Single Envoy-version source of truth
Centralize one `ENVOY_VERSION` (the upstream git tag, e.g. `v1.38.0`) in
`//bazel/envoy` and derive from it:
- the `abi.h` fetch URL (already there),
- the dynamic-modules SDK git tag (today a separate literal in `Cargo.toml` —
  document the lockstep, or template it),
- the proxy image **tag** in the chart (the digest still pins immutability; a CI
  check asserts the digest's tag == `ENVOY_VERSION`).

go-control-plane is a **separate Go-module version** that lags upstream; it
cannot be set by `ENVOY_VERSION`. Document the mapping (`v1.38.0` → the latest
go-control-plane that supports it, today `1.37.0`) and add a reminder/check to
bump it when go-control-plane ships the matching minor. The xDS API skew is safe
(wire-compatible).

### B2. Bazel/CI validation against the stock image (the highest-value, cheapest win)
The real risk on an Envoy bump is "config or module silently breaks." Catch it
in CI without building Envoy by running the **pulled** stock image:
- **Config validation:** `envoy --mode validate -c <agent-rendered bootstrap>`
  as a Bazel test — proves the chart's `envoy.yaml` (and any generated bootstrap)
  is accepted by the pinned Envoy.
- **Module-load test:** load `libaether_stats_filter.so` into that Envoy and
  drive a request, asserting the listener is accepted and the module loads (we do
  this manually via docker today — promote to a Bazel/integration test). This is
  the canary that catches ABI/version drift between the proxy and the module.
- `container_structure_test` (already used in the repo) can assert
  `envoy --version` == `ENVOY_VERSION` on the image.

**Empirically validated.** The chart's rendered bootstrap (`envoy.yaml` from the
proxy ConfigMap, 170 lines) run through the pinned stock image:

```
docker run envoyproxy/envoy:distroless-v1.38.0 -c bootstrap.yaml --mode validate \
  --service-node test-node --service-cluster aether-proxy
→ configuration 'bootstrap.yaml' OK
```

(It first surfaced a real gap — `node id/cluster are required` — which the chart
supplies via `--service-node`/`--service-cluster`; the validate test must pass
those, exactly as the DaemonSet command does.) This is a ~1s test, no Envoy
build, and would have caught any bootstrap breakage on the Envoy bump.

### B3. Optional: custom proxy image via rules_img (not a source build)
If/when "customize the image" is needed, build a **custom image = stock Envoy
distroless base + baked-in module(s)** with `rules_img` (we already build the
`.so` and a `image_index`). This is repackaging, not compiling Envoy.

| | Image volume (current) | Custom proxy image (rules_img repackage) |
|---|---|---|
| Proxy image | stock upstream | ours (base = stock + layer) |
| Module delivery | K8s `volume.image` at runtime | baked in |
| Runtime dep | containerd ≥ 2.0 (image volumes) | none |
| Coupling | module released independently | module version == image build |
| Maintenance | none on the image | rebuild/re-publish on Envoy or module bump |
| CVE patching | inherit upstream image as-is | rebuild on base bump (still stock base) |

Keep the image volume as default (stock image, decoupled releases); offer the
custom image for clusters without image-volume support or that prefer a
self-contained artifact.

## Recommendation

**Do not bring the Envoy source build into the workspace yet (reject Option A
today).** The WORKSPACE path (A2) is upstream-deprecated as of 1.39
([#42910]); the bzlmod path (A1) only fails because 1.38 predates Envoy's bzlmod
migration. **Revisit Option A1 when Envoy 1.39 ships its bzlmod external-build
support** — at that point an `@envoy` `bazel_dep` could give a version-aligned,
customizable, in-Bazel proxy build cleanly. Until then, adopt Option B:

1. **B1** — one `ENVOY_VERSION` drives `abi.h` + SDK tag + proxy image tag;
   document the go-control-plane mapping + bump trigger; add a CI assert that the
   image's Envoy version matches.
2. **B2** — add Bazel tests: `envoy --mode validate` on the bootstrap and a
   module-load test against the pinned stock image. This is the concrete
   "validate as part of Bazel" deliverable and the best protection against
   version drift.
3. **B3** — implement the custom-image repackage as an *option* alongside the
   image volume, only if a deployment needs it.

This delivers all three goals — alignment (B1), validation (B2), customization
(B3) — at a fraction of the cost and risk of compiling Envoy, and keeps us on the
stock, upstream-maintained, CVE-patched Envoy binary.

## Appendix — spike facts & empirical results (2026-06-14)

- BCR: `envoy` 404, `envoy_api` 200, `envoy_build_config` 404, `envoy_mobile` 404.
- **Option A1 run (bzlmod):** `bazel mod deps` with `git_override(envoy@v1.38.0)`
  → `envoy_api@1.37.0-dev not found in registries` (Envoy pins unpublished `-dev`
  internal modules). Infeasible without multiple overrides + the C++ graph.
- **Option A2 run (WORKSPACE):** custom-envoy-style workspace pinned to v1.38.0
  (Bazel 7.7.1, Envoy's macro chain) → minimal port fails in WORKSPACE eval
  (`@@bazel_features_version//:version.bzl … cycles in main repo mapping`); needs
  the full reference scaffolding + a multi-hour build.
- **[envoyproxy/envoy#42910]:** Envoy moves to **bzlmod in the 1.39 dev cycle;
  WORKSPACE builds will no longer be supported** (external bzlmod build docs to
  follow). → WORKSPACE is EOL; A1 is the future path.
- **Option B2 run:** `envoy --mode validate` (stock `distroless-v1.38.0`) on the
  chart's rendered bootstrap → `configuration OK` (with
  `--service-node/--service-cluster`). ~1s, no build.
- `go-control-plane/envoy` newest tag: `v1.37.0` (no 1.38) — the API skew is
  forced by upstream cadence; xDS is wire-compatible across minors.
- Envoy `MODULE.bazel` @ `v1.38.0` deps: aspect_bazel_lib, envoy_api(-dev),
  envoy_build_config(-dev), envoy_mobile(-dev), gperftools, numactl, platforms,
  rules_python, rules_rust 0.67.0, rules_shell, zlib, zstd (+ large transitive
  C++ graph).
- Current pins: proxy `distroless-v1.38.0`; `abi.h`/SDK `v1.38.0`;
  go-control-plane `v1.37.0`.

[#42910]: https://github.com/envoyproxy/envoy/issues/42910
[envoyproxy/envoy#42910]: https://github.com/envoyproxy/envoy/issues/42910
