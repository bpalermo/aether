# Proposal 045: Move aether-proxy to the Envoy 1.41 dev snapshot, under Bazel 9

**Status:** Proposed. The owner decided on 2026-10-31 to make the bump. The
implementation is the pull request stacked on this one. Nothing is released
until the 8 h soak in *Validation* has passed. The facts below were read from
`main` at `9b256ed5f4`, from Envoy at `011e39616b`, and from
`envoyproxy/bazel-registry` at `36d481b01b`.
**Author:** Bruno Palermo
**Date:** 2026-10-31
**Related:** proposal [010](010_custom-proxy-workspace.md) (the proxy
workspace), `proxy/README.md` § *Envoy version bumps* (the recipe this follows),
aether#1050, #1054, #1074, #1126 (the carried patches), aether#980 (the
previous snapshot pin).

## The decision

> **aether-proxy moves from `envoy`/`envoy_api` `1.40.0.envoy` (registry
> `a1264b53`) to `1.41.0-dev.20261028.011e396.envoy` (registry `36d481b01b`),
> and its workspace moves from Bazel 8.7.0 to 9.3.1 in the same change. Two
> carried patches drop (aether1054c, aether1074). Four stay: 47740 and aether1126
> as they are, aether1050 and aether1054 with their context refreshed.**

No upstream pull request or issue is opened or commented on for this. The
owner has put all upstream Envoy activity on hold, so aether1126 stays
unreported and envoyproxy/envoy#47740 and #47834 are left alone.

## Why this version

aether consumes Envoy only as a module of the envoy bazel-registry.
`modules/envoy/metadata.json` at `36d481b01b` lists four versions: the
`189f198` dev snapshot (twice, `.envoy` and `.envoy.1`), the `1.40.0.envoy`
release, and `1.41.0-dev.20261028.011e396.envoy`. Measured:

- **v1.40.1 and v1.40.2 are not published there.** They would gain nothing
  anyway. `git log v1.40.0..v1.40.2` is CI, dependency and istio backports, and
  neither envoyproxy/envoy#47868 nor #47893 was backported.
- **Envoy `main` past `011e396` has no snapshot.** On 2026-10-31 that is 55
  commits. Pinning it would take a `git_override` that leaves the registry's
  `.envoy` closure. The README's rule against forcing a closure the registry
  did not publish applies here.
- **`011e396` contains both fixes the release lacks.** envoyproxy/envoy#47868
  merged as `2eff6e8ccf` (2026-10-13) and #47893 as `77b512527c` (2026-10-20).
  `git merge-base --is-ancestor` confirms both are in `011e396` and neither is in
  `v1.40.0` or `v1.40.2`.

The snapshot is 276 commits past `v1.40.0`.

## The carried patches

Each patch was checked by applying the series in order with `git apply` to a
worktree of the upstream commit (measured):

| Patch | Upstream | At `011e396` | Action |
|---|---|---|---|
| `47740-quic-hostname-check-explicit-san` | envoyproxy/envoy#47740, open | applies unchanged | keep |
| `aether1050-hotrestart-nonblocking-forward` | envoyproxy/envoy#47834, open | the source hunks apply; `test/server/hot_restarting_base_test.cc` conflicts next to #47068's new tests | keep, refreshed (below) |
| `aether1054-http3-drain-idle-connections` | not proposed | two one-line insertions reject on context: the runtime guards and initializer list around them changed upstream | keep, refreshed (below) |
| `aether1054c-paused-udp-listener-no-read` | envoyproxy/envoy#47868, merged `2eff6e8ccf` | reverse-applies (already present) | **drop** |
| `aether1074-quic-persistent-info-lifetime` | envoyproxy/envoy#47893, merged `77b512527c` | no longer applies; upstream merged the same fix, plus a `cluster_manager_impl.cc` hunk the carried patch did not need | **drop** |
| `aether1126-forwarded-udp-worker-index` | not reported | applies unchanged | keep |

The `proxy/MODULE.bazel` comment that still called #47893 "open" was stale:
it merged on 2026-10-20. The rewritten comment says so.

### aether1050: the refresh, and the semantic check against #47068

**The refresh.** The pull request branch for envoyproxy/envoy#47834 was merged
with Envoy `main` at `ab652e7164` (2026-10-26), after #47068. Its head,
`39dacbff0f`, therefore already resolves the conflict. The refreshed patch is
`git diff ab652e7164 39dacbff0f -- source/server test/server`, the same scope
as before (docs and changelog left out). Its `+`/`-` lines are byte-identical to
the carried patch's; only context moved. It applies cleanly at `011e396` on
top of 47740.

**The semantic check.** envoyproxy/envoy#47068 changed the same receive loop
aether1050 changes. A length prefix within `sizeof(uint64_t)` of `UINT64_MAX` is
now dropped: `resetReceiveState()`, then `continue`. Read in the merged
`RpcStream::receiveHotRestartMessage()`:

- **Non-blocking mode:** after the drop the next `recvmsg()` returns `EAGAIN`,
  and the loop returns `nullptr` as before. Unchanged.
- **Blocking mode:** aether1050 gives the domain socket `SO_RCVTIMEO`
  (`RECEIVE_TIMEOUT_SLICE`). After a drop, the next `recvmsg()` waits at most one
  slice. With no datagram it returns `nullptr`, with `O_NONBLOCK` restored.
  Upstream's comment says "the loop then blocks for the next datagram". Under
  aether1050 that wait is bounded, and every blocking caller the patch
  introduces (the child's reply wait that keeps servicing forwarded packets)
  already treats `nullptr` as "keep waiting until the deadline". No caller
  is left expecting a non-null message from a blocking receive.
- **Buffered partial messages:** the drop happens only on the first fragment's
  length prefix. A partial message aether1050 keeps across calls is
  never discarded by it.
- **#47068's tests** (`OversizedLengthPrefixDropped*`) build their socket with
  `socketpair()` and never call `setSocketTimeouts()`. Their blocking case
  writes both datagrams before receiving, so it still returns the second
  message under the patch. They run in `//bazel/patches:carried_patch_tests`
  next to the patch's own cases.

Conclusion: the two changes compose. Nothing in the patch's logic changes for
#47068.

### aether1054: the refresh

The two hunks that rejected are the `drain_idle_http3_connections_` initializer
in `ConnectionManagerImpl`'s constructor and the
`envoy_reloadable_features_http3_drain_idle_connections` guard in
`runtime_features.cc`. Their neighbours (`header_mutation_url_encode_query_params`,
`http1_close_connection_on_zombie_stream_complete`) were removed upstream.
Both lines were reinserted at the same place. The member is still declared
right after `use_connection_event_drain_`, so the initializer order matches
the declaration order. `use_connection_event_drain`, which the patch requires,
still exists at `011e396`. Upstream changed no drain logic in
`conn_manager_impl.cc` between `v1.40.0` and `011e396`: the four commits that
touch it are filter buffering, span tags, idle counters and a guard removal.
The `+`/`-` lines are identical to the carried patch's.

## Why the Bazel 9 move rides with the bump

The owner asked for the Bazel move to land alone on 1.40.0 first, if that were
possible. It is not. Measured on this workspace with Bazel 9.3.1 and the
`1.40.0.envoy` pin:

1. `bazel mod graph` fails: `c-ares@1.34.6.bcr.1` is yanked (CVE-2025-62408).
   Bazel 8.7.0 resolved the same graph from the lockfile.
2. With `c-ares@1.34.6.bcr.3` forced, module extension evaluation fails in
   `emsdk@4.0.23.envoy` (`emscripten_toolchain/toolchain.bzl`, "at index 0 of
   provides, got element of type NoneType"). That repository is loaded even
   though the wasm extensions are compiled out.
3. With `emsdk@4.0.23.envoy.1` forced as well, and
   `--incompatible_merge_fixed_and_default_shell_env` removed (Bazel 9 dropped
   it), analysis fails in `proxy-wasm-cpp-host`'s `bazel/v8_transition.bzl`:
   "The CcInfo symbol has been removed".

The registry fixed these for Bazel 9 by publishing `.envoy.1` revisions of
about twenty modules. The 1.41 snapshot's closure references them; the
1.40.0 closure does not. Running 1.40.0 under Bazel 9 would mean overriding
each of those modules by hand. That is an unpublished combination nobody
upstream builds, which the README tells us not to create. Upstream Envoy
itself moved to 9.3.1 between the two pins (envoyproxy/envoy#48608,
#48527), so the snapshot is built and tested upstream under exactly that
version.

## What else changes in `proxy/`

- **`.bazelversion`** `8.7.0` → `9.3.1`, the version upstream Envoy pins at
  `011e396`.
- **`MODULE.bazel`:** the pin and its `single_version_override` move together.
  `proxy-wasm-cpp-host` moves to `0.0.0-260704-f2db56a.envoy.1`, `rules_cc`
  `0.2.25` → `0.2.27`, and `rules_python` `2.3.3` → `2.4.5`. The resolved graph
  selects those versions, and `--check_direct_dependencies` warns when the root
  names an older one. `protobuf`, `rules_rust`, `toolchains_llvm` and
  `abseil-cpp` are unchanged. Transitively, `quiche` moves from
  `261006-9e60ac2` to `261019-67bce2e` (envoyproxy/envoy#48288).
- **`.bazelrc`:**
  - The registry line moves to `36d481b01b`.
  - `--incompatible_merge_fixed_and_default_shell_env` goes: removed in Bazel 9,
    as upstream did in #48608.
  - `--features=system_include_paths --host_features=system_include_paths`
    comes in with upstream's Bazel 9 move (#48527).
  - The two rules_rust `-Alinker_messages` flags come in with the compiler-rt
    switch.
  - `--@toolchains_llvm//toolchain/config:compiler-rt=false` and
    `config:libunwind=false` go. Upstream dropped both in envoyproxy/envoy#47716.
- **Clang runtimes (envoyproxy/envoy#48641):** the envoy module's own
  `llvm.toolchain` now links with `-rtlib=compiler-rt -unwindlib=libunwind
  -static-libgcc`. The binary's runtime builtins and unwinder are LLVM's,
  linked statically, instead of libgcc's. Measured with `bazel aquery` on
  `//:envoy`: those flags are on the link line, and `--build-id=sha1` is still
  the last `--build-id` (the aether#651/#653 invariant the `proxy` workflow
  asserts). The image stays on `distroless/cc`. The `symtab_test` and
  `build_id_test` gates are unchanged and run in the workflow.
- **Carried patches:** two files deleted, two refreshed. The comments in
  `//bazel/patches:carried_patch_tests` say what each remaining target covers.
  `udp_listener_impl_test` stays: it carries aether1126's case, and now runs
  #47868's upstream case too.
- **Docs:** `proxy/README.md` (the carried-patch table, the pin, the Bazel
  version), `README.md`, `Makefile` and `docs/runbook.md` (the Bazel version
  of the proxy workspace).

Nothing outside `proxy/` and the docs changes. `go-control-plane/envoy` stays
`v1.40.0`. `scripts/check-envoy-api-parity.sh` accepts bindings one minor
behind a `-dev` pin, which this is. No chart change: `proxy-release.yml` opens
the chart pin pull request after the merge, as for every proxy change.

## Risk: upstream changes that touch what aether depends on

Read from `git log v1.40.0..011e396` over the hot-restart, UDP, QUIC, HTTP
connection manager, TLS, listener and xDS sources. Inferred: none of these
was exercised against aether's configuration before the soak.

| Change | What it touches in aether | Expected effect | Watched by |
|---|---|---|---|
| envoyproxy/envoy#45504, "QUIC eBPF hot restart part 0" | QUIC listener setup: the connection-ID generator factory and UDP socket options move to `initializeWorkerRouting()`, and the listener's init target registers after it. Behind `envoy.restart_features.defer_worker_routing_init`, default **on**. Touches no hot-restart IPC file. | No behaviour change intended. The path is the one aether1050 and aether1126 sit next to (QUIC listeners on a hot-restart child, CID → worker routing). | soak: proxy rolls under QUIC load, stateless resets, misses near handoffs |
| envoyproxy/envoy#47068 | the hot-restart receive loop | composes with aether1050 (see above) | `carried_patch_tests`; soak rolls |
| envoyproxy/envoy#48570, upstream TLS default max 1.3 | every TCP mesh hop: aether sets only `tls_minimum_protocol_version` (TLSv1_2), so mesh upstreams now offer and negotiate TLS 1.3. QUIC was 1.3 already. Behind `envoy.reloadable_features.default_client_tls_max_version_is_1_3`. | A wire-visible change. Resumption stays off (`max_session_keys: 0`). The per-source certificate selector is exercised under 1.3 for the first time. | soak: TCP tiers of the prober; the guard is the rollback lever (see *Rollback*) |
| envoyproxy/envoy#47067, `wait_for_warm_on_init` for all cluster types | CDS warming: one CDS pause handle per warming cluster instead of one global handle | aether sets no `wait_for_warm_on_init` (default true): each warming cluster still holds CDS back. ACK timing per cluster may shift. | soak: agent ACK/NACK metrics, the `503 NC` count at twin admission |
| envoyproxy/envoy#48024, ODCDS remembers "missing" | QUIC twins over ODCDS | behind `envoy.reloadable_features.odcds_missing_cluster_cache`, **default off** at `011e396`: no change unless turned on | nothing to watch; re-check when upstream flips it |
| envoyproxy/envoy#48214, `prefer_gro` honoured | the inbound QUIC listener | aether sets `prefer_gro: true` (`agent/internal/xds/proxy/ingress.go`), so GRO stays on; the fix only stops Envoy turning GRO on where `prefer_gro` is false | soak: QUIC per-request CPU (the #1021 gate) |
| envoyproxy/envoy#48403 (listener addresses updated in place), #48413 and #47455 (use-after-free fixes in connection-handler and accepted-socket teardown) | listener updates, drains | fixes; no aether configuration change needed | soak |
| envoyproxy/envoy#48288, QUICHE `132bfd9` | every HTTP/3 hop | dependency bump | soak |

Runtime flags aether sets: `hot_restart_propagate_stat_tags` still exists at
`011e396`, and the only other one is 47740's own guard. None of the guards
upstream removed in this range is one aether sets.

## Validation

Before merge:

1. Local, measured on this change:
   - `bazel mod graph --depth=1` resolves with no warning.
   - `bazel mod show_repo --base_module=envoy @envoy_build_config` is our
     `local_repository`.
   - `bazel build --nobuild //...` analyses all 30 targets, which fetches
     `@envoy` with the four patches applied.
   - `scripts/check-envoy-pin.sh` and `--online` pass.
   - The repository gates pass: `make test-unit`, `make lint`,
     `make format-check`, `make gazelle`, `make tidy`.
2. The `proxy` workflow on the implementation pull request, both arches on
   RBE:
   - the build;
   - `//...`, which runs `//bazel/patches:carried_patch_tests` (aether1050's
     four hot-restart targets, aether1054's `conn_manager_impl_test`, and
     aether1126's and #47868's `udp_listener_impl_test`);
   - `image_test`, `symtab_test` and `build_id_test`;
   - the `--build-id=sha1` link-line check;
   - `envoy_validate` of the agent's generated configuration against the
     binary this pull request builds.
3. The tests that build only in the Envoy workspace: aether1054's
   `quic_http_integration_test` case, and aether1050's suite under Envoy's own
   test setup. They run from an Envoy checkout at `011e396` with the four
   patches applied. The implementation pull request says whether they were
   run, and with what result.

Before release: **an 8 h soak** (the aether-soak-runner procedure: sortie
load, rollout churn, the external prober) on the build of the merged pull
request. `AGENTS.md` does not require one for an Envoy bump. Every
hot-restart and QUIC change before this one was soaked, and this one carries
#45504, the TLS 1.3 default and a new toolchain runtime. The soak is started
by the owner, not by this change.

## Rollback

Revert the implementation pull request. That restores `1.40.0.envoy`, Bazel
8.7.0 and the six patches. `proxy-release.yml` then builds the previous proxy
again, and the chart pin follows. To keep the bump and back out only a
behaviour, use the runtime guards: `default_client_tls_max_version_is_1_3`
(TLS 1.3) and `defer_worker_routing_init` (#45504) can be set false without
rebuilding Envoy. The proxy bootstrap (`charts/aether/templates/agent-proxy-configmap.yaml`)
has no `layered_runtime` today, so doing that takes a chart change that adds a
static layer. The admin endpoint's `/runtime_modify` works too, but it is lost
at the next hot restart.

## Not verified

- No local Envoy build and no local test run. The compile and every Envoy test
  run in the `proxy` workflow (point 2 of *Validation*).
- Whether the 1.41 snapshot resolves under Bazel 8. It was never tried: the
  move is to Bazel 9 regardless.
- The behaviour of the risk table's changes on aether's configuration. That is
  what the soak is for.
- `scripts/check-envoy-pin.sh --online` checks the `.envoy`-suffixed direct
  dependencies. `proxy-wasm-cpp-host`'s `.envoy.1` version did not appear in
  its output, so that one was confirmed by `bazel mod graph` alone.
