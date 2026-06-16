# Proposal: aether_stats as a native C++ Envoy extension

**Status:** Design — 2026-06-15
**Supersedes:** proposal 011 (the Rust dynamic-module port) for the chosen direction
**Follows:** proposal 007 (telemetry filter), proposal 010 (custom proxy workspace)

## Why

`aether_stats` is a Rust **dynamic module** (proposal 007). Dynamic modules talk
to Envoy only through the `abi.h` C ABI, which forces three compromises we keep
hitting:

1. **Response flags** aren't exposed to the dynamic-module HTTP-filter hook, so
   the module derives them from `on_local_reply` details and hand-maps them to
   `UF`/`UH`/… (`classify_flag`).
2. **Stats naming** is locked to the dynamic-modules scope
   (`dynamicmodulescustom.aether_requests_total`).
3. The ABI is a moving, limited surface.

Now that we **build a custom Envoy from source** (proposal 010), the filter can be
a **first-class C++ HTTP filter** with direct `StreamInfo` access — eliminating
all three. The logic is small (~120 lines of Rust) and ports cleanly.

## What the C++ extension gets natively (verified at v1.38.0)

| Need | Dynamic module (today) | C++ extension |
|---|---|---|
| response flags | `on_local_reply` details + `classify_flag` workaround | `ResponseFlagUtils::toShortString(stream_info)` — the **same** `UF/UH/NC/…` vocabulary, built in |
| response code | `get_attribute_int(ResponseCode)` | `stream_info.responseCode()` |
| dest cluster | `get_cluster_name()` | `stream_info.upstreamClusterInfo()->name()` |
| peer URI SAN | `get_attribute_string(ConnectionUriSanPeerCertificate)` | `ssl()->uriSanPeerCertificate()` |
| stat name | `dynamicmodulescustom.*` (scoped) | root scope, e.g. `aether.requests_total` |

So `classify_flag`, `spiffe_service`, `dest_service_from_cluster` carry over
(trivial string logic); the flag-from-local-reply workaround is **deleted** in
favor of `toShortString`.

## Design

### Layout (mirrors Envoy's filter extensions)

```
proxy/source/extensions/filters/http/aether_stats/
  aether_stats.proto          # filter config message
  filter.h / filter.cc        # Http::PassThroughFilter; records at onStreamComplete
  config.h / config.cc        # FactoryBase<Config>; REGISTER_FACTORY
  BUILD                       # envoy_cc_library + envoy_cc_extension + proto
```

### Config proto

```proto
package aether.filters.http.aether_stats.v3;

message Config {
  string reporter = 1;             // "source" (outbound) | "destination" (inbound)
  string source_service = 2;
  string source_pod = 3;
  string destination_service = 4;  // inbound: local identity
  string mesh_domain = 5;          // default "aether.internal"
  bool emit_pod = 6;
}
```

The agent sends it as a `TypedStruct` (the same field set it already builds as
JSON), so **no Go proto binding** is needed — Envoy converts the `Struct` to
`Config` in the factory. (A real Go binding can come later if desired.)

### Filter (`Http::PassThroughFilter`)

- `decodeHeaders` (source/outbound only): capture the routed cluster — actually
  read it at completion from `streamInfo().upstreamClusterInfo()`, so no per-hook
  state is needed.
- `onStreamComplete()`: build the label tuple and increment the counter. Records
  once for every request, including local replies (matches the Rust
  `on_stream_complete`).
- Source/destination split is unchanged: outbound takes source from config +
  destination from the cluster name; inbound takes destination from config +
  source from the peer URI SAN (`spiffe_service`).

### Native tagged stat

Define one counter in the filter-config's scope with native Envoy tags (symbol
table), so the existing OTel sink (`emit_tags_as_attributes` +
`use_tag_extracted_name`) exports them as OTLP attributes exactly as the
`counter_vec` did — but under a clean name:

```
aether.requests_total{reporter, source_service, source_pod,
                      destination_service, response_code, response_flags}
```

```cpp
Stats::StatNameTagVector tags{{reporter_, reporter_val}, /* … */};
config_->scope_.counterFromStatNameWithTags(config_->requests_total_, tags).inc();
```

### Registration + linking

`REGISTER_FACTORY(Factory, NamedHttpFilterConfigFactory)` with name
`envoy.filters.http.aether_stats`. Link into the custom Envoy via
`AETHER_EXTENSIONS` in `proxy/BUILD.bazel`:

```starlark
AETHER_EXTENSIONS = ["//source/extensions/filters/http/aether_stats:config"]
envoy_cc_binary(name = "envoy", deps = AETHER_EXTENSIONS + ["@envoy//source/exe:envoy_main_entry_lib"])
```

## Agent change (control plane)

The agent stops emitting the `dynamic_modules` HTTP filter and instead emits an
`envoy.filters.http.aether_stats` filter with a `TypedStruct` `typed_config`
carrying the same fields (reporter/source_service/source_pod/… per the
inbound/outbound side). Filter placement, per-pod identity injection, and the
source/destination reporter split are unchanged — only the filter name +
config encoding change. The `DynamicModuleConfig` wiring is removed.

## Removed

- `proxy/filters/http/aether_stats/` (the entire Rust dynamic module: `src/`,
  `Cargo.*`, `patches/`).
- Any dynamic-module image plumbing (`/modules`, `ENVOY_DYNAMIC_MODULES_SEARCH_PATH`)
  — never added since the module was parked.
- Agent `DynamicModuleConfig` generation for stats.

## Risks & validation

- **arm64.** The extension compiles into Envoy, so it shares the **currently
  broken** arm64 build (#189 LuaJIT `ld`/`lld`). Fix that first (or single-arch).
- **Tag → OTLP attribute mapping.** Confirm `counterFromStatNameWithTags` + the
  OTel sink reproduce the attributes the `counter_vec` produced (the metric name
  changes from `dynamicmodulescustom.aether_requests_total` to
  `aether.requests_total` — update dashboards/queries).
- **Unit tests** become `envoy_cc_test` with a mock `StreamInfo` (replacing the
  Rust `MockEnvoyHttpFilter` tests) — port the label-derivation cases.
- **Envoy-bump coupling.** A compiled-in C++ extension recompiles/adapts on Envoy
  API changes (vs the ABI-stable dynamic module). Acceptable — we build Envoy.

## Rollout (PR stack)

1. **Proxy**: the C++ extension (proto + filter + factory + BUILD) linked into
   `//:envoy`; `proxy-pr` builds it (gated on the arm64 fix). Includes the
   `envoy_cc_test`.
2. **Agent**: switch the generated filter from `dynamic_modules` to
   `aether.filters.http.aether_stats`; delete the Rust module + its wiring.
3. **Dashboards**: rename `dynamicmodulescustom.aether_requests_total` →
   `aether.requests_total`.

The net result: full `StreamInfo` access, real response flags with no workaround,
native stat naming, and a single self-contained Envoy binary.
```
