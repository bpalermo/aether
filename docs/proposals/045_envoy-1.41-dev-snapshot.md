# Proposal 045: Move aether-proxy to the Envoy 1.41 dev snapshot, under Bazel 9

**Status:** Accepted; implemented by #1733. The owner decided on 2026-10-31 to
make the bump, and on 2026-11-01 that the soak validates the deployed full
bump: the release goes out through `proxy-release.yml` in its normal auto
mode, with no hold and no stage before the soak (see *Release and soak*). The
facts below were read from
`main` at `9b256ed5f4`, from Envoy at `011e39616b`, and from
`envoyproxy/bazel-registry` at `36d481b01b`.
**Author:** Bruno Palermo
**Date:** 2026-10-31
**Related:** proposal [010](010_custom-proxy-workspace.md) (the proxy
workspace), `proxy/README.md` § *Envoy version bumps* (the recipe this follows),
aether#1050, #1054, #1074, #1126 (the carried patches), aether#980 (the
previous snapshot pin), #1733 (the implementation, and aether1733's name),
proposal [042](042_sortie-soak-driver.md) (the soak).

## The decision

> **aether-proxy moves from `envoy`/`envoy_api` `1.40.0.envoy` (registry
> `a1264b53`) to `1.41.0-dev.20261028.011e396.envoy` (registry `36d481b01b`),
> and its workspace moves from Bazel 8.7.0 to 9.3.1 in the same change. Two
> carried patches drop (aether1054c, aether1074). Four stay: 47740 and aether1126
> as they are, aether1050 and aether1054 with their context refreshed. One is
> added: aether1733, for a crash the snapshot brings into `envoy --mode
> validate` (see *A new carried patch*).**

No upstream pull request or issue is opened or commented on for this. The
owner has put all upstream Envoy activity on hold, so aether1126 and the
aether1733 bug stay unreported, and envoyproxy/envoy#47740 and #47834 are left
alone.

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

### Running a dev snapshot in production

A `main` snapshot has no release branch. No upstream backport will ever target
`1.41.0-dev.20261028.011e396`, so while the proxy is on it, a fix reaches it in
one of three ways:

1. **A later snapshot.** The registry cuts them now and then, never on a
   schedule. Moving to one is another bump of this size: closure, `.bazelrc`
   re-diff, patch check, soak.
2. **The `1.41.0` release.** Envoy releases quarterly; 1.40.0 was tagged on
   2026-10-13, so 1.41.0 is expected around January 2027. **The pin moves to
   `1.41.0.envoy` as soon as the registry publishes it.**
   `modules/envoy/metadata.json` is checked on every dependency sweep, as
   `proxy/README.md` § *Envoy version bumps* already prescribes.
3. **A carried patch.** For a security fix that cannot wait for 1 or 2, the
   upstream commit, public once the advisory is out, is carried in
   `proxy/bazel/patches/` like any other. It is dropped at the next pin that
   contains it.

Staying on `1.40.0.envoy` would not be safer on this count. The registry has
not published `1.40.1` or `1.40.2` either, so a security patch release on the
1.40 branch would have reached the proxy only through 3 as well.

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
| `aether1733-validate-quic-null-socket` | not reported (a bug in envoyproxy/envoy#45504) | new | **add** (below) |

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

### A new carried patch: aether1733

The first `proxy` workflow run of the implementation pull request (run
41308410926, both arches) failed `//agent/test/envoy_validate`. `envoy --mode
validate` of every bootstrap with a static QUIC listener died with SIGSEGV in
`Quic::ActiveQuicListenerFactory::initializeWorkerRouting()`, called from
`ListenerManagerImpl::addOrUpdateListenerInternal()` under
`ValidationInstance::initialize()`. Three tests went down with it:
`TestEnvoyValidate/{node,edge}_bootstrap.json` and
`TestNodeProxyADSReconnectBackoffIsBounded`, which validates the node
bootstrap. The `FilterStateMatcherValidationError.Key` rejection in the same
log is a different matter: it is the deliberate negative case in
`capture_onehcm_test.go`, which passed.

- **Cause** (read at `011e396`, not reproduced locally): envoyproxy/envoy#45504
  moved the QUIC listener's socket options into `initializeWorkerRouting()`.
  That path sits behind `envoy.restart_features.defer_worker_routing_init`, on
  by default.
  - It applies the options to `factory->getListenSocket(i)` for every worker.
    With concurrency > 1 (validate mode defaults to the host's core count),
    those options include the BPF reuseport option.
  - Validation mode creates no sockets:
    `ValidationListenerComponentFactory::createListenSocket()` returns
    `nullptr` by design, so `setOption()` runs on a null socket.
  - Raw-UDP listeners take `ActiveRawUdpListenerConfig::initializeWorkerRouting()`,
    a no-op, which is why the UDP capture bootstrap passes.
  - Upstream `main` still has the bug as of 2026-10-30.
- **Production exposure** (inferred): the proxy-supervisor's check before
  every hot restart on a changed bootstrap is `envoy --mode validate`, at the
  chart's concurrency. Today's chart bootstraps carry static clusters only;
  listeners come over LDS, which validation does not fetch. So nothing crashes
  in production today. A future bootstrap with a static QUIC listener would
  fail that check, and with it every hot restart.
- **The fix:** the patch skips a null socket, which only validation mode
  produces, as validation did before #45504. The alternatives were set aside.
  Turning the guard off through a runtime layer would also disable #45504 in
  production. Validating with `--concurrency 1` would hide the crash, since
  production validates at the chart's concurrency.
- **The regression test** is `//agent/test/envoy_validate:envoy_validate_test`,
  which the `proxy` workflow runs against the binary it builds. It was red
  without the patch on both arches (run 41308410926, head `8ce601a3`). It must
  be green with it before merge. An Envoy-workspace unit test is not carried:
  none was built or run here.

**Upstream report, prepared and not posted.** The owner posts upstream. The
text below is for an issue on envoyproxy/envoy, or for the description of a
pull request carrying the same one-line guard:

> **`envoy --mode validate` segfaults on a bootstrap with a static QUIC
> listener since #45504**
>
> With `envoy.restart_features.defer_worker_routing_init` at its default
> (true) and `--concurrency` > 1, `envoy --mode validate` of a bootstrap that
> declares a QUIC listener in `static_resources` crashes with SIGSEGV
> ("suspect faulting address 0") in
> `Quic::ActiveQuicListenerFactory::initializeWorkerRouting()`, called from
> `ListenerManagerImpl::initializeWorkerRoutingForUdpListener()`,
> `addOrUpdateListenerInternal()` and `ValidationInstance::initialize()`.
>
> `initializeWorkerRouting()` applies `options_` (which, with concurrency > 1,
> holds the BPF reuseport option) to `factory->getListenSocket(i)` for each
> worker. Under validation, `ValidationListenerComponentFactory::createListenSocket()`
> returns `nullptr` by design, so `Network::Socket::applyOptions()` operates
> on a null socket. Before #45504 the options were applied through
> `socketOptions()` at socket creation, which validation never reaches.
> Raw-UDP listeners are unaffected (`ActiveRawUdpListenerConfig::initializeWorkerRouting()`
> is a no-op).
>
> Reproduce: a bootstrap with one static listener whose
> `udp_listener_config.quic_options` is set and a QUIC downstream transport
> socket, run as `envoy --mode validate --concurrency 2 -c <file>`. Seen at
> `011e39616b` and in `main` as of 2026-10-30.
>
> Suggested fix: skip a null socket in that loop. Only validation mode
> produces one. A validation-server test with a static QUIC listener at
> concurrency 2 covers it.

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
  `261006-9e60ac2` to `261019-67bce2e`, in two upstream bumps:
  envoyproxy/envoy#48288 (to `132bfd9`) and #48484 (to `67bce2e`, the
  revision that ships).
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
- **Carried patches:** two files deleted, two refreshed, one added
  (aether1733). The comments in
  `//bazel/patches:carried_patch_tests` say what each remaining target covers.
  `udp_listener_impl_test` stays: it carries aether1126's case, and now runs
  #47868's upstream case too.
- **Docs:** `proxy/README.md` (the carried-patch table, the pin, the Bazel
  version), `README.md`, `Makefile` and `docs/runbook.md` (the Bazel version
  of the proxy workspace).

Nothing outside `proxy/` and the docs changes. `go-control-plane/envoy` stays
`v1.40.0`. `scripts/check-envoy-api-parity.sh` accepts bindings one minor
behind a `-dev` pin, which this is. No chart change: after the merge,
`proxy-release.yml` publishes the image and, in its auto mode, opens the chart
pin pull request and arms its auto-merge, as for every proxy change (see
*Release and soak*).

## Risk: upstream changes that touch what aether depends on

Read from `git log v1.40.0..011e396` over the hot-restart, UDP, QUIC, HTTP
connection manager, TLS, listener and xDS sources. Inferred: none of these
was exercised against aether's configuration before the soak.

| Change | What it touches in aether | Expected effect | Watched by |
|---|---|---|---|
| envoyproxy/envoy#45504, "QUIC eBPF hot restart part 0" | QUIC listener setup: the connection-ID generator factory and UDP socket options move to `initializeWorkerRouting()`, and the listener's init target registers after it. Behind `envoy.restart_features.defer_worker_routing_init`, default **on**. Touches no hot-restart IPC file. | One defect already found: with concurrency > 1, `envoy --mode validate` segfaults on any bootstrap with a static QUIC listener (*A new carried patch: aether1733*), fixed by aether1733. Otherwise no behaviour change is intended, but the path is the one aether1050 and aether1126 sit next to (QUIC listeners on a hot-restart child, CID → worker routing). | `//agent/test/envoy_validate:envoy_validate_test` in the `proxy` workflow (the validate crash); the soak (step 5), including its upgrade and rollback rolls: proxy rolls under QUIC load, stateless resets, misses near handoffs |
| envoyproxy/envoy#47068 | the hot-restart receive loop | composes with aether1050 (see above) | `carried_patch_tests`; soak rolls |
| envoyproxy/envoy#48570, upstream TLS default max 1.3 | every TCP mesh hop: aether sets only `tls_minimum_protocol_version` (TLSv1_2), so mesh upstreams now offer and negotiate TLS 1.3. QUIC was 1.3 already. Behind `envoy.reloadable_features.default_client_tls_max_version_is_1_3`. | A wire-visible change. Resumption stays off (`max_session_keys: 0`). The per-source certificate selector is exercised under 1.3 for the first time. Per the release note, a client certificate the server rejects becomes a reset *after* connect: `connect-failure` retries and `tcp_proxy` `max_connect_attempts` no longer move it to another endpoint, connect-only TCP health checks pass (the readiness probe, below), and outlier detection counts it as a success. The default key exchange becomes post-quantum, so ClientHellos get larger. | the TLS-1.3-compatible readiness probe below (#1735, merged before #1733); the TCP-routed soak check in *Validation*; the guard is the rollback lever (see *Rollback*) |
| envoyproxy/envoy#47067, `wait_for_warm_on_init` for all cluster types | CDS warming: one CDS pause handle per warming cluster instead of one global handle | aether sets no `wait_for_warm_on_init` (default true): each warming cluster still holds CDS back. ACK timing per cluster may shift. | soak: agent ACK/NACK metrics, the `503 NC` count at twin admission |
| envoyproxy/envoy#48024, ODCDS remembers "missing" | QUIC twins over ODCDS | behind `envoy.reloadable_features.odcds_missing_cluster_cache`, **default off** at `011e396`: no change unless turned on | nothing to watch; re-check when upstream flips it |
| envoyproxy/envoy#48214, `prefer_gro` honoured | the inbound QUIC listener | aether sets `prefer_gro: true` (`agent/internal/xds/proxy/ingress.go`), so GRO stays on; the fix only stops Envoy turning GRO on where `prefer_gro` is false | soak: QUIC per-request CPU (the #1021 gate) |
| envoyproxy/envoy#48403 (listener addresses updated in place), #48413 and #47455 (use-after-free fixes in connection-handler and accepted-socket teardown) | listener updates, drains | fixes; no aether configuration change needed | soak |
| envoyproxy/envoy#48288 and #48484, QUICHE `9e60ac2` → `132bfd9` → `67bce2e` (ships `0.0.0-261019-67bce2e.envoy`) | every HTTP/3 hop | dependency bumps | soak |
| Hot restart **across** the two versions | every node's upgrade roll is a 1.40.0 parent → 1.41-dev child handoff, and a rollback is the reverse | compatible, by the evidence below | the soak's upgrade and rollback rolls (*Release and soak*) |

**Hot restart across 1.40.0 and 1.41-dev.** A surge roll starts the new proxy
pod's Envoy as a hot-restart child of the old pod's: same host network
namespace, same `/dev/shm`, same base id. So every node's upgrade is a 1.40.0 →
1.41-dev handoff, and a revert is the reverse handoff on every node. The soak
alone would not show it, since it rolls 1.41 to 1.41. The evidence that the
two versions interoperate (read):

- **The segment and its version:** `HOT_RESTART_VERSION` is 11 at both
  `v1.40.0` and `011e396`. That is also the value the supervisor's
  stale-flag repair is tied to (`envoyHotRestartVersion` in
  `agent/internal/proxy/hotrestart/sharedmemory.go`).
- **The IPC sources:** `git diff v1.40.0 011e396` changes nothing in
  `source/server/hot_restart.proto`, `hot_restart_impl.{h,cc}`, or
  `hot_restarting_{child,parent}.cc`.
- **The receive loop:** the only change in `hot_restarting_base.{h,cc}` is
  #47068. It drops a length prefix that no legitimate peer sends, so a 1.40.0
  peer, which lacks it, sends and receives the same messages.
- **aether's IPC patches:** aether1050 and aether1126 carry identical
  `+`/`-` lines in both builds. aether1733 touches validation only.

What this does not cover is behaviour: #45504's deferred option setup on a
child that inherits a 1.40.0 parent's QUIC sockets. Hence the validation step.

Runtime guards aether relies on, all at their defaults:
`hot_restart_propagate_stat_tags` (the chart's stats configuration assumes it
is on; the chart sets no runtime) still exists at `011e396`, and 47740's own
guard comes with the patch. None of the guards upstream removed in this range
is one aether relies on.

**TLS 1.3 and the inbound readiness probe.** The upstream release note for
#48570 says that under TLS 1.3 a server's rejection of the client certificate
reaches the client only after the client considers the connection
established. `NewInboundReadyProbeCluster` (`agent/internal/xds/proxy/cluster.go`)
gates an endpoint's promotion on a connect-only TCP health check over TLS, which
passes on the Connected event. Under TLS 1.2 that event implied the pod's
inbound had also accepted the probe's client certificate (the node SVID).
Under TLS 1.3 it no longer does, so an endpoint could be promoted while its
inbound rejects the node identity. The probe's documented contract
(listening, presents a certificate that verifies, URI SAN is this pod's) still
holds. The implicit client-acceptance half does not.

The fix, a requirement the owner set on 2026-11-01, is a TLS-1.3-compatible
probe in its own agent pull request, #1735, merged **before** #1733. There is
no TLS 1.2 pin.
- **What changed:** the probe is an HTTP/2 GET of `/-/-/live` that the
  inbound answers itself. Its liveness `health_check` filter is the first HTTP
  filter of the no-SNI h2 chain the probe's ALPN selects, with no pass-through
  and no cluster condition, so the application never takes part.
- **Why that works:** a pass needs a response the inbound sends after it has
  accepted the connection. A rejection arrives as a reset and fails the check
  on TLS 1.2 and 1.3 alike.
- **What stayed:** the probe's own transport socket, a fresh handshake per
  check and the 5 s cadence.
- **The proof** is a live test against real Envoy,
  `TestInboundReadyProbe_RequiresTheInboundToAcceptTheNodeIdentity`. Under TLS
  1.3, with an inbound that rejects the node SVID, it was red on the
  connect-only probe and is green on the new one. `validate-built-proxy.sh`
  also runs it against the proxy a proxy pull request builds, so #1733's CI
  exercises it on the 1.41 snapshot.

The prober's traffic does not exercise a certificate rejection, so it cannot
stand in for this.

## Validation

Before merge:

1. **Local**, measured on the implementation:
   - `bazel mod graph --depth=1` resolves with no warning.
   - `bazel mod show_repo --base_module=envoy @envoy_build_config` is our
     `local_repository`.
   - `bazel build --nobuild //...` analyses all 30 targets, which fetches
     `@envoy` with every carried patch applied.
   - `scripts/check-envoy-pin.sh` and `--online` pass.
   - `make gazelle`, `make tidy`, `make format-check` and lint as CI runs it
     pass.
   - `make test-unit` cannot run cleanly in a sandbox without Unix sockets,
     which is where these checks ran. The `ci` workflow runs it; the change
     touches no Go code.
2. **The `proxy` workflow** on the implementation pull request, both arches on
   RBE:
   - the build;
   - `//...`, which runs `//bazel/patches:carried_patch_tests` (aether1050's
     four hot-restart targets, aether1054's `conn_manager_impl_test`, and
     aether1126's and #47868's `udp_listener_impl_test`);
   - `image_test`, `symtab_test` and `build_id_test`;
   - the `--build-id=sha1` link-line check;
   - `envoy_validate` of the agent's generated configuration against the
     binary this pull request builds. This is also aether1733's regression
     test.
3. **The tests that build only in the Envoy workspace:** aether1054's
   `quic_http_integration_test` case, and the http3 `conn_pool_test` that came
   with #47893. They run from an Envoy checkout at `011e396` with the patches
   applied, on BuildBuddy RBE. The implementation pull request says whether
   they were run, and with what result.

### Release and soak

The owner's decision of 2026-11-01: the soak runs only after the full bump,
not in between. There is no release hold and no pre-soak stage. On the merge
of #1733, `proxy-release.yml` runs as for every proxy change:

- it builds, signs and pushes the image by digest;
- with `RELEASE_PAT` present (its *auto* mode), it opens the
  `chore/proxy-image-bump-<sha>` chart pin pull request, arms
  `gh pr merge --auto --squash` on it, and closes older pin pull requests as
  superseded;
- the pin merges as soon as its checks pass.

The soak then validates the deployed full bump on the reference cluster.
Before the deploy:

4. **The TLS-1.3-compatible readiness probe** (*Risk*, TLS 1.3; #1735). The
   order is strict:
   1. #1735 merges;
   2. its agent image is deployed on the reference cluster, and the agent
      rollout completes: **every node's agent runs the #1735 image**;
   3. only then does #1733 merge.

   "#1735 merged first" alone is not enough. Once #1733 is on `main`, a single
   `helm upgrade` from `main` would roll the agent and the proxy together, so
   some node's proxy could speak TLS 1.3 by default while its agent still
   programs the connect-only probe. With this order, every node's probe is
   the new one before any node's proxy runs 1.41-dev.

Then:

5. **The soak, on the deployed full bump**: the external soak harness
   (proposal [042](042_sortie-soak-driver.md): sortie load, rollout churn,
   the external prober), 8 h. `AGENTS.md` does not require one for an Envoy
   bump. Every hot-restart and QUIC change before this one was soaked, and
   this one carries #45504, the TLS 1.3 default and a new toolchain runtime.
   The cross-version hot restart is folded into it rather than run as an
   earlier stage. The deploy roll *is* the 1.40.0 → 1.41-dev handoff on every
   node, so it is measured where it happens.

   **Order of the rolls.** None of these can be scoped to one node: the chart
   has no node-scoped proxy canary. A Helm image change rolls the whole
   DaemonSet (maxSurge 1, maxUnavailable 0) and the edge Deployment when it
   is enabled, DaemonSets have no partition, and `OnDelete` is not a
   hot-restart handoff. The rolls, in order:
   1. **Same-version baseline, actually run:** a roll of the 1.40.0 proxy onto
      the same image (a pod-template annotation change), with the same load
      and the measurements below. Its per-handoff numbers are the bar.
   2. **Upgrade:** the deploy, 1.40.0 → 1.41-dev, measured per handoff.
   3. **The 8 h soak proper** on 1.41-dev, with its usual 1.41 → 1.41 churn.
   4. **Rollback and forward:** 1.41-dev → 1.40.0 → 1.41-dev at the end, so
      the reverse handoff that a revert would cause on every node is
      measured too. The reference cluster's owner accepts these rolls as part
      of the soak.

   **Traffic through every inherited listener.** The prober alone does not
   prove that inherited QUIC listeners carried traffic through a handoff: its
   liveness check is a local reply, and other probes may reach another node
   or use TCP. The soak's load therefore includes:
   - for every node, a QUIC load target of its own: a service with a single
     endpoint, pinned to that node, and a source on another node. A twin
     load-balances over all of a service's endpoints and keeps its connections
     pooled, so one service with a pod on every node would not guarantee each
     node's listener any traffic; a single-endpoint service per node does;
   - before each roll, a check that each target's requests reach its pod over
     HTTP/3. The inbound HCM request counters cannot show it: the h2 and QUIC
     chains share the `inbound` stat prefix. Two signals that are
     QUIC-specific show it instead:
     - on the source, the twin's `upstream_rq_total` rises with the load. A
       twin is a QUIC-only cluster (`<ns>/<svc>@<ns>/<sa>`), so its requests
       are HTTP/3 by construction;
     - on the destination node, the pod's own QUIC listener holds
       `downstream_cx_active` > 0. Its stat prefix is `inbound_<ns>_<pod>_h3`,
       which the proxy's stats tags split, so in Prometheus it is
       `envoy_listener_inbound_downstream_cx_active{aether_namespace="<ns>", aether_pod="<pod>_h3"}`.

     Then HTTP/3 connections stay established, with requests flowing
     throughout the roll;
   - at least one load target routed over raw TCP, the TCP floor
     (`tcp_<ns>/<svc>`), for the TCP mTLS hop. Its client opens a **new
     connection per request**. The source's `tcp_proxy` opens one upstream
     connection per downstream connection, so every request is a fresh mTLS
     handshake with the source's own identity, at a steady rate, through
     every window.

   **Measurements.** Envoy counters are per generation: a hot-restart child
   inherits only the parent's unflushed delta, so a raw value can fall during
   a healthy handoff. Every counter below is therefore read as a reset-aware
   increase or rate over the exported series (Prometheus `increase()` /
   `rate()`), never as a raw value.

   **A series born inside the window.** `increase()` alone cannot see it.
   - The proxy's OTel stats sink exports only counters that have been used,
     with cumulative temporality.
   - A failure counter therefore appears in Prometheus at the first export
     after its first failure, already carrying it. That first sample is
     `increase()`'s baseline, so the burst reads 0.
   - Per-pod series make this the common case, not the corner. Every pod
     starts its roll with no failure series at all, and a first-ever
     `TLSv1.2` handshake would hide the same way.

   The rule used here keeps cumulative temporality and counts a series born
   in the window at its full value:
   - a series that existed at the window's start contributes its
     `increase()`;
   - a series that did not contributes `max_over_time()`, its whole
     cumulative count. It started from zero in its own process, apart from
     the small unflushed delta a hot-restart child inherits.

   In PromQL, for any counter `X`:

   `(increase(X[w]) and X offset w) or (max_over_time(X[w]) unless X offset w)`

   The alternative, delta temporality on the sink with `sum_over_time()`, was
   set aside. It changes the export for every consumer of these series, not
   only the soak.

   A QUIC twin's stats are keyed by its `alt_stat_name`, which
   `QUICAltStatName` (`agent/internal/xds/proxy/egress.go`) sets to
   `<ns>/<svc>@<ns>/<sa>`. So the twin's series is
   `cluster.<ns>/<svc>@<ns>/<sa>.*`, in Prometheus
   `aether_cluster=~"<ns>/<svc>@.*"`. Nothing is keyed `quic:…`.

   **Per node, not per service.** The documented metrics pipeline can leave the
   per-service cluster series collapsed across nodes
   (`docs/observability/metric-labels.md`, the `node` label on the node proxy's
   per-cluster stats). Collapsed, another node's source proxy could keep a
   twin's rate up during a failed handoff, and interleaved per-generation
   counters can look like resets. A `node` label alone is not enough either.
   During a handoff the hot-restart parent and the surge child run on the same
   node and export the same label set, so with `node` only, their two counters
   interleave in one series. `increase()` and `rate()` then read the switches
   between them as resets inside the window, and the TLS checks mix the
   parent's version with the child's.

   The soak's pipeline therefore carries, on these series, both:
   - `node` (from `k8s.node.name`);
   - the proxy pod, `k8s.pod.name` (or `k8s.pod.uid`). The node proxy sets
     both as resource attributes on its Envoy stats
     (`docs/observability/metric-labels.md`), but the reference cluster's
     pipeline does not promote either to a label today (measured: the node
     proxy's cluster series carry `node` and no pod label). Promoting one is
     a precondition of the soak. The parent and the surge child are different
     pods.

   Every rate and increase below is taken **per pod first**, so each
   generation's counter stays monotonic within its own series, **then summed
   by `node`**: the source proxy's node for twin and TCP cluster stats, and
   the destination's node for inbound stats. For example:
   `sum by (node) (increase(<counter>{…}[<window>]))`, where `increase()` runs
   on each pod's series before the sum.

   **The queries.** The PromQL below uses the series names the node proxy
   exports today. Counters carry `_total`, cluster stats are labelled
   `aether_cluster`, listener stats `aether_namespace` / `aether_pod`, and
   the node proxy's series have `job="aether-proxy"`. Every expression was
   run against the reference cluster's Prometheus over a recent window,
   grouped by the labels that exist there today. Each parsed and returned
   one row per node.

   A failure or version counter is read with the born-in-window rule, applied
   **to one counter at a time**: `increase()` and `max_over_time()` drop
   `__name__`, so one `{__name__=~…}` selector over several counters that
   share every other label is rejected ("vector cannot contain metrics with
   the same labelset"; measured). Per counter `C`, with the target's selector
   `S` and the window `w`, the per-node term is:

   `T(C) = sum by (node) ((increase(C{S}[w]) and C{S} offset w) or (max_over_time(C{S}[w]) unless C{S} offset w)) or on(node) (0 * group by (node) (A{S}))`

   `A` is a series every node that carries the load has, the anchor. The
   `or on(node)` gives such a node a zero row when `C` has no series there
   yet; a bare `or vector(0)` carries no labels and cannot. The terms of
   different counters are then added with `+`, which matches on `node`
   alone.

   For each node's handoff window, in each roll:
   - **Forward progress:** two counters keep a non-zero rate through the
     window, and both are QUIC-specific:
     - on the destination, the target pod's QUIC listener's
       `sum by (node) (rate(envoy_listener_inbound_http_inbound_downstream_rq_completed_total{job="aether-proxy", aether_namespace="<ns>", aether_pod="<pod>_h3"}[1m]))`;
     - on the source, the twin's
       `sum by (node) (rate(envoy_cluster_upstream_rq_total{job="aether-proxy", aether_cluster=~"<ns>/<svc>@.*"}[1m]))`.
   - **Request failures:** on the source twin (`S` =
     `job="aether-proxy", aether_cluster=~"<ns>/<svc>@.*"`, anchor
     `A` = `envoy_cluster_upstream_rq_total`), the sum
     `T(envoy_cluster_upstream_rq_5xx_total) + T(envoy_cluster_upstream_rq_rx_reset_total) + T(envoy_cluster_upstream_rq_tx_reset_total) + T(envoy_cluster_upstream_cx_destroy_remote_with_active_rq_total)`,
     plus the load generator's own error count, is no greater than the
     baseline roll's worst handoff.
     - Envoy has no `upstream_rq_reset`. Its reset counters are
       `upstream_rq_rx_reset` and `upstream_rq_tx_reset`.
     - `upstream_rq_rx_reset_no_error` (a peer ending a stream with NO_ERROR)
       is not a failure and is left out. It exists at both pins:
       `envoy/upstream/upstream.h` at `v1.40.0` declares it beside
       `upstream_rq_rx_reset`, and nothing in `v1.40.0..011e396` touches it.
       So the 1.40.0 baseline and 1.41-dev are graded with the same split.
     - Counters are created lazily. Today `envoy_cluster_upstream_rq_rx_reset_total`
       has no series at all, and its term is still a zero row per node.
     - A failure series born inside the window (every failure series of a
       pod that has just rolled) counts at its full value, not as 0.
   - **Prober:** misses in the window are no greater than the baseline roll's
     worst handoff, and the soak's per-target gate (proposal 042) holds.
   - **Connections, as diagnostics rather than the pass criterion:** the
     inbound QUIC listener's `downstream_cx_total` counts only new sessions
     (incremented when `EnvoyQuicDispatcher` creates one). A pooled HTTP/3
     connection that survives the handoff leaves it flat, and a dropped one
     that reconnects makes it rise. A rise above the baseline's, together
     with the stateless-reset count, points at connections lost in the
     handoff. Its live count is
     `envoy_listener_inbound_downstream_cx_active{job="aether-proxy", aether_namespace="<ns>", aether_pod="<pod>_h3"}`.
   - **Process:** the child reaches ready, the parent exits on its own, and
     the supervisor's restart counters show no stale-flag repair and no
     validation rejection.
   - **TLS version on the TCP path, by source-proxy version.** The stat key
     depends on the target. An HTTP service reached over h2 is keyed by its
     `alt_stat_name`, `<ns>/<svc>`. A raw-TCP floor or per-port cluster is
     keyed `tcp_<ns>/<svc>` or `tcp_<ns>/<svc>_<port>` (`TCPStatKey` /
     `TCPPortStatKey` in `agent/internal/xds/proxy/egress.go`). Nothing is
     keyed `cluster.<svc>`.

     `ssl.versions.*` counts handshakes, not requests. Pooled h2 upstream
     connections carry load without new handshakes, and a child opens its
     pools during the parent's drain, so an h2 target would see few or no
     handshakes after the parent exits. The TLS checks therefore read two
     sources that handshake on their own schedule:
     - **the raw-TCP load target**, with one new connection per request, so
       one source-identity handshake per request;
     - **the inbound-readiness probe clusters** (`inboundready_<ns>_<pod>`).
       They open a fresh connection to every local pod every 5 s, so they
       give a steady handshake rate per node under the node proxy's default
       client TLS settings.

     With `S` = `job="aether-proxy", aether_cluster=~"inboundready_.*|tcp_<ns>/<svc>"`
     and anchor `A` = `envoy_cluster_ssl_handshake_total` (the anchor also
     makes a missing series read 0, never pass vacuously), per source node:
     - a 1.41-dev source proxy: `T(envoy_cluster_ssl_versions_TLSv1_3_total) > 0`
       and `T(envoy_cluster_ssl_versions_TLSv1_2_total) == 0`, after the
       upgrade and after the final forward roll;
     - a 1.40.0 source proxy: `T(envoy_cluster_ssl_versions_TLSv1_2_total) > 0`
       and `T(envoy_cluster_ssl_versions_TLSv1_3_total) == 0`, in the
       baseline and after the rollback. 1.40.0 offers TLS 1.2 at most by
       default, so expecting TLS 1.3 there would fail a healthy run.

     **When a TLS window starts.** Until its parent exits, the child keeps
     merging the parent's latched stat deltas into its own counters on every
     flush (the parent's admin shutdown resets its flush timer). A 1.40.0
     parent's TLS 1.2 probe handshakes would therefore create
     `ssl.versions.TLSv1.2` in the 1.41-dev child, the born-in-window rule
     would count it at full value, and "TLS 1.3 only" would fail a healthy
     upgrade. The rollback has the mirror problem.

     So the window starts **one export interval after the parent exits**,
     rounded up to two. That interval comes from the real pipeline:
     - the node proxy's stats are not scraped. Its OTel sink pushes them over
       OTLP on every stats flush, every 5 s (the chart leaves
       `stats_flush_interval` at Envoy's default);
     - each sample is stamped at its export. Measured: 60 samples per series
       in 5 minutes, and no `up` series for `job="aether-proxy"`, which a
       scrape would create;
     - so the last merge of the parent's deltas lands in a sample stamped no
       later than one flush after the parent exits, and ingestion delay does
       not move that timestamp.

     The window therefore starts 10 s (two flushes) after the parent exits.
     By then the child's version series already exist, so `increase()`
     counts only the child's own handshakes. The window lasts a few minutes.

     The source identity comes from a different source than the TLS version,
     because neither TLS source logs it at the destination: the raw-TCP floor
     chain's `tcp_proxy` writes no access log, and health-check requests are
     dropped from the inbound access log (`notHealthCheckFilter`). It comes
     from the h2 target instead. After the parent exits, its requests ride
     connections the child opened, with the child's TLS settings and its
     per-source certificate selector. The destination inbound's HTTP access
     log for those requests shows the expected source SPIFFE ID in
     `downstream_peer_uri_san`, under each version. The TLS version of those
     connections is read from the version sources above, not from this log.

   The upgrade and the rollback must meet the same bar as the baseline. A
   failure in either is a soak FAIL, and the rollback below applies. The
   owner starts the soak.

What the owner accepted with this order: the first 1.40.0 → 1.41-dev handoffs
happen at the deploy itself, on the reference cluster, with the auto-merged
pin already on `main`. A broken handoff would show there first, and its
remedy, the rollback, is the reverse handoff, measured only at the end of the
soak.

## Rollback

Two ways back to 1.40.0, both ending in the reverse handoff on every node
(1.41-dev → 1.40.0), which the soak's rollback roll measures:

- **Revert the pin.** Revert the auto-merged `chore/proxy-image-bump-<sha>`
  pull request. The chart points at the previous proxy image again, and the
  next deploy rolls back to it. This is the fast path: nothing is rebuilt.
- **Revert the implementation pull request.** That restores `1.40.0.envoy`,
  Bazel 8.7.0 and the six patches. `proxy-release.yml` then builds the
  previous proxy again, and its pin follows in auto mode.

To keep the bump and back out only a behaviour, two runtime guards can be set
to false without rebuilding Envoy:

- `default_client_tls_max_version_is_1_3` (TLS 1.3). Envoy reads it only when
  it builds an upstream TLS context configuration.
- `defer_worker_routing_init` (#45504), a restart feature. Envoy reads it only
  when it builds a listener.

Neither changes what already exists, and the proxy bootstrap
(`charts/aether/templates/agent-proxy-configmap.yaml`) has no
`layered_runtime` today. The lever that works is therefore a chart change
that adds a static runtime layer with the guard set to false, followed by a
proxy roll. That is a hot restart into a process that builds every TLS
context and listener with the override from the start. The admin endpoint's
`/runtime_modify` is not a rollback:

- it needs an admin layer the bootstrap does not have;
- it affects only clusters and listeners built afterwards;
- its effect is lost at the next hot restart.

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
