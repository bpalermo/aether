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
> `.github/workflows/proxy.yml`, the PR check, which builds both arches on
> BuildBuddy RBE. The release path is a separate workflow,
> `.github/workflows/proxy-release.yml`: on a push to `main` touching `proxy/**`
> it builds and pushes each arch by digest, assembles the multi-arch manifest
> tagged with the commit SHA, and opens the chart pin PR that moves
> `charts/aether/values.yaml`'s `proxy.image` onto it (#703/#727). Never push a
> proxy image or edit that pin by hand.
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
| `bazel/image_metadata.sh` | emits the image's OCI provenance labels (aether commit + the Envoy pin) |
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

## Which Envoy is this? (`envoy_server_version`, and the image labels)

### What `envoy_server_version` actually reports

**It reports the _aether_ commit the proxy image was built from. It says nothing
about Envoy.** Do not try to match it against the pin — it will never agree, and
the disagreement is not a bug (aether #837).

You do not have to take that on inference. The pinned-Envoy target — today
`//test/envoy_validate:envoy_bin`, via `//bazel/proxy_pin`; #841 factors it out,
so `bazel query` for it if the label has moved — is the Envoy binary extracted
from the **published** `aether-proxy` image at the digest
`charts/aether/values.yaml` pins. You can ask it directly, locally, with no
cluster:

```console
$ bazel build //test/envoy_validate:envoy_bin
$ "$(bazel info output_base)/$(bazel cquery --output=files //test/envoy_validate:envoy_bin)" --version
envoy  version: cff8beb87eb685a44e00cf70c1a534695039da1b/1.40.0-dev/Clean/RELEASE/BoringSSL
```

`cff8beb87eb685a44e00cf70c1a534695039da1b` is an **aether** commit — it is that
image's own `tag:` in `values.yaml`. The Envoy part of the string is just
`1.40.0-dev`, with no Envoy revision in it anywhere. So for that image the gauge
is `0xcff8be`; a proxy reporting something else is simply an older image.

The chain that produces it, end to end:

1. `.bazelrc` sets `--workspace_status_command="bash bazel/get_workspace_status"`,
   and that script runs `git rev-parse HEAD`. `proxy/` is a *nested* workspace
   inside the aether git repository, so `BUILD_SCM_REVISION` is the **aether**
   commit. Envoy is a bzlmod dependency here; nothing ever asks *its* git for a
   revision.
2. `envoy_cc_binary` defaults to `stamp = 1` (Envoy's `bazel/envoy_binary.bzl`),
   which is `cc_binary`'s "always stamp, even under `--nostamp`". So the
   `//source/common/version:version_linkstamp` linkstamp compiles that real sha
   in even though no build here passes `--stamp`. (Verified: with a plain
   `cc_binary`, `--nostamp` yields the redacted `BUILD_SCM_REVISION` of `"0"`;
   with `stamp = 1` it yields the workspace-status value.)
3. `VersionInfo::revision()` returns it, and Envoy's `server.cc` does
   `atoull(revision().substr(0, 6), 16)` to set the `server.version` gauge.

So the gauge is the **first six hex digits of the aether commit**. The reading
that opened #837 decodes cleanly that way:

```
envoy_server_version = 3121738 = 0x2FA24A -> aether commit 2fa24a8
  "test: drop duplicate integration targets ... (#772) (#788)"
```

That is a genuinely useful fact — it tells you which aether tree cut the proxy —
but it is only 24 bits of it, and it is *not* the Envoy revision. `13144f`, the
first six digits of the pinned `1.40.0-dev.20260904.13144fb.envoy`, is what you
would be looking for and it is nowhere in the process.

### Where the Envoy revision actually is: the image labels

`//:image_metadata` (see `bazel/image_metadata.sh`) parses the pin out of the
two files that define it and puts it on the image as both OCI **labels** (image
config) and **annotations** (manifest):

| key | value |
|---|---|
| `org.opencontainers.image.source` | `https://github.com/bpalermo/aether` — **overrides** the `GoogleContainerTools/distroless` value inherited from the base, which used to be the image's only annotation |
| `org.opencontainers.image.revision` | the aether commit; the same sha the gauge reports the first six digits of |
| `dev.aethermesh.envoy.module-version` | e.g. `1.40.0-dev.20260904.13144fb.envoy` |
| `dev.aethermesh.envoy.revision` | e.g. `13144fb` — the upstream Envoy commit |
| `dev.aethermesh.envoy.bazel-registry` | the `envoyproxy/bazel-registry` commit; the other half of the pin (see "Envoy version bumps") |

To read them off a published image, without pulling it:

```bash
crane config ghcr.io/bpalermo/aether/aether-proxy@sha256:… | jq .config.Labels
crane manifest ghcr.io/bpalermo/aether/aether-proxy@sha256:… | jq .annotations
```

None of this costs reproducibility: the labels are fixed strings, `created`
stays unset (`1970-01-01T00:00:00Z`) and the history entries stay `bazel bu…`.
The metadata genrule is tagged `no-cache` on purpose — `BUILD_SCM_REVISION` is a
*volatile* workspace-status key, which Bazel deliberately does not invalidate
on, so a cacheable action would re-stamp a stale commit.

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

**Check `modules/envoy/metadata.json` first.** It lists every `envoy` version the
registry currently publishes, and one API call answers the whole question:

```bash
gh api repos/envoyproxy/bazel-registry/contents/modules/envoy/metadata.json \
  --jq '.content' | base64 -d | jq .versions
```

If the only version listed is the one already pinned, there is nothing to bump to
and the loop below will confirm it the slow way. The real gate on a bump is a new
`envoy` snapshot, not a newer registry commit.

Otherwise the bump recipe is a **resolvability loop** — find the newest registry
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

Two traps when checking a candidate commit:

- **`--registry` accumulates; it does not override.** Adding
  `--registry=…/<candidate>` on the command line *appends* a registry behind the
  two `.bazelrc` already pins, so the pinned commit still answers every lookup
  and the probe passes no matter how broken the candidate is. To
  test a candidate you must edit the `--registry=` line in `.bazelrc`, and pass
  `--lockfile_mode=off` so `MODULE.bazel.lock` does not answer from cache.
- **A missing version directory is only fatal if nothing requests a higher
  one.** Bazel selects the MVS maximum, so `foo@1.3.0.envoy` resolves fine if
  some other module in the closure asks for `foo@1.3.3.envoy` and *that*
  directory exists. The step-3 rule above is the conservative approximation; if
  it rejects a commit you otherwise want, re-check against the selected version
  rather than the requested one.

If the loop finds no commit newer than the current pin, that is the normal
steady state between snapshots, **not** a failed bump: the registry keeps one
version per module and bumps siblings ahead of `modules/envoy/<version>`, so
after a snapshot publishes it is usually resolvable for only a handful of
commits. Do not "fix" the misses with `single_version_override` — forcing a
sibling past what the `envoy` module was published against buys an unvalidated
combination that only a multi-hour CI build can disprove. Wait for the next
snapshot.

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

When a stable release publishes a `1.40.0.envoy` (etc.) module, move to it. As of
2026-09-19 no such module exists — `modules/envoy/metadata.json` still lists only
the `1.40.0-dev.20260904.13144fb.envoy` snapshot.

Nothing in the root workspace needs re-pinning alongside it any more. The root
`MODULE.bazel` used to carry `@envoy_binary_linux_*`, a stock Envoy release asset
that `//test/envoy_validate` ran `envoy --mode validate` with, and that pin had to
move in step. #709 replaced it: `//bazel/proxy_pin` now lifts
`/usr/local/bin/envoy` out of the aether-proxy image at the digest
`charts/aether/values.yaml` pins, so the validate gate follows the chart pin — and
therefore this module — with no second version to keep in sync.

The `aether_stats` C++ extension builds against this same Envoy tree, so there
is no separate SDK version to keep in sync.

## Status

**Custom Envoy + `aether_stats` C++ extension building on CI** (proposals 010 /
012), on bzlmod against an Envoy `main` dev snapshot (#697). The Envoy compile
runs on BuildBuddy RBE — amd64 driven from an x64 runner, arm64 driven from a
native `ubuntu-24.04-arm` runner (`@clang_platform` and `@llvm_toolchain` derive
their exec constraints from the *driver* host's arch, so an x64 driver cannot
drive the arm64 pool).
